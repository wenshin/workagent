package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/sandbox"
)

const (
	// Invoker 对单次工具调用的墙钟是 30s，这里必须明显短于它，
	// 否则命令还没到自己的超时就被外层掐断，报错会指向错误的层。
	defaultShellTimeout = 15 * time.Second
	maxShellTimeout     = 25 * time.Second
	// 命令输出比文件内容更碎，给 16KB；再多对模型也没有价值。
	shellMaxResultBytes = 16 * 1024
)

// localPathStore 是能把 key 解析成本机路径的存储实现（目前是 localfs）。
// 用可选接口而不是扩大 storage.Storage：将来换成 S3 时，只有这条能力消失，
// 其它工具完全不受影响。
type localPathStore interface {
	LocalPath(key string) (string, error)
}

// shellExec 在当前空间的文件目录里执行命令。
//
// 边界不靠提示词，靠 sandbox：全盘只读、只有空间 files/ 可写、默认无网络、
// 有超时和输出上限。命令失败（非零退出、超时）作为结果回给模型，
// 只有「沙箱不可用」「存储不可用」这类平台问题才返回 error。
func shellExec() Tool {
	return Tool{
		Name:       "shell_exec",
		Capability: "shell.exec",
		Description: "在当前空间的文件目录里执行 shell 命令并返回输出。命令运行在沙箱中：" +
			"系统其它位置只读，有超时与输出上限。" +
			"命令里出现 curl/wget 或脚本解释器（sh/python/node）时，需要用户确认联网后再执行；" +
			"装依赖（pip install / npm i / fnm install）不需要确认，直接装。" +
			"联网执行时沙箱只挂 .deps 安装目录，当前空间的其它文件不可见——下载、安装和读文件的操作要分开写，" +
			"不要写成一条 `curl ... && python ...`。" +
			"依赖与运行时请装进 .deps（FNM_DIR / PIP_TARGET / NPM_CONFIG_PREFIX 已指向那里），" +
			"例如 `pip install requests`、`npm i`、`fnm install 20`，装好后用 .deps 下的路径调用；" +
			"不要往 /usr、/etc 装东西，也不要试图改系统包管理器。" +
			"适合做文件统计、批量改名、跑本地脚本；要查外网信息请用 web_search。" +
			"命令返回非零退出码或超时时，先看 stderr 再调整，不要重复执行同一条失败命令。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的命令，由 sh -c 执行"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认 15，最大 25"}
  },
  "required": ["command"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 卡片徽标按命令判：`ls` 不应该和 `rm -rf` 显示成同一档。
		RiskFor: shellRisk,
		// 默认只有命中名单的命令才要确认，其余命令直接跑（沙箱已限定范围）。
		ShellApproval: DefaultApprovalCommands,
		SideEffects:   policy.SideEffectsWorkspace,
		Runtime:       "builtin",
		// 命令输出（尤其是测试日志）比文件内容更长，单独放宽回填额度
		MaxResultBytes: shellMaxResultBytes,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			var in struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			command := strings.TrimSpace(in.Command)
			if command == "" {
				return "", fmt.Errorf("command 不能为空")
			}
			if env.Shell == nil {
				return "", fmt.Errorf("命令执行不可用：平台没有可用的沙箱后端")
			}
			if env.Storage == nil || env.SpaceID == "" {
				return "", fmt.Errorf("空间存储不可用")
			}

			local, ok := env.Storage.(localPathStore)
			if !ok {
				return "", fmt.Errorf("当前存储不支持命令执行（需要可直接访问的文件系统）")
			}
			spaceRoot, err := local.LocalPath(env.Storage.SpaceKey(env.SpaceID))
			if err != nil {
				return "", fmt.Errorf("解析空间目录失败: %w", err)
			}
			workDir := filepath.Join(spaceRoot, workspaceFilesDir)
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				return "", fmt.Errorf("创建空间文件目录失败: %w", err)
			}

			timeout := defaultShellTimeout
			if in.TimeoutSeconds > 0 {
				timeout = time.Duration(in.TimeoutSeconds) * time.Second
			}
			if timeout > maxShellTimeout {
				timeout = maxShellTimeout
			}

			// 要不要出网由命令本身决定：curl / 脚本解释器 / 包管理器需要，
			// 其余（ls、grep、mv）不需要。出网时沙箱只挂 .deps，
			// 工作区的其它文件在命令里根本不存在。
			needsNet := shellNeedsNetwork(command)
			res, runErr := env.Shell.Run(ctx, sandbox.Spec{
				Cmd:     []string{"sh", "-c", command},
				Dir:     workDir,
				Mode:    sandbox.ModeWorkspaceWrite,
				Timeout: timeout,
				Network: needsNet,
				Env:     installEnv(workDir),
			})

			// 超时与输出超限是「命令没跑完」，属于结果的一部分，回给模型让它调整；
			// ctx 取消、启动失败这类才是平台错误。
			if runErr != nil &&
				!errors.Is(runErr, sandbox.ErrTimeout) &&
				!errors.Is(runErr, sandbox.ErrOutputTooLarge) {
				return "", runErr
			}
			return formatShellResult(res, runErr, timeout), nil
		},
	}
}

// InstallMirrors 是安装源镜像地址。
//
// 出网边界已经交给 NetworkPolicy，这张表只决定「装得快不快」：
// 按 region 换一整套源即可，不用重新编译。
type InstallMirrors struct {
	NPM  string // npm / pnpm / yarn
	PyPI string // pip / uv
	Node string // fnm 拉 node 二进制
	Go   string // GOPROXY
}

var installMirror = InstallMirrors{
	NPM: "https://registry.npmmirror.com",
	// 与 deploy/runner/Dockerfile 的 PYPI_INDEX 保持一致：
	// 镜像构建期装的包和运行时装来自同一源，出问题时才只有一个变量。
	PyPI: "https://mirrors.aliyun.com/pypi/simple",
	Node: "https://cdn.npmmirror.com/binaries/node",
	Go:   "https://goproxy.cn",
}

// SetInstallMirrors 覆盖安装源，字段为空则保留原值。
// 启动时按部署环境调用一次；不是每空间配置，因为镜像属于部署事实。
func SetInstallMirrors(m InstallMirrors) {
	if m.NPM != "" {
		installMirror.NPM = m.NPM
	}
	if m.PyPI != "" {
		installMirror.PyPI = m.PyPI
	}
	if m.Node != "" {
		installMirror.Node = m.Node
	}
	if m.Go != "" {
		installMirror.Go = m.Go
	}
}

// CurrentInstallMirrors 返回当前生效的安装源。
func CurrentInstallMirrors() InstallMirrors { return installMirror }

// InstallMirrorSources 把当前安装源摊成提示词要的 map。
//
// 提示词里那行「安装源：npm→…」和 installEnv 注入的环境变量必须是同一份值：
// 提示词说 A 源、命令实际走 B 源时，模型会照着提示词改配置，
// 排查的人则一路往「模型是不是没听话」上找。
func InstallMirrorSources() map[string]string {
	m := installMirror
	return map[string]string{
		"npm":  m.NPM,
		"pip":  m.PyPI,
		"node": m.Node,
		"go":   m.Go,
	}
}

// installEnv 把安装源与安装目录写进环境变量。
//
// 安装目录固定在 .deps 下：出网执行时沙箱只挂这一个子目录，
// 装进去的东西必须落在这里，否则下次调用就找不到了。
func installEnv(workDir string) []string {
	deps := sandbox.DepsDir(workDir)
	if deps == "" {
		return nil
	}
	m := installMirror
	py := filepath.Join(deps, "py")
	return []string{
		"FNM_DIR=" + filepath.Join(deps, "node"),
		"FNM_NODE_DIST_MIRROR=" + m.Node,
		"NPM_CONFIG_PREFIX=" + filepath.Join(deps, "npm"),
		"NPM_CONFIG_CACHE=" + filepath.Join(deps, "cache", "npm"),
		"NPM_CONFIG_REGISTRY=" + m.NPM,
		"PIP_TARGET=" + py,
		// PIP_TARGET 装的包不在 sys.path 里，必须补 PYTHONPATH，
		// 否则「装成功了但 import 不到」。
		"PYTHONPATH=" + py,
		"PIP_CACHE_DIR=" + filepath.Join(deps, "cache", "pip"),
		"PIP_INDEX_URL=" + m.PyPI,
		"GOPROXY=" + m.Go,
		// 缓存也放进 .deps：出网档位下 HOME 是私有 tmp，放那里等于每次重下。
		"XDG_CACHE_HOME=" + filepath.Join(deps, "cache"),
	}
}

// shellNetRe 命中的命令意味着「这条命令有出网可能」。
//
// 口径故意宽：脚本解释器只看得到文件名，看不到里面有没有 urllib。
// 多给一次网只是白跑一次，少给一次是功能直接坏掉。
var shellNetRe = regexp.MustCompile(`\b(curl|wget|nc|ncat|netcat|socat|telnet|ssh|scp|sftp|rsync|ftp|sh|bash|dash|zsh|ksh|python[0-9.]*|node|deno|bun|ruby|perl|php)\b`)

// shellPkgNet 是包管理器的 install 类子命令：没网时它们 100% 失败。
var shellPkgNet = []*regexp.Regexp{
	regexp.MustCompile(`\b(npm|pnpm|yarn)\s+(i|install|ci|add|update|up)\b`),
	regexp.MustCompile(`\b(pip|pip3)\s+(install|download)\b`),
	regexp.MustCompile(`\bpython[0-9.]*\s+-m\s+pip\s+(install|download)\b`),
	regexp.MustCompile(`\buv\s+(pip\s+install|sync|add|venv)\b`),
	regexp.MustCompile(`\bcargo\s+(build|install|add|fetch)\b`),
	regexp.MustCompile(`\bgo\s+(get|install|build|mod|generate)\b`),
	regexp.MustCompile(`\bfnm\s+(install|use)\b`),
}

// shellOfflineForms 是「长得像上面那些词、但确实不需要网」的形态。
//
// 不加这一层的话，`python3 -m venv .venv` 会被判成要出网，于是只挂 .deps，
// 结果虚拟环境建进 tmpfs，命令一结束就没了。
var shellOfflineForms = []*regexp.Regexp{
	regexp.MustCompile(`\bpython[0-9.]*\s+(-V|--version)\b`),
	regexp.MustCompile(`\bpython[0-9.]*\s+-m\s+venv\b`),
	regexp.MustCompile(`\bnode\s+(-v|--version)\b`),
	regexp.MustCompile(`\bgo\s+(version|env)\b`),
	regexp.MustCompile(`\b(npm|pnpm|yarn)\s+(run|ls|list|config|--version|-v)\b`),
}

// shellSplitPipeline 把命令按 `&&` `||` `;` `|` 切成子命令。
//
// 不处理引号：切错的后果只是多切一刀，每一段仍会被完整判断，不会漏掉危险命令；
// 而按 shell 语义解析引号要写一个小型词法分析器，收益不足以抵消维护成本。
func shellSplitPipeline(command string) []string {
	return strings.FieldsFunc(command, func(r rune) bool {
		return r == '&' || r == '|' || r == ';'
	})
}

func isPkgInstall(cmd string) bool {
	for _, re := range shellPkgNet {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func isOfflineForm(cmd string) bool {
	for _, re := range shellOfflineForms {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// shellNeedsNetwork 判断一条命令要不要出网。
//
// 这是唯一决定「沙箱能不能看到工作区」的开关：出网时只挂 .deps，
// 非出网时挂整个 Dir。判错的两种后果不对称——判多只是白给一次网，
// 判少是命令直接失败，所以口径偏宽（包管理器安装也算需要网）。
func shellNeedsNetwork(command string) bool {
	if strings.Contains(command, "/dev/tcp/") {
		// bash 内置，不需要任何外部二进制就能发请求。
		return true
	}
	for _, part := range shellSplitPipeline(command) {
		if isOfflineForm(part) {
			continue
		}
		if isPkgInstall(part) || shellNetRe.MatchString(part) {
			return true
		}
	}
	return false
}

// shellNeedsApproval 判断这条命令要不要人工确认。
//
// 判据比 shellNeedsNetwork 窄：只有「能任意外发数据」的命令才算数——
// 网络工具（curl / wget / nc / ssh ...）和脚本解释器（sh / python / node ...），
// 目标地址由命令内容决定，一条被注入的命令就能把会话内容拼进 URL 带出去。
//
// 包管理器安装（pip install / npm i / fnm install）虽然也要网，但它是
// 「装东西」不是「发东西」：默认终点由 PIP_INDEX_URL / NPM_CONFIG_REGISTRY
// 指向镜像源，`--index-url` 仍能覆盖，这里不做强制。理由是这条路的收益极低——
// 改源最多装进一个恶意包，而沙箱里既没有凭证也没有业务数据，拿不到有价值的东西。
// 放宽的收益很实际：每次装依赖都弹确认，用户会养成无脑点「同意」的习惯，
// 真正危险的命令反而被淹没。
func shellNeedsApproval(command string) bool {
	if strings.Contains(command, "/dev/tcp/") {
		return true
	}
	for _, part := range shellSplitPipeline(command) {
		// 安装与「确实不需要网」的形态先摘出去，剩下的部分再判：
		// `pip install node-fetch` 摘完是空的，`curl x && pip install y` 还剩 curl。
		if isOfflineForm(part) || isPkgInstall(part) {
			continue
		}
		if shellNetRe.MatchString(part) {
			return true
		}
	}
	return false
}

// shellReadOnlyCommands 是「静态可判定为只读」的命令名单，只用于展示。
//
// 口径故意窄：白名单外的命令（解释器 python/node/sh、mv/cp/tee/sed/awk、
// mysql/psql 这类客户端）一律按写入展示——把 `mv` 显示成「只读」比把
// `echo hi > f` 显示成「写入」糟糕得多。
var shellReadOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "fd": true,
	"find": true, "du": true, "df": true, "stat": true, "file": true,
	"pwd": true, "echo": true, "printf": true, "date": true, "uname": true,
	"whoami": true, "id": true, "which": true, "type": true, "tree": true,
	"diff": true, "cmp": true, "cut": true, "tr": true, "nl": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true, "column": true,
	"od": true, "hexdump": true, "jq": true, "sort": true,
}

// shellGitReadOnly 是 git 的只读子命令；`git commit` / `push` / `checkout`
// 这些会改动仓库或远端，按写入展示。
var shellGitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true,
	"rev-parse": true, "ls-files": true, "ls-tree": true, "describe": true,
	"shortlog": true, "cat-file": true, "grep": true,
}

var (
	// shellDevNull 是「写 /dev/null」的重定向：不落地成文件，不算写操作。
	shellDevNull = regexp.MustCompile(`(?:[0-9]|&)?>>?\s*/dev/null\b`)
	// 这两个参数会把白名单里的读命令变成写：`find -delete/-exec`、`sort -o`。
	shellFindWrites = regexp.MustCompile(`\s-(delete|exec|execdir|ok|okdir)\b`)
	shellSortWrites = regexp.MustCompile(`\s(--output|-o)\b`)
)

// shellRisk 按命令给出这一次调用的展示等级。
//
// 判据与审批同源（复用 MatchApprovalCommand 的名单与动态形态），但**只影响
// 徽标**：审批该弹还是弹，空间写权限也仍按声明的 L3 判。返回 0 表示判不出来，
// 由 Tool.EffectiveRisk 退回声明等级。
func shellRisk(t Tool, args json.RawMessage) int {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return 0
	}
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return 0
	}
	// 重定向写文件、命令替换、xargs、嵌套 shell 一律按写入：这些形态静态判不准。
	if shellWritesByRedirect(command) {
		return policy.RiskWrite
	}
	if hit := MatchApprovalCommand(command, t.ShellApproval); hit != "" {
		return policy.RiskWrite
	}
	segments := shellSegments(command)
	if len(segments) == 0 {
		return 0
	}
	for _, seg := range segments {
		if !shellSegmentReadOnly(seg) {
			return policy.RiskWrite
		}
	}
	return policy.RiskReadOnly
}

// shellWritesByRedirect 判断命令里是否有落地成文件的重定向（`>` / `>>`）。
//
// `>/dev/null` 与 `>&1` / `>&2` 不算：前者没有数据落地，后者写的是已打开的句柄。
func shellWritesByRedirect(command string) bool {
	rest := shellDevNull.ReplaceAllString(command, " ")
	for i := 0; i < len(rest); i++ {
		if rest[i] != '>' {
			continue
		}
		j := i + 1
		if j < len(rest) && rest[j] == '>' {
			j++
		}
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		if j < len(rest) && rest[j] == '&' {
			i = j
			continue
		}
		return true
	}
	return false
}

// shellSegments 按管道 / 顺序分隔符切段。
//
// 只认真正起分隔作用的符号：引号里的 `|`、转义的 `\|`（grep 的或模式）、
// `&&` / `||` 的连写都不产生新段。按字节硬切会把 `grep "a\|b" f` 切成两段，
// 后一段的首词是 `b"`，于是整条只读命令被误判成写入。
func shellSegments(command string) []string {
	out := make([]string, 0, 4)
	start := 0
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			switch {
			case c == '\\' && quote == '"':
				i++ // 双引号里的转义：下一个字符不参与判定
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			i++ // 转义的分隔符不是分隔符（grep 的 \|）
		case ';', '&', '|', '\n', '\r':
			if s := strings.TrimSpace(command[start:i]); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if s := strings.TrimSpace(command[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// shellSegmentReadOnly 判断单段命令是不是纯读：命令名在只读名单里，
// 且没有把读变成写的参数（`find -exec`、`sort -o`）。
func shellSegmentReadOnly(seg string) bool {
	fields := strings.Fields(seg)
	for i := 0; i < len(fields); i++ {
		token := strings.Trim(fields[i], `"'()`)
		if token == "" {
			continue
		}
		// VAR=value 前缀与 wrapper 不是命令名，继续往后找（与审批判定同一口径）
		if idx := strings.IndexByte(token, '='); idx > 0 && !strings.ContainsAny(token[:idx], "/.-") {
			continue
		}
		if shellWrappers[token] {
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		name := baseName(token)
		if name == "git" {
			sub := shellFirstArg(fields, i+1)
			// 只有选项、没有子命令（`git --version`）不会改任何东西
			return sub == "" || shellGitReadOnly[sub]
		}
		if !shellReadOnlyCommands[name] {
			return false
		}
		switch name {
		case "find":
			return !shellFindWrites.MatchString(seg)
		case "sort":
			return !shellSortWrites.MatchString(seg)
		}
		return true
	}
	return false
}

// shellFirstArg 取命令名之后的第一个非选项参数（`git status` 的 `status`）。
// 取不到就是判不出来，按非只读处理。
func shellFirstArg(fields []string, from int) string {
	for i := from; i < len(fields); i++ {
		token := strings.Trim(fields[i], `"'()`)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		return token
	}
	return ""
}

// formatShellResult 把执行结果整理成模型能直接读的文本。
// 先给结论（退出码 / 耗时 / 终止原因），再给输出，避免模型从一大段日志里找结论。
func formatShellResult(res sandbox.Result, runErr error, timeout time.Duration) string {
	var b strings.Builder
	switch {
	case errors.Is(runErr, sandbox.ErrTimeout):
		fmt.Fprintf(&b, "命令超时（%s），已终止整棵进程树\n", timeout)
	case errors.Is(runErr, sandbox.ErrOutputTooLarge):
		b.WriteString("命令输出超过上限，已终止\n")
	case runErr != nil:
		fmt.Fprintf(&b, "执行失败：%v\n", runErr)
	default:
		fmt.Fprintf(&b, "退出码 %d，耗时 %s\n", res.ExitCode, res.Duration.Round(time.Millisecond))
	}

	write := func(label, content string) {
		content = strings.TrimRight(content, "\n")
		if content == "" {
			return
		}
		fmt.Fprintf(&b, "\n[%s]\n%s\n", label, content)
	}
	write("stdout", res.Stdout)
	write("stderr", res.Stderr)
	if b.Len() > 0 && !strings.Contains(b.String(), "\n[") && runErr == nil {
		b.WriteString("（无输出）\n")
	}
	return b.String()
}
