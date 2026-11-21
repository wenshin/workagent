package tool

import (
	"fmt"
	"regexp"
	"strings"
)

// 这个文件是 shell 网络策略的唯一判据。
//
// 执行（Spec.Network）与审批（ApprovalFor）都必须读这里的结果：
// 两处各判一次就一定会漂移，出现「批了但没网」或「没批就联网」，而后者是静默的。
//
// 三条档位（docs/K8s容器沙箱方案.md「网络策略：按命令形态分三档」）：
//
//	网络工具（curl / wget / nc / ssh）  有网 + 先审批（拒绝即不执行）
//	脚本解释器（sh / python / node / go） 默认无网；声明 network=true 才有网 + 审批
//	其他命令（ls / git / npm / apt）      有网 + 不审批
//
// 给不给网不改变文件可见面：三档都挂整个工作区，`curl -o x` 与随后的脚本能连着写。
//
// 判据按「每段命令真正执行的那个命令名」分类，不看整条命令里出现过什么词：
// `pip install node-fetch` 的参数里带 node，但它执行的是 pip，属于「其他命令」。

// ShellNetCategory 是命令的网络分类，只用于审计与测试。
type ShellNetCategory string

const (
	// NetCategoryTool 是网络工具：用途就是把数据发出去。
	NetCategoryTool ShellNetCategory = "net-tool"
	// NetCategoryScript 是脚本解释器：跑一段代码，里面有没有网络请求静态看不出来。
	NetCategoryScript ShellNetCategory = "script"
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
	// ShellNetAsk 给网，但必须先经用户确认；拒绝就不执行。
	ShellNetAsk
)

// ShellNetPolicy 是一条命令最终落到沙箱上的网络策略。
type ShellNetPolicy struct {
	Mode ShellNetMode
	// Category 是命中的类别，写审计时能看出「这次为什么给了网」。
	Category ShellNetCategory
	// Reason 是审批卡上的一句话，只在 Mode == ShellNetAsk 时有值。
	// 要具体到「哪个命令、会怎样」，不要写「该操作需要确认」这种等于没说的话。
	Reason string
}

// NeedsNetwork 是 sandbox.Spec.Network 的取值。
func (p ShellNetPolicy) NeedsNetwork() bool { return p.Mode != ShellNetOff }

// NeedsApproval 表示执行前必须拿到用户许可，拿不到就不执行。
func (p ShellNetPolicy) NeedsApproval() bool { return p.Mode == ShellNetAsk }

// shellNetTools 是「用途就是发请求」的命令。
//
// 它们联网前必须拿到用户许可：一条被注入的 curl 就能把会话内容拼进 URL 带出去。
// 许可之后照常在工作区里执行，下载与解析可以连着写。
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
	"ruby": true, "perl": true, "php": true, "lua": true, "Rscript": true,
	"go": true, "java": true, "julia": true, "swift": true, "dotnet": true,
}

// shellRunnerVersioned 匹配带版本号的入口（python3.12 / node18 / php8）。
// 不用子串：`python-dateutil` 是包名，不是解释器。
var shellRunnerVersioned = regexp.MustCompile(`^(python|node|deno|bun|ruby|perl|php|go|java|lua)[0-9.]*$`)

func isScriptRunner(name string) bool {
	return shellScriptRunners[name] || shellRunnerVersioned.MatchString(name)
}

// ShellNetPlan 给出一条命令的网络策略。
//
// wantNet 是调用方（模型）在参数里声明的「这次脚本需要联网」。它只对脚本档生效：
// 网络工具本来就必须联网、必须审批，声明与否都一样；其他命令默认就有网。
// 这样设计是为了不给模型「传个参数就能逼出审批」的入口。
func ShellNetPlan(command string, wantNet bool) ShellNetPolicy {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryOther}
	}
	// bash 内置通道：不需要任何外部二进制就能建立连接。
	if strings.Contains(cmd, "/dev/tcp/") {
		return ShellNetPolicy{
			Mode:     ShellNetAsk,
			Category: NetCategoryTool,
			Reason: "命令使用 /dev/tcp 直接建立网络连接。" +
				"请检查命令里是否带上了对话内容、文件内容或其他敏感信息",
		}
	}

	hasScript := false
	for _, name := range shellNetHeads(cmd) {
		if shellNetTools[name] {
			return ShellNetPolicy{
				Mode:     ShellNetAsk,
				Category: NetCategoryTool,
				Reason: fmt.Sprintf("%s 会直接访问网络。"+
					"请检查命令里是否带上了对话内容、文件内容或其他敏感信息", name),
			}
		}
		if isScriptRunner(name) {
			hasScript = true
		}
	}

	if hasScript {
		if wantNet {
			return ShellNetPolicy{
				Mode:     ShellNetAsk,
				Category: NetCategoryScript,
				Reason: "这段脚本将以联网方式执行（脚本默认不联网）。联网后能访问任意公网地址，" +
					"请检查脚本与参数里是否带上了对话内容、文件内容或其他敏感信息",
			}
		}
		return ShellNetPolicy{Mode: ShellNetOff, Category: NetCategoryScript}
	}

	return ShellNetPolicy{Mode: ShellNetOn, Category: NetCategoryOther}
}

// shellNetPrefix 是只改身份 / 调度、不改动作的前缀命令。
//
// 判定时必须穿透：`sudo curl …` 真正执行的是 curl，不能因为首词是 sudo
// 就当普通命令放行——那样「拿到网却没有审批」是静默的。
var shellNetPrefix = map[string]bool{
	"sudo": true, "su": true, "env": true, "command": true, "exec": true,
	"nice": true, "nohup": true, "timeout": true, "stdbuf": true, "time": true,
}

// shellNetHeads 返回每段命令真正执行的命令名。
//
// 与 commandWords 的差别：那个函数服务于「需确认命令」名单，必须抓到 sudo 本身；
// 这个函数要穿过 sudo / timeout / env 找到真正的动作，否则审批与授权会错位。
//
// 局限：`sudo -u root curl …` 里 `root` 是选项值，当前会把它当成命令名而漏判。
// 这类形态仍会被「需确认命令」名单里的 sudo 命中并弹审批，不会漏掉确认；
// 写一个完整的 shell 词法分析器来消灭它不划算。
func shellNetHeads(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		fields := strings.Fields(seg)
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
			continue
		}
		if shellNetPrefix[head] {
			// 前缀之后可能还有选项与选项值，直接找这一段里第一个已知的动作名。
			if real := firstKnownCommand(fields); real != "" {
				head = real
			}
		}
		out = append(out, head)
	}
	return out
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

// looksLikeNetworkFailure 判断一次「无网执行」的失败是否像网络问题。
//
// 脚本默认不联网，模型看到非零退出码时未必知道原因是没网。命中这些片段时
// 在回执末尾补一句「可以声明 network=true 重试」，比让它反复猜要省一轮。
func looksLikeNetworkFailure(stdout, stderr string) bool {
	text := strings.ToLower(stdout + "\n" + stderr)
	for _, frag := range []string{
		"temporary failure in name resolution",
		"name or service not known",
		"could not resolve host",
		"nodename nor servname",
		"network is unreachable",
		"enetunreach",
		"ehostunreach",
		"eai_again",
		"getaddrinfo",
		"connection refused",
		"econnrefused",
		"connection timed out",
		"etimedout",
		"socket.gaierror",
		"urlopen error",
		"failed to establish a new connection",
	} {
		if strings.Contains(text, frag) {
			return true
		}
	}
	return false
}
