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
// 判据分两步：先看模型声明的 need_network，再看每段命令真正执行的命令名
// （归成装依赖 / 跑脚本 / 网络工具三类），按 docs/K8s容器沙箱方案.md「网络策略」给结论：
//
//	未声明 / false，只有装依赖     有网，不确认（装依赖本来就是下载）
//	未声明 / false，其余           无网执行；脚本 / 网络工具失败且像网络问题时申请联网重跑
//	声明 true，只有装依赖           有网，不确认
//	声明 true，有脚本或网络工具      先确认：批准有网，拒绝不执行
//	声明 true，什么都没有（ls/git）  有网，不确认
//
// 为什么让模型先声明：只有它知道这条命令要干什么。判据只负责在「它说要网」之后
// 决定要不要人看一眼——装依赖的终点由安装源决定，没有把对话内容拼进请求的通路；
// 脚本是「跑一段未知代码」，网络工具的唯一用途就是发请求，两者都要给用户看命令原文。
//
// 声明 false 时一律无网：没有网就没有外发通路，所以不需要人确认。
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
	// NetCategoryOther 是其余命令：声明联网时有网，不弹确认。
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

// ShellNetPolicy 是一条命令最终落到沙箱上的网络策略。
//
// 没有「拒绝后仍按无网执行」这一档：声明了要网而用户不同意，这条命令就不执行。
// 用户拒的是「让这段未知代码/网络工具带着网跑」，替他把半条命令跑完不是他要的结论。
type ShellNetPolicy struct {
	Mode ShellNetMode
	// Category 是命中的类别，写审计时能看出「这次为什么给了网」。
	Category ShellNetCategory
	// Reason 是审批卡上的一句话，只在 Mode == ShellNetAsk 时有值。
	// 要具体到「哪个命令、会怎样」，不要写「该操作需要确认」这种等于没说的话。
	Reason string
	// Escalate 表示「先无网跑，网络失败后再申请联网重跑」。
	// 只在模型没声明联网时成立：声明了就已经在审批里问过了。
	Escalate bool
}

// NeedsNetwork 表示这条命令在「已获准」时应当联网。
func (p ShellNetPolicy) NeedsNetwork() bool { return p.Mode != ShellNetOff }

// NeedsApproval 表示执行前必须拿到用户许可。
func (p ShellNetPolicy) NeedsApproval() bool { return p.Mode == ShellNetAsk }

// shellNetTools 是「用途就是发请求」的命令：声明联网时必须拿到用户许可。
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
// needNetwork 是模型对这次调用的判断：需要外网就声明 true。声明 false（或不写）时
// 一律无网执行——没有网就没有外发通路，不需要再问人；真需要时脚本会因断网失败，
// 再走一次「联网重跑」的审批，模型判断错了也不至于卡死。
//
// 唯一的例外是纯装依赖：那是下载，没网只会白跑一次，装什么也由镜像源决定，
// 所以无论声明与否都给网、不弹确认。
func ShellNetPlan(command string, needNetwork bool) ShellNetPolicy {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryOther}
	}

	k := classifyCommand(cmd)
	switch {
	case !needNetwork && k.install && !k.script && !k.netTool:
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryInstall}
	case !needNetwork:
		return ShellNetPolicy{Mode: ShellNetOff, Category: k.category(), Escalate: k.script || k.netTool}
	case k.netTool || k.script:
		return ShellNetPolicy{Mode: ShellNetAsk, Category: k.category(), Reason: k.reason()}
	default:
		return ShellNetPolicy{Mode: ShellNetOn, Category: k.category()}
	}
}

// shellNetKinds 是一条命令里出现的类别汇总。
type shellNetKinds struct {
	install bool
	script  bool
	netTool bool
	// netNames 是命中的网络工具名（去重、最多三个），审批卡上要指名道姓。
	netNames []string
}

func (k shellNetKinds) category() ShellNetCategory {
	switch {
	case k.netTool:
		return NetCategoryTool
	case k.install && k.script:
		return NetCategoryInstallScript
	case k.install:
		return NetCategoryInstall
	case k.script:
		return NetCategoryScript
	default:
		return NetCategoryOther
	}
}

// reason 是审批卡上的一句话：审批只发生在「声明要网且命令里有脚本或网络工具」时。
func (k shellNetKinds) reason() string {
	const (
		aftermath = "批准后有网执行，拒绝则不执行。"
		sensitive = "请检查命令里是否带上了对话内容、文件内容或其他敏感信息"
	)
	switch {
	case k.netTool && k.script:
		return fmt.Sprintf("命令包含 %s 与脚本执行：前者直接访问网络，后者是未知代码。%s%s",
			strings.Join(k.netNames, " / "), aftermath, sensitive)
	case k.netTool:
		return fmt.Sprintf("命令包含 %s，会直接访问网络。%s%s",
			strings.Join(k.netNames, " / "), aftermath, sensitive)
	case k.install && k.script:
		return "这条命令既装依赖又跑脚本：装依赖需要联网，脚本内容是未知代码。" +
			aftermath + sensitive
	default:
		return "这条命令会执行脚本（未知代码），脚本里可能发起网络请求。" +
			aftermath + sensitive
	}
}

// classifyCommand 把整条命令按段归类。
//
// /dev/tcp 是 bash 内置通道，不需要任何外部二进制就能建立连接，单独算网络工具；
// 复合命令按最严的一档合并（网络工具 > 装依赖 + 脚本 > 装依赖 / 脚本）。
//
// 只看会被解释的部分（shellCommandText）：heredoc 正文是数据，里面写着 curl 或
// python 不代表这条命令在联网——真的执行它的是后面那一条调用，那时再判即可。
func classifyCommand(cmd string) shellNetKinds {
	var k shellNetKinds
	text := shellCommandText(cmd)
	if strings.Contains(text, "/dev/tcp/") {
		k.netTool = true
		k.netNames = append(k.netNames, "/dev/tcp")
	}
	for _, fields := range shellSegmentFields(text) {
		head := segmentHead(fields)
		if head == "" {
			continue
		}
		switch classifySegment(fields, head) {
		case kindNetTool:
			k.netTool = true
			k.addNetName(head)
		case kindInstall:
			k.install = true
		case kindScript:
			k.script = true
		}
	}
	return k
}

// addNetName 记下命中的网络工具名：去重，最多三个，避免复合命令把审批卡撑满。
func (k *shellNetKinds) addNetName(name string) {
	for _, n := range k.netNames {
		if n == name {
			return
		}
	}
	if len(k.netNames) >= 3 {
		return
	}
	k.netNames = append(k.netNames, name)
}

// shellNeedNetwork 取模型对这次调用的判断：这条命令是否需要访问外网。
//
// 缺省为 false（按无网执行）：没有声明就当不需要，安全的一侧是静默的；
// 反过来默认给网会让「模型忘了写参数」变成一次无人确认的外发机会。
func shellNeedNetwork(args json.RawMessage) bool {
	var in struct {
		NeedNetwork bool `json:"need_network"`
	}
	_ = json.Unmarshal(args, &in)
	return in.NeedNetwork
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

// ShellNetRetryReason 是「没声明联网、无网执行失败后申请联网重跑」审批卡上的话。
const ShellNetRetryReason = "这条命令在没有网络的沙箱里失败了，错误看起来是网络不可达。" +
	"联网重跑后它能访问任意公网地址，请检查命令与参数里是否带上了对话内容、文件内容或其他敏感信息"

// ShellNetRetryDoneNote 是联网重跑成功后的回执说明。
//
// 必须带上「下次直接声明」：这是模型唯一能纠正「漏声明」的地方。只在被拒时提醒，
// 模型下一轮会用同样的方式再撞一次——先无网失败、再等用户批一轮，每次多一次确认。
const ShellNetRetryDoneNote = "\n[提示] 用户批准了联网重跑，上面是这次联网执行的结果。" +
	"同类命令（脚本自身要联网）请直接在参数里设 need_network=true，不用再走「先失败、再重跑」。\n"

// ShellNetRetryDeniedNote 是没拿到联网许可时的回执说明。
const ShellNetRetryDeniedNote = "\n[提示] 这次执行仍是无网环境，联网重跑没有获批。" +
	"确实需要联网就设 need_network=true 重新调用；纯本地处理请去掉网络请求。\n"

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
