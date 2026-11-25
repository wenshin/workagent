package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
)

// ArgsHash 返回「工具 + 规范化参数」的指纹。
//
// 续跑时模型重新生成的 tool_call id 是新的，参数通常一模一样；把参数规范化
// （解码再编码，map key 自动排序）之后取哈希，才能认出「这就是刚才那次调用」。
// 用途有两个：复用已批/已拒的结论，以及回放已经产生的结果。
func ArgsHash(toolName string, args json.RawMessage) string {
	norm := normalizeArgs(args)
	sum := sha256.Sum256([]byte(toolName + "\x00" + norm))
	return hex.EncodeToString(sum[:16])
}

func normalizeArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		// 不是合法 JSON 时按去空白处理：至少让同样的原文产生同样的指纹
		return strings.Join(strings.Fields(string(args)), "")
	}
	b, err := json.Marshal(v) // map 的 key 按字典序输出，天然稳定
	if err != nil {
		return strings.Join(strings.Fields(string(args)), "")
	}
	return string(b)
}

// 插件的审批级别：安装方在插件卡片上定，作用范围是这个插件的全部工具调用。
//
// 默认档（declared）走「按声明判断」——脚本看出网 verbs、MCP 看 tools.writes、
// shell 看命令名单。另两档是给安装方的两个方向：宁可多点几次，或这个插件已经
// 被远端鉴权兜住了、不想每次打扰。判断口径本身不参与审批的设计没变：等级仍然
// 只用于展示，档位是安装方的显式选择，不是拿风险等级反推出来的。
const (
	// ApprovalDeclared 按清单声明与命令名单判断（默认）。
	ApprovalDeclared = "declared"
	// ApprovalAlways 每次调用都确认。
	ApprovalAlways = "always"
	// ApprovalOff 不确认，直接执行。
	ApprovalOff = "off"
)

// ApprovalPolicyKey 是安装配置里审批档位的键。
//
// 定义在 tool 而不是 plugin：内置工具的档位由这里的 builtinPolicy 读，plugin 侧
// 只负责写配置。两边共用一个常量，避免键名漂移后「配了但读不到」。
const ApprovalPolicyKey = "approval_policy"

// NormalizeApprovalLevel 收敛档位取值：不认的值一律按默认档处理。
//
// 这样旧安装记录（没有这个键）与拼错的值都退化成「按声明判断」，不会因为一个
// 字符串把某个插件的确认悄悄关掉。
func NormalizeApprovalLevel(level string) string {
	switch strings.TrimSpace(level) {
	case ApprovalAlways, ApprovalOff:
		return strings.TrimSpace(level)
	default:
		return ApprovalDeclared
	}
}

// DefaultApprovalCommands 是 shell_exec 默认需要确认的命令。
//
// 名单刻意很短：沙箱已经把「跑出去」和「删系统盘」挡住（全盘只读、只有当前空间
// files/ 可写、默认无网、超时与进程组回收），这里只兜住不可逆地改用户数据的两类：
// 删除/覆盖、改权限。解释器（python / node / make）不列——危险动作最终仍会落在上面。
// 名单是默认值，空间管理员可以在插件卡片里增删。
var DefaultApprovalCommands = []string{
	"rm", "rmdir", "dd", "truncate", "shred",
	"chmod", "chown",
	"sudo", "su", "mount",
}

// demo 之外的组合形态：单个命令名判不出来，必须看整条命令。
var shellCombos = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`\bfind\b[^;&|\n]*-delete\b`), "find -delete"},
	{regexp.MustCompile(`\bfind\b[^;&|\n]*-exec\s+rm\b`), "find -exec rm"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard"},
	{regexp.MustCompile(`\bgit\s+clean\b`), "git clean"},
	{regexp.MustCompile(`\b(curl|wget)\b[^;&|\n]*\|\s*(ba|z|k)?sh\b`), "curl | sh"},
	{regexp.MustCompile(`\bxargs\b`), "xargs"},
	{regexp.MustCompile(`\b(ba|z|k|)sh\s+-c\b`), "shell -c"},
	{regexp.MustCompile("`"), "反引号命令替换"},
	{regexp.MustCompile(`\$\(`), "命令替换"},
}

// wapper 只影响参数解析，不改变真正执行的命令：跳过它们继续找命令名。
var shellWrappers = map[string]bool{
	"env": true, "command": true, "nice": true, "nohup": true,
	"timeout": true, "exec": true, "stdbuf": true, "time": true,
}

// ErrApprovalParked 表示「这一轮已经停在等人确认上」。
//
// 它不是一个错误结论，也不该被翻译成「用户拒绝」：审批是会话的暂停点，
// 执行体收到它就把本轮结束（断点里留着那条没执行的调用），
// 等用户点了同意，由决策驱动的续跑把这条调用重新发出来。
var ErrApprovalParked = errors.New("本轮已暂停，等待人工确认")

// Approver 是审批通道：命中审批动作时登记待办，并把本轮停在这里。
// 实现由会话层提供（谁发起、谁能批、事件怎么推都在那一层）。
type Approver interface {
	// Decide 返回本轮记忆里的结论；没有可复用的结论时登记待办并返回
	// ErrApprovalParked，由调用方结束本轮。
	//
	// 不设等待上限，也不阻塞：审批是会话的暂停点，不是一次有超时的询问。
	// 用户不点，这一轮就一直停在那里（待办与断点同寿命），他回来点同意再接着跑。
	Decide(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// ApprovalRequest 是一条审批请求。
type ApprovalRequest struct {
	// ID 由 run_id + call_id 派生，前端点按钮时回传。
	ID        string
	RunID     string
	SessionID string
	CallID    string
	Tool      string
	// Kind 是动作类别（net.write / shell）。
	Kind string
	// Key 是本轮记忆的键：同类动作只问一次，不同命令分别问。
	Key string
	// ArgsHash 是同一次调用的指纹，用于续跑时复用结论（见 ArgsHash）。
	ArgsHash string
	Detail   string
	// Reason 是给用户看的一句话：为什么要确认这个动作。
	Reason string
	Risk   int
	// RiskLabel 是这次动作的具体标签（网络请求 / 写文件 / 脚本执行…）。
	// 确认卡与工具卡上的徽标用同一份文案，为空时退回等级文案。
	RiskLabel string
}

// ApprovalDecision 是用户的选择。
type ApprovalDecision struct {
	Allow bool
	// Remember 表示「本轮内同类动作都照这个结论」，False 表示只这一次。
	Remember bool
	// Reason 是拒绝时回给模型与审计的一句话。
	Reason string
}

// ApprovalFor 返回这次调用是否需要人工确认；nil 表示直接执行。
//
// 三类动作会命中：
//   - 会改动外部系统的调用：脚本插件按出网 verbs（非 GET/HEAD）判定，
//     MCP 按清单里的 tools.writes 声明判定——远端工具对平台是黑盒，
//     没有动词可看，只能由作者显式声明；
//   - 参数里的命令命中 shell 审批名单（或无法静态判断的动态形态）；
//   - shell 网络准入：模型声明了 need_network，且命令里出现网络工具或脚本时先确认
//     （判据见 ShellNetPlan）。没声明联网就不确认——命令会在无网沙箱里跑，
//     失败且像网络问题时由 Invoker 再申请一次联网重跑。
//
// 后两类可能同时命中（一条 `rm -rf` 的脚本既要名单确认又要联网准入）：合成一张卡
// 一次点击（见 mergeActions），不让人对同一件事点两次。
func ApprovalFor(t Tool, args json.RawMessage) *policy.Action {
	// 安装方的档位先于声明生效：always 与调用内容无关，off 直接放行。
	switch NormalizeApprovalLevel(t.ApprovalLevel) {
	case ApprovalAlways:
		return &policy.Action{
			Kind:   policy.ActionPlugin,
			Detail: argSummary(args),
			// Key 带上参数指纹：档位是「每次都问」，本轮记忆不能把它并成「同类动作只问一次」；
			// 但参数完全一样的重复调用（模型重试）复用结论，不再多弹一次。
			Key:    policy.ActionPlugin + ":" + t.Name + ":" + ArgsHash(t.Name, args),
			Reason: "这个插件被设为「每次都问」" + pluginAlwaysHint(t),
		}
	case ApprovalOff:
		return nil
	}
	if t.NetworkWrite {
		// MCP 工具与脚本工具的出网方式不同，文案要说清「谁在改、改哪里」：
		// 脚本是插件自己发请求，域名来自清单白名单；MCP 是远端 Server 替它改，
		// 平台看不到目标地址——这两件事对用户来说是完全不同的判断依据。
		if t.ServerID != "" {
			name := t.ServerName
			if name == "" {
				name = t.Name
			}
			return &policy.Action{
				Kind:   policy.ActionNetwork,
				Key:    policy.ActionNetwork,
				Detail: argSummary(args),
				Reason: "这次调用会改动远端系统（MCP 服务「" + name + "」），平台看不到它具体访问哪些地址",
			}
		}
		return &policy.Action{
			Kind:   policy.ActionNetwork,
			Key:    policy.ActionNetwork,
			Detail: argSummary(args),
			Reason: netWriteReason(t.NetHosts, t.NetVerbs),
		}
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		return nil
	}
	var hits []policy.Action
	if len(t.ShellApproval) > 0 {
		if hit := MatchApprovalCommand(in.Command, t.ShellApproval); hit != "" {
			hits = append(hits, policy.Action{
				Kind:   policy.ActionShell,
				Key:    policy.ActionShell + ":" + hit,
				Detail: in.Command,
				Reason: "命令包含需要确认的操作：" + hit,
			})
		}
	}
	// 网络准入与执行必须是同一个判据（ShellNetPlan）：这里批了、执行时不给网，
	// 或这里没问、执行时给了网，都是「用户以为没联网，实际联了」。
	//
	// Key 带参数指纹：同一条命令重试复用结论，改了命令则算另一次调用。
	// Invoker 用同一个 Key 认出「这次批的是联网」，批准才置 NetGrant。
	if plan := ShellNetPlan(in.Command, shellNeedNetwork(args)); plan.NeedsApproval() {
		hits = append(hits, policy.Action{
			Kind:   policy.ActionShell,
			Key:    shellNetApprovalKey(t.Name, args),
			Detail: in.Command,
			Reason: plan.Reason,
		})
	}
	return mergeActions(hits)
}

// netWriteReason 把「要访问哪里、用什么方法」写进确认卡的第一句话。
//
// 「访问外部系统」说了等于没说：用户要点同意，就得知道是哪个域名、发什么请求。
// 措辞用「可能访问」而不是「会访问」——域名是清单白名单，脚本运行时到底请求
// 其中哪一个、请求哪个路径，平台事前并不知道；说成确定目标同样是在骗人。
func netWriteReason(hosts, verbs []string) string {
	if len(hosts) == 0 {
		return "这次调用会以非只读方法访问外部网络，对方系统可能已经收到改动"
	}
	method := "非 GET 方法"
	if len(verbs) > 0 {
		method = strings.Join(verbs, "、")
	}
	return "这次调用可能访问 " + strings.Join(hosts, "、") +
		"（允许 " + method + "），对方系统可能已经收到改动"
}

// pluginAlwaysHint 补一句「这次大概要做什么」。
//
// 「每次都问」是安装方定的，与内容无关；但卡片只说这一句，用户仍然不知道
// 自己批的是读数据还是发请求。这里按工具声明补一个方向，不猜具体动作。
func pluginAlwaysHint(t Tool) string {
	switch {
	case t.NetworkWrite && t.ServerID != "":
		return "；这次调用会改动远端系统"
	case t.NetworkWrite:
		return "；这次调用可能访问外部网络"
	case len(t.ShellApproval) > 0 || t.NetPlan != nil:
		return "；这次调用会执行脚本"
	}
	return ""
}

// mergeActions 把同一次调用命中的多条审批动作并成一张卡。
//
// 一条命令可能既命中需确认名单（rm、反引号…）、又需要联网准入。分成两次问的话，
// 用户要对同一件事点两次同意，中间还各要续跑一轮（每轮都是一次完整的重新推理）。
// 合成一条之后一次点击覆盖两件事：Covers 记下每个键，执行侧据此判断「这次批的
// 包含给网」；Key 取第一条，让本轮记忆（scope=turn）的粒度与单独命中时一致。
func mergeActions(hits []policy.Action) *policy.Action {
	switch len(hits) {
	case 0:
		return nil
	case 1:
		one := hits[0]
		return &one
	}
	merged := hits[0]
	merged.Covers = make([]string, 0, len(hits))
	reasons := make([]string, 0, len(hits))
	for _, h := range hits {
		merged.Covers = append(merged.Covers, h.Key)
		if h.Reason != "" {
			reasons = append(reasons, strings.TrimSuffix(h.Reason, "。"))
		}
	}
	merged.Reason = strings.Join(reasons, "；") + "。"
	return &merged
}

// MatchApprovalCommand 返回命中的规则名，空表示不需要确认。
//
// 判不准就判危险：出现命令替换、xargs、嵌套 shell 时静态分析不成立，一律要求确认。
//
// 只看「会被 shell 解释的部分」（见 shellCommandText）：写进文件的正文是数据，
// 里面的反引号、$( ) 在带引号的 heredoc 里不会被展开，按原文匹配等于每写一次
// 代码就问一次，问了也没有可判断的信息。
func MatchApprovalCommand(command string, list []string) string {
	text := shellCommandText(command)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	for _, c := range shellCombos {
		if c.re.MatchString(text) {
			return c.name
		}
	}
	allow := make(map[string]bool, len(list))
	for _, c := range list {
		allow[strings.TrimSpace(c)] = true
	}
	for _, word := range commandWords(text) {
		if allow[word] {
			return word
		}
	}
	return ""
}

// commandWords 按管道/顺序分隔符切段，取每段真正执行的命令名。
func commandWords(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, seg := range fields {
		for _, token := range strings.Fields(seg) {
			token = strings.Trim(token, `"'()`)
			// VAR=value 前缀不是命令名，继续往后找
			if i := strings.IndexByte(token, '='); i > 0 && !strings.ContainsAny(token[:i], "/.-") {
				continue
			}
			if shellWrappers[token] {
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			if name := baseName(token); name != "" {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

func baseName(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// argSummary 把入参压成一行摘要，供审批卡片展示。
func argSummary(args json.RawMessage) string {
	const max = 300
	s := strings.TrimSpace(string(args))
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
