package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

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

// DefaultApprovalTimeout 是一次审批请求的等待上限；超时按拒绝处理。
const DefaultApprovalTimeout = 5 * time.Minute

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

// Approver 是审批通道：命中审批动作时阻塞等待用户决策。
// 实现由会话层提供（谁发起、谁能批、事件怎么推都在那一层）。
type Approver interface {
	// Decide 返回本轮记忆里的结论，或阻塞等待用户点击（超时按拒绝）。
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
	// Timeout 为零时用 DefaultApprovalTimeout。
	Timeout time.Duration
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
//   - shell 网络准入：网络工具（curl / wget / nc / ssh）一律先确认，拒绝则不执行；
//     脚本解释器声明了 network=true 才确认（默认无网执行）。判据见 ShellNetPlan。
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
			Reason: "这个插件的审批级别是「每次调用都确认」",
		}
	case ApprovalOff:
		return nil
	}
	if t.NetworkWrite {
		// MCP 工具与脚本工具的出网方式不同，文案要说清「谁在改」：
		// 脚本是插件自己发请求，MCP 是远端 Server 替它改，用户判断依据不一样。
		reason := "这次调用会以非只读方法访问外部系统"
		if t.ServerID != "" {
			reason = "这次调用会改动远端系统（MCP 工具）"
		}
		return &policy.Action{
			Kind:   policy.ActionNetwork,
			Key:    policy.ActionNetwork,
			Detail: argSummary(args),
			Reason: reason,
		}
	}
	var in struct {
		Command string `json:"command"`
		Network bool   `json:"network"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		return nil
	}
	if len(t.ShellApproval) > 0 {
		if hit := MatchApprovalCommand(in.Command, t.ShellApproval); hit != "" {
			return &policy.Action{
				Kind:   policy.ActionShell,
				Key:    policy.ActionShell + ":" + hit,
				Detail: in.Command,
				Reason: "命令包含需要确认的操作：" + hit,
			}
		}
	}
	// 网络准入与执行必须是同一个判据（ShellNetPlan）：这里批了、执行时不给网，
	// 或这里没问、执行时给了网，都是「用户以为没联网，实际联了」。
	//
	// 网络工具不批准就不执行；脚本默认无网，只有声明 network=true 才走到这里。
	// Key 带参数指纹：同一条命令重试复用结论，改了 network 参数则算另一次调用。
	if plan := ShellNetPlan(in.Command, in.Network); plan.NeedsApproval() {
		return &policy.Action{
			Kind:   policy.ActionShell,
			Key:    policy.ActionShell + ":net:" + ArgsHash(t.Name, args),
			Detail: in.Command,
			Reason: plan.Reason,
		}
	}
	return nil
}

// MatchApprovalCommand 返回命中的规则名，空表示不需要确认。
//
// 判不准就判危险：出现命令替换、xargs、嵌套 shell 时静态分析不成立，一律要求确认。
func MatchApprovalCommand(command string, list []string) string {
	if strings.TrimSpace(command) == "" {
		return ""
	}
	for _, c := range shellCombos {
		if c.re.MatchString(command) {
			return c.name
		}
	}
	allow := make(map[string]bool, len(list))
	for _, c := range list {
		allow[strings.TrimSpace(c)] = true
	}
	for _, word := range commandWords(command) {
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
