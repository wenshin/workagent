package tool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
)

// 这个文件是 shell 网络策略的唯一判据。
//
// 执行（Spec.Network）、审批（ApprovalFor）与失败后的升级重跑都读这里的结果：
// 三处各判一次就一定会漂移，出现「批了但没网」或「没批就联网」，而后者是静默的。
//
// 判据只看每段命令真正执行的命令名，把它们归成三类：装依赖 / 跑脚本 / 网络工具，
// 再按 docs/K8s容器沙箱方案.md「网络策略」的四条规则给结论：
//
//	只有装依赖            有网，不确认
//	只有脚本              无网执行；失败且像网络问题时申请联网重跑一次
//	装依赖 + 脚本          先确认：批准有网，拒绝按无网执行
//	出现网络工具           先确认：批准有网，拒绝不执行
//	什么都没有（ls/git）   有网，不确认
//
// 分档理由：装依赖是「下载」，终点由安装源决定，没有把对话内容拼进请求的通路；
// 脚本是「跑一段未知代码」，里面有没有外发静态看不出来；网络工具的唯一用途就是发请求。
//
// 给不给网不改变文件可见面：每类都挂整个工作区，`curl -o x` 与随后的脚本能连着写。

// ShellNetCategory 是命令的网络分类，只用于审计、测试与拒绝后的处理方式。
type ShellNetCategory string

const (
	// NetCategoryInstall 是只有装依赖：下载安装，不执行用户代码。
	NetCategoryInstall ShellNetCategory = "install"
	// NetCategoryScript 是只有脚本：跑一段未知代码，默认无网。
	NetCategoryScript ShellNetCategory = "script"
	// NetCategoryInstallScript 是装依赖与脚本混在一起：两类都要，先问人。
	NetCategoryInstallScript ShellNetCategory = "install+script"
	// NetCategoryTool 是网络工具：用途就是把数据发出去。
	NetCategoryTool ShellNetCategory = "net-tool"
	// NetCategoryOther 是其余命令：默认有网，不弹确认。
	NetCategoryOther ShellNetCategory = "other"
)

// ShellNetMode 是一条命令的网络档位。
type ShellNetMode int

const (
	// ShellNetOff 不给网（沙箱 --unshare-net）。
	ShellNetOff ShellNetMode = iota
	// ShellNetOn 给网，不需要确认。
	ShellNetOn
	// ShellNetAsk 给网，但必须先经用户确认。
	ShellNetAsk
)

// ShellDeny 是用户拒绝联网之后这条命令的处置方式。
//
// 两种处置不能合并：网络工具拒绝后连执行都不该做（它的用途就是外发），
// 而「装依赖 + 脚本」拒绝联网后仍应按无网跑完——用户拒的是网，不是这个任务。
type ShellDeny int

const (
	// DenyRefuse 拒绝即不执行。
	DenyRefuse ShellDeny = iota
	// DenyOffline 拒绝后按无网执行。
	DenyOffline
)

// ShellNetPolicy 是一条命令最终落到沙箱上的网络策略。
type ShellNetPolicy struct {
	Mode ShellNetMode
	// Category 是命中的类别，写审计时能看出「这次为什么给了网」。
	Category ShellNetCategory
	// Reason 是审批卡上的一句话，只在 Mode == ShellNetAsk 时有值。
	// 要具体到「哪个命令、会怎样」，不要写「该操作需要确认」这种等于没说的话。
	Reason string
	// OnDeny 决定用户拒绝之后怎么办。
	OnDeny ShellDeny
	// Escalate 表示「先无网跑，网络失败后再申请联网重跑」，只对纯脚本成立。
	Escalate bool
}

// NeedsNetwork 表示这条命令在「已获准」时应当联网。
func (p ShellNetPolicy) NeedsNetwork() bool { return p.Mode != ShellNetOff }

// NeedsApproval 表示执行前必须拿到用户许可。
func (p ShellNetPolicy) NeedsApproval() bool { return p.Mode == ShellNetAsk }

// shellNetTools 是「用途就是发请求」的命令。
//
// 它们联网前必须拿到用户许可：一条被注入的 curl 就能把会话内容拼进 URL 带出去。
var shellNetTools = map[string]bool{
	"curl": true, "wget": true, "aria2c": true,
	"nc": true, "ncat": true, "netcat": true, "socat": true,
	"telnet": true, "ssh": true, "scp": true, "sftp": true, "rsync": true, "ftp": true,
	"ping": true, "dig": true, "nslookup": true, "host": true, "traceroute": true,
	// httpie 的两个入口
	"http": true, "https": true,
}

// shellScriptRunners 是脚本解释器：跑的是代码，静态看不出里面有没有网络请求。
var shellScriptRunners = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ash": true, "zsh": true, "ksh": true, "fish": true,
	"node": true, "deno": true, "bun": true, "tsx": true,
	"npx": true, "bunx": true, "uvx": true, "uv": true,
	"ruby": true, "perl": true, "php": true, "lua": true, "Rscript": true,
	"go": true, "java": true, "julia": true, "swift": true, "dotnet": true,
}

// shellInstallers 是装依赖的入口：参数是包名，行为是下载并落地文件。
//
// 与 shellScriptRunners 有交集（go / deno / dotnet / uv）：同一个入口换个子命令
// 就换了类别——`go install` 是下载，`go run` 是跑代码。顺序上先看子命令，
// 见 classifySegment。
var shellInstallers = map[string]bool{
	"pip": true, "pip3": true, "pipx": true, "uv": true, "poetry": true, "conda": true,
	"npm": true, "pnpm": true, "yarn": true, "gem": true, "bundler": true,
	"cargo": true, "go": true, "deno": true, "dotnet": true,
	"apt": true, "apt-get": true, "apk": true, "dnf": true, "yum": true, "brew": true,
	"mise": true, "composer": true,
}

// shellInstallVerbs 是「下载安装」的子命令；其余子命令按解释器处理（在跑代码）。
var shellInstallVerbs = map[string]bool{
	"install": true, "i": true, "add": true, "ci": true, "update": true, "upgrade": true,
	"sync": true, "download": true, "fetch": true, "get": true, "restore": true, "vendor": true,
}

// shellFixedModules 是 `python -m <模块>` 里只建目录、既不下载也不跑用户代码的模块。
// 归到其他命令：`python3 -m venv v && pip install x` 的第二段要网，第一段不该把它拖成无网。
var shellFixedModules = map[string]bool{
	"venv": true, "virtualenv": true,
}

// shellModuleInstalls 是 `python -m <模块>` 里的装依赖入口。
var shellModuleInstalls = map[string]bool{
	"pip": true, "pip3": true, "ensurepip": true,
}

// shellRunnerVersioned 匹配带版本号的入口（python3.12 / node18 / php8）。
// 不用子串：`python-dateutil` 是包名，不是解释器。
var shellRunnerVersioned = regexp.MustCompile(`^(python|node|deno|bun|ruby|perl|php|go|java|lua)[0-9.]*$`)

func isScriptRunner(name string) bool {
	return shellScriptRunners[name] || shellRunnerVersioned.MatchString(name)
}

// shellCommandKind 是一段命令的类别。
type shellCommandKind int

const (
	kindOther shellCommandKind = iota
	kindInstall
	kindScript
	kindNetTool
)

// classifySegment 给一段命令归类：网络工具 > 装依赖 > 脚本 > 其他。
//
// 「装依赖」必须看子命令而不是只看入口名：`npm i` 与 `npm run build` 是同一个
// 入口，但前者是下载、后者在跑 Makefile 一样的脚本。
func classifySegment(fields []string, head string) shellCommandKind {
	if shellNetTools[head] {
		return kindNetTool
	}
	if isScriptRunner(head) {
		if mod, ok := pythonModule(fields, head); ok {
			switch {
			case shellFixedModules[mod]:
				return kindOther
			case shellModuleInstalls[mod]:
				return kindInstall
			}
			return kindScript
		}
		if shellInstallers[head] && shellInstallVerbs[firstSubcommand(fields)] {
			return kindInstall
		}
		return kindScript
	}
	if shellInstallers[head] && shellInstallVerbs[firstSubcommand(fields)] {
		return kindInstall
	}
	return kindOther
}

// pythonModule 取 `python -m <模块>` 里的模块名；不是这种形态时返回 false。
func pythonModule(fields []string, head string) (string, bool) {
	if !strings.HasPrefix(strings.TrimLeft(baseName(head), `"'()`), "python") {
		return "", false
	}
	for i := 0; i+1 < len(fields); i++ {
		switch strings.Trim(fields[i], `"'()`) {
		case "-m", "--module":
			return strings.Trim(fields[i+1], `"'()`), true
		}
	}
	return "", false
}

// firstSubcommand 返回命令名之后的第一个非选项参数（`pip install` 的 install）。
func firstSubcommand(fields []string) string {
	seenHead := false
	for _, token := range fields {
		token = strings.Trim(token, `"'()`)
		if token == "" {
			continue
		}
		if i := strings.IndexByte(token, '='); i > 0 && !strings.ContainsAny(token[:i], "/.-") {
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		if !seenHead {
			seenHead = true
			continue
		}
		return token
	}
	return ""
}

// ShellNetPlan 给出一条命令的网络策略。
//
// 模型不再声明 network 参数：联网与否完全由命令内容决定。声明式开关与真实行为
// 漂移时，模型会以为自己申请过网，而用户看到的是「批了却没网」。
func ShellNetPlan(command string) ShellNetPolicy {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryOther}
	}
	// bash 内置通道：不需要任何外部二进制就能建立连接。
	if strings.Contains(cmd, "/dev/tcp/") {
		return ShellNetPolicy{
			Mode:     ShellNetAsk,
			Category: NetCategoryTool,
			OnDeny:   DenyRefuse,
			Reason: "命令使用 /dev/tcp 直接建立网络连接。" +
				"请检查命令里是否带上了对话内容、文件内容或其他敏感信息",
		}
	}

	install, script := false, false
	for _, fields := range shellSegmentFields(cmd) {
		head := segmentHead(fields)
		if head == "" {
			continue
		}
		switch classifySegment(fields, head) {
		case kindNetTool:
			return ShellNetPolicy{
				Mode:     ShellNetAsk,
				Category: NetCategoryTool,
				OnDeny:   DenyRefuse,
				Reason: fmt.Sprintf("%s 会直接访问网络。"+
					"请检查命令里是否带上了对话内容、文件内容或其他敏感信息", head),
			}
		case kindInstall:
			install = true
		case kindScript:
			script = true
		}
	}

	switch {
	case install && script:
		return ShellNetPolicy{
			Mode:     ShellNetAsk,
			Category: NetCategoryInstallScript,
			OnDeny:   DenyOffline,
			Reason: "这条命令既装依赖又跑脚本：装依赖需要联网，脚本内容是未知代码。" +
				"批准后整条命令联网执行；拒绝则按无网执行（装依赖多半会失败）。" +
				"请检查命令里是否带上了对话内容、文件内容或其他敏感信息",
		}
	case install:
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryInstall}
	case script:
		return ShellNetPolicy{Mode: ShellNetOff, Category: NetCategoryScript, Escalate: true}
	default:
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryOther}
	}
}

// shellNetApprovalKey 是「联网准入」审批在本轮的记忆键：同一条命令重试复用结论，
// 换了命令则算另一次动作。
func shellNetApprovalKey(toolName string, args json.RawMessage) string {
	return policy.ActionShell + ":net:" + ArgsHash(toolName, args)
}

// errNetNeeded 是 shell 回给 Invoker 的控制信号：这次无网执行失败了，
// 而且错误看起来是网络不可达。
//
// 它不是平台错误：Invoker 收到它会去申请联网重跑，而不是把错误回给模型。
// 用错误传递是因为工具只能返回（结果, error）两个值，而「要不要问用户」
// 必须由持有审批通道与墙钟外 ctx 的 Invoker 决定。
type errNetNeeded struct{}

func (errNetNeeded) Error() string { return "无网执行失败：疑似网络不可达" }

// shellNetRetryKey 是「无网失败后申请联网重跑」的记忆键。
func shellNetRetryKey(toolName string, args json.RawMessage) string {
	return policy.ActionShell + ":net-retry:" + ArgsHash(toolName, args)
}

// shellCommandOf 取 shell_exec 参数里的命令原文：审批卡与回执都要用它。
func shellCommandOf(args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &in)
	return strings.TrimSpace(in.Command)
}

// ShellNetRetryReason 是「无网脚本失败后申请联网重跑」审批卡上的话。
const ShellNetRetryReason = "这段脚本在没有网络的沙箱里失败了，错误看起来是网络不可达。" +
	"联网重跑后它能访问任意公网地址，请检查命令与参数里是否带上了对话内容、文件内容或其他敏感信息"

// ShellNetRetryDoneNote 是联网重跑成功后的回执说明。
const ShellNetRetryDoneNote = "\n[提示] 用户批准了联网重跑，上面是联网执行的结果。\n"

// ShellNetRetryDeniedNote 是没拿到联网许可时的回执说明。
const ShellNetRetryDeniedNote = "\n[提示] 这次执行仍是无网环境，联网重跑没有获批。" +
	"需要联网请说明用途，或改用装依赖 / 网络工具的方式。\n"

// shellNetPrefix 是只改身份 / 调度、不改动作的前缀命令。
//
// 判定时必须穿透：`sudo curl …` 真正执行的是 curl，不能因为首词是 sudo
// 就当普通命令放行——那样「拿到网却没有审批」是静默的。
var shellNetPrefix = map[string]bool{
	"sudo": true, "su": true, "env": true, "command": true, "exec": true,
	"nice": true, "nohup": true, "timeout": true, "stdbuf": true, "time": true,
}

// shellSegmentFields 复用 shellSegments 的切段规则（尊重引号与转义），
// 再切成字段：分类要看整段参数（`-m pip`、子命令这类判断要越过选项）。
func shellSegmentFields(command string) [][]string {
	segs := shellSegments(command)
	out := make([][]string, 0, len(segs))
	for _, seg := range segs {
		if fields := strings.Fields(seg); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// segmentHead 找出一段命令真正执行的命令名，穿透 sudo / timeout / env 这类前缀。
//
// 局限：`sudo -u root curl …` 里 `root` 是选项值，当前会把它当成命令名而漏判。
// 这类形态仍会被「需确认命令」名单里的 sudo 命中并弹审批，不会漏掉确认；
// 写一个完整的 shell 词法分析器来消灭它不划算。
func segmentHead(fields []string) string {
	head := ""
	for _, token := range fields {
		token = strings.Trim(token, `"'()`)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		// VAR=value 前缀不是命令名，继续往后找
		if i := strings.IndexByte(token, '='); i > 0 && !strings.ContainsAny(token[:i], "/.-") {
			continue
		}
		head = baseName(token)
		break
	}
	if head == "" {
		return ""
	}
	if shellNetPrefix[head] {
		// 前缀之后可能还有选项与选项值，直接找这一段里第一个已知的动作名。
		if real := firstKnownCommand(fields); real != "" {
			head = real
		}
	}
	return head
}

// firstKnownCommand 在一段命令里找第一个已知的网络工具或解释器。
//
// 只在首词是 sudo / timeout 这类前缀时调用，把 `timeout 30 curl …` 里的
// `30`、`sudo -E` 里的选项跳过去。找不到就返回空，调用方保留原首词。
func firstKnownCommand(fields []string) string {
	for _, token := range fields {
		token = strings.Trim(token, `"'()`)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		name := baseName(token)
		if shellNetTools[name] || isScriptRunner(name) {
			return name
		}
	}
	return ""
}

// looksLikeNetworkFailure 判断一次无网执行的失败是否像网络不可达。
//
// 只用于「纯脚本」的升级重跑：脚本默认无网，模型看到非零退出码时未必知道
// 原因是没网。判据要宽到能覆盖常见运行时的报错形态，但只在失败时用，
// 误判的代价是「多问用户一次」，漏判的代价是「脚本永远拿不到网」。
func looksLikeNetworkFailure(stdout, stderr string) bool {
	text := strings.ToLower(stdout + "\n" + stderr)
	for _, frag := range []string{
		"temporary failure in name resolution",
		"name or service not known",
		"could not resolve host",
		"nodename nor servname",
		"network is unreachable",
		"no route to host",
		"enetunreach",
		"ehostunreach",
		"eai_again",
		"enotfound",
		"getaddrinfo",
		"connection refused",
		"econnrefused",
		"connection timed out",
		"connect timeout",
		"etimedout",
		"socket.gaierror",
		"urlopen error",
		"failed to establish a new connection",
		"no matching distribution found",
		"could not find a version that satisfies",
	} {
		if strings.Contains(text, frag) {
			return true
		}
	}
	return false
}
