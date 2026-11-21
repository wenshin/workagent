package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
		Description: "在当前空间的工作目录里执行 shell 命令并返回输出。命令运行在沙箱中：" +
			"系统其它位置只读，有超时与输出上限。网络按命令形态决定，调用前先看清楚：" +
			"curl/wget/nc/ssh 这类网络工具会先请用户确认，用户拒绝就不执行。" +
			"sh/bash/python/node/deno/go 这类脚本默认不联网，访问外网会失败；确实需要联网时传 `network: true`，" +
			"会先请用户确认，批准后才联网执行。" +
			"其余命令（ls/git/make/npm i/pip install/apt install）默认有网且不弹确认。" +
			"下载与解析可以连着写，例如 `curl -o data.json <url> && python parse.py`。" +
			"容器闲置会被回收，系统目录回到基础镜像：缺什么直接装，包在本地缓存里，重跑很快。" +
			"Python 已预置 .pkg/venv 并在 PATH 里，直接 `pip install` 即可；" +
			"npm 全局安装已指向 .pkg/npm，直接 `npm i -g` 即可，两者重建后都还在。" +
			"系统级工具（apt install）装进可写层，重建后会消失，需要时再装一次。" +
			"适合做文件统计、批量改名、跑本地脚本；要查外网信息请用 web_search。" +
			"命令返回非零退出码或超时时，先看 stderr 再调整，不要重复执行同一条失败命令。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的命令，由 sh -c 执行"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认 15，最大 25"},
    "network": {"type": "boolean", "description": "脚本需要联网时置 true，会先请用户确认、批准后才联网执行。只对 sh/python/node/deno/go 这类脚本有意义；curl/wget 等网络工具本来就会先确认，其他命令默认有网"}
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
				// Network 是调用方替脚本档声明的「这次需要联网」。
				// 它不是权限开关：声明之后仍要用户批准，批准之前不联网。
				Network bool `json:"network"`
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

			// 网络按命令形态分三档，判据只在 shellnet.go：
			//   网络工具  有网 + 先审批
			//   脚本     默认无网，声明 network=true 才有网 + 审批
			//   其他     有网，不审批
			//
			// 审批已经在 Invoker 里做完（ApprovalFor 读的是同一个 ShellNetPlan），
			// 这里只把结论落到沙箱上。两处各判一次必然漂移，而且「没批就联网」
			// 是静默的——用户不会收到任何提示。
			plan := ShellNetPlan(command, in.Network)
			mode := sandbox.ModeWorkspaceWrite
			if shellNeedsSystemWrite(command) {
				mode = sandbox.ModeSystemWrite
			}
			if err := CheckSpaceQuota(spaceRoot); err != nil {
				return "", err
			}
			ensureVenv(ctx, spaceRoot)
			res, runErr := env.Shell.Run(ctx, sandbox.Spec{
				Cmd:     []string{"sh", "-c", command},
				Dir:     workDir,
				Mode:    mode,
				Timeout: timeout,
				Network: plan.NeedsNetwork(),
				Env:     InstallEnv(spaceRoot, workDir),
			})

			// 超时与输出超限是「命令没跑完」，属于结果的一部分，回给模型让它调整；
			// ctx 取消、启动失败这类才是平台错误。
			if runErr != nil &&
				!errors.Is(runErr, sandbox.ErrTimeout) &&
				!errors.Is(runErr, sandbox.ErrOutputTooLarge) {
				return "", runErr
			}
			out := formatShellResult(res, runErr, timeout)
			// 脚本默认无网：失败原因是网络时给一句可执行的下一步。
			// 不给的话模型只能从 EAI_AGAIN 这类错误里猜「是不是沙箱的问题」。
			if plan.Mode == ShellNetOff && looksLikeNetworkFailure(res.Stdout, res.Stderr) {
				out += "\n[提示] 这次执行没有联网（脚本默认不联网）。" +
					"如果失败与网络有关，确认后带 `network: true` 重新调用，会先请用户确认。\n"
			}
			return out, nil
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

// CacheRoot 是租户级缓存根目录，部署时挂载，所有空间共享。
//
// 只放"重装时能命中"的东西：apt 索引与 deb、npm / pip / go 的下载缓存。
// 不放装好的包——那些属于空间目录（.pkg），共享会互相覆盖版本。
var (
	cacheRootMu sync.RWMutex
	cacheRoot   = "/cache"
)

// SetCacheRoot 覆盖租户缓存根目录，启动时按部署环境调用一次。
func SetCacheRoot(p string) {
	p = strings.TrimSpace(p)
	cacheRootMu.Lock()
	if p != "" && filepath.IsAbs(p) {
		cacheRoot = filepath.Clean(p)
	}
	cacheRootMu.Unlock()
}

// CacheRoot 返回当前租户缓存根目录。
func CacheRoot() string {
	cacheRootMu.RLock()
	defer cacheRootMu.RUnlock()
	return cacheRoot
}

// installEnv 把安装位置与下载缓存写进环境变量。
//
// 两类目录分工明确：
//   - .pkg 在空间目录下，装进去的东西 Pod 重建后还在（npm 全局包、venv）
//   - /cache 是租户级缓存，只保证"重装时不用下载"，不保证"装好了"
//
// pip 用 venv 不用 PIP_TARGET：后者只往目录塞包，console script、
// 升级、卸载都不正常，长期用的环境就该是 venv。
//
// PATH 必须在这里拼好：--clearenv 之后沙箱只继承 baseEnvKeys 里的变量，
// 在 shell 里 export 影响不到下一条命令。
func InstallEnv(spaceRoot, workDir string) []string {
	if spaceRoot == "" {
		spaceRoot = filepath.Dir(workDir)
	}
	pkg := filepath.Join(spaceRoot, ".pkg")
	venv := filepath.Join(pkg, "venv")
	npm := filepath.Join(pkg, "npm")
	cache := CacheRoot()
	m := installMirror
	path := strings.Join([]string{
		filepath.Join(venv, "bin"),
		filepath.Join(npm, "bin"),
	}, ":") + ":" + sandbox.FallbackPath()
	return []string{
		"WA_SPACE_DIR=" + spaceRoot,
		// npm 全局包装进空间目录，Pod 重建后还在。
		"NPM_CONFIG_PREFIX=" + npm,
		"NPM_CONFIG_CACHE=" + filepath.Join(cache, "npm"),
		"NPM_CONFIG_REGISTRY=" + m.NPM,
		// venv 激活后 pip 装进 venv，同时天然绕开 PEP 668（不动系统 python）。
		"VIRTUAL_ENV=" + venv,
		"PIP_CACHE_DIR=" + filepath.Join(cache, "pip"),
		"PIP_INDEX_URL=" + m.PyPI,
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"GOPROXY=" + m.Go,
		"GOMODCACHE=" + filepath.Join(cache, "go", "mod"),
		"FNM_DIR=" + filepath.Join(cache, "fnm"),
		"FNM_NODE_DIST_MIRROR=" + m.Node,
		// HOME 是每次调用的私有 tmp，缓存放那里等于每次重下。
		"XDG_CACHE_HOME=" + filepath.Join(cache, "xdg"),
		"PATH=" + path,
	}
}

// ensureVenv 保证 .pkg/venv 存在，建一次就够。
//
// 失败不阻断命令：没有 venv 时 pip 会落到系统 python 上，
// 报错信息是 PEP 668，用户照样能看懂。真正要避免的是
// "每条命令都试一次 python3 -m venv" 的开销。
func ensureVenv(ctx context.Context, spaceRoot string) {
	venv := filepath.Join(spaceRoot, ".pkg", "venv")
	if _, err := os.Stat(filepath.Join(venv, "bin", "python")); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(spaceRoot, ".pkg"), 0o755); err != nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "python3", "-m", "venv", venv).CombinedOutput()
	if err != nil {
		slog.Warn("创建空间 venv 失败", "dir", venv, "err", err, "out", strings.TrimSpace(string(out)))
	}
}

// shellSysWriteRe 命中的命令要往系统目录装东西，必须放开 /usr、/var/lib/dpkg。
//
// 只有 apt / dpkg 在这里：pip 装进 .pkg/venv、npm 全局装进 .pkg/npm，
// 两者都在空间目录里，不需要系统写权限。
// 不放开的表现是 dpkg 报 "Read-only file system"、apt 报
// "Unable to lock the administration directory"，都像权限问题其实是挂载问题。
var shellSysWriteRe = []*regexp.Regexp{
	regexp.MustCompile(`\bapt(-get)?\s+(install|reinstall|upgrade|dist-upgrade|purge|remove|update)\b`),
	regexp.MustCompile(`\bdpkg\s+(-i|--install|--configure|-r|--remove)\b`),
}

// NeedsSystemWrite 供其它执行体（插件开发会话）复用同一套判据，
// 避免两处规则各写一遍、慢慢漂移。
func NeedsSystemWrite(cmd string) bool { return shellNeedsSystemWrite(cmd) }

func shellNeedsSystemWrite(cmd string) bool {
	for _, re := range shellSysWriteRe {
		if re.MatchString(cmd) {
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
