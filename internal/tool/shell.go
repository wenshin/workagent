package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
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
		// 描述长度受插件清单约束（plugin.maxDescRunes=500），内置工具也要过发布校验：
		// 超了会在启动时发布失败，工具直接消失，所以这里只留模型判断「何时用」必需的信息。
		Description: "在当前空间的工作目录里执行 shell 命令并返回输出。命令运行在沙箱中：" +
			"系统其它位置只读，有超时与输出上限。网络由命令内容自动决定，没有开关参数：" +
			"只装依赖（pip install / npm i / apt install）直接联网；" +
			"只跑脚本（sh/python/node/deno/go）默认不联网，因断网失败时会把「联网重跑」交给用户确认；" +
			"装依赖和脚本同时出现先请用户确认，拒绝仍按无网执行；" +
			"curl/wget/nc/ssh 这类网络工具一律先确认，拒绝就不执行。" +
			"下载与解析可以连着写，例如 `curl -o data.json <url> && python parse.py`。" +
			"容器闲置会被回收，系统目录回到基础镜像：缺什么直接装，包在缓存里。" +
			"Python venv、npm 全局目录在 PATH 里（首次自动建），直接 `pip install` / `npm i -g`，重建后还在；" +
			"apt install 装进可写层，重建后会消失。" +
			"适合跑本地脚本与文件统计；查外网请用 web_search。" +
			"命令返回非零退出码或超时时先看 stderr 再调整，不要重复执行同一条失败命令。",
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
		// 网络判据挂在工具上：Invoker 据此决定问答、拒绝后的处置与升级重跑。
		NetPlan: func(args json.RawMessage) ShellNetPolicy {
			return ShellNetPlan(shellCommandOf(args))
		},
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
			spaceKey := env.Storage.SpaceKey(env.SpaceID)
			spaceRoot, err := local.LocalPath(spaceKey)
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

			// 网络按命令内容分四类，判据只在 shellnet.go：
			//   只有装依赖           有网，不确认
			//   只有脚本            无网；失败且像网络问题时由 Invoker 申请联网重跑
			//   装依赖 + 脚本        先确认；拒绝按无网执行
			//   出现网络工具         先确认；拒绝不执行
			//
			// 审批已经在 Invoker 里做完（ApprovalFor 与这里的 plan 读同一份判据），
			// env.NetGrant 就是它的结论：拿到许可才落 Spec.Network=true。
			plan := ShellNetPlan(command)
			mode := sandbox.ModeWorkspaceWrite
			if shellNeedsSystemWrite(command) {
				mode = sandbox.ModeSystemWrite
			}
			if err := CheckSpaceQuota(spaceRoot); err != nil {
				return "", err
			}
			pkgDir := filepath.Join(spaceRoot, ".pkg")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				return "", fmt.Errorf("创建空间安装目录失败: %w", err)
			}
			// 宿主路径只出现在 Source 里：它留在控制面，过线的是 Key。
			// 沙箱里统一是 /workspace 与 /pkg，命令输出、报错、环境变量都不会
			// 暴露 data/storage/spaces/<id> 这套结构。
			res, runErr := env.Shell.Run(ctx, sandbox.Spec{
				Cmd: []string{"sh", "-c", venvBootstrap + command},
				Dir: sandbox.WorkspaceDir,
				Mounts: []sandbox.Mount{
					{Source: workDir, Key: spaceKey + "/" + workspaceFilesDir, Target: sandbox.WorkspaceDir, Write: true},
					{Source: pkgDir, Key: spaceKey + "/.pkg", Target: sandbox.PkgDir, Write: true},
					{Source: CacheRoot(), Root: sandbox.RootCache, Target: sandbox.CacheDir, Write: true},
				},
				Mode:    mode,
				Timeout: timeout,
				Network: plan.Mode == ShellNetOn || (plan.NeedsApproval() && env.NetGrant),
				Env:     InstallEnv(),
			})

			// 超时与输出超限是「命令没跑完」，属于结果的一部分，回给模型让它调整；
			// ctx 取消、启动失败这类才是平台错误。
			if runErr != nil &&
				!errors.Is(runErr, sandbox.ErrTimeout) &&
				!errors.Is(runErr, sandbox.ErrOutputTooLarge) {
				return "", runErr
			}
			out := formatShellResult(res, runErr, timeout)
			// 纯脚本无网执行、退出码非零且错误像网络不可达：交给 Invoker 去申请联网重跑。
			// 这里只判断「要不要问」，问完批不批不归工具管——执行阶段套着 30s 墙钟，
			// 在工具里等人点按钮会先超时。
			if plan.Escalate && !env.NetGrant && res.ExitCode != 0 &&
				looksLikeNetworkFailure(res.Stdout, res.Stderr) {
				return out, errNetNeeded{}
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
	PyPI string // pip
	Node string // mise 拉 node 二进制
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
// InstallEnv 返回沙箱内的安装环境变量。
//
// 一律用沙箱内路径（/pkg、/cache）：宿主目录只出现在挂载表里，
// 环境变量、命令输出与报错里都看不到 STORAGE_ROOT。
func InstallEnv() []string {
	pkg := sandbox.PkgDir
	venv := path.Join(pkg, "venv")
	npm := path.Join(pkg, "npm")
	cache := sandbox.CacheDir
	mise := path.Join(cache, "mise")
	m := installMirror
	// mise 装的运行时通过 shims 暴露成普通命令：`mise use -g node@22` 会写 shims，
	// PATH 里有它就能直接用 node，不需要 source 任何脚本（shell_exec 是独立的
	// 非交互 sh -c，靠 shell 集成改 PATH 的做法在这里不成立）。
	searchPath := strings.Join([]string{
		path.Join(venv, "bin"),
		path.Join(npm, "bin"),
		path.Join(mise, "shims"),
	}, ":") + ":" + sandbox.FallbackPath()
	return []string{
		"WA_SPACE_DIR=" + sandbox.WorkspaceDir,
		// npm 全局包装进空间目录，Pod 重建后还在。
		"NPM_CONFIG_PREFIX=" + npm,
		"NPM_CONFIG_CACHE=" + path.Join(cache, "npm"),
		"NPM_CONFIG_REGISTRY=" + m.NPM,
		// venv 激活后 pip 装进 venv，同时天然绕开 PEP 668（不动系统 python）。
		"VIRTUAL_ENV=" + venv,
		"PIP_CACHE_DIR=" + path.Join(cache, "pip"),
		"PIP_INDEX_URL=" + m.PyPI,
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		// 兜底：venvBootstrap 建 venv 失败时，pip 仍要能装进可写层。
		// 不设的话系统 python 是 externally-managed，pip 一律被 PEP 668 拒绝，
		// 而报错里只有一个 hint 链接，看起来像权限问题。
		"PIP_BREAK_SYSTEM_PACKAGES=1",
		"GOPROXY=" + m.Go,
		"GOMODCACHE=" + path.Join(cache, "go", "mod"),
		// mise 的数据 / 配置必须在这里给：它们依赖缓存根，父进程不知道。
		// HOME 是每次调用的私有 tmp，放默认位置等于每次重装、全局版本配置丢失。
		"MISE_DATA_DIR=" + mise,
		"MISE_CACHE_DIR=" + path.Join(mise, "cache"),
		"MISE_CONFIG_DIR=" + path.Join(mise, "config"),
		"MISE_NODE_MIRROR=" + m.Node,
		// python 走预编译包，不编译源码（否则一次 mise use python@3.12 要几分钟）。
		"MISE_PYTHON_COMPILE=false",
		"MISE_YES=1",
		// 沙箱里 agent 可能写 mise.toml；信任提示只会让它卡住，且容器本身就是一次性的。
		"MISE_TRUSTED_CONFIG_PATHS=/",
		// HOME 是每次调用的私有 tmp，缓存放那里等于每次重下。
		"XDG_CACHE_HOME=" + path.Join(cache, "xdg"),
		"PATH=" + searchPath,
	}
}

// venvBootstrap 在沙箱内保证 /pkg/venv 属于当前执行体，拼在用户命令之前。
//
// 为什么必须在沙箱里建：venv 的 bin/pip 是带 shebang 的脚本，指向创建它的那个
// python。控制面建出来的 venv（macOS 的 homebrew python、或另一个镜像的 python）
// 进容器后 shebang 悬空，PATH 里的 /pkg/venv/bin/pip3 执行失败，sh 会静默回落到
// /usr/bin/pip3，再被 PEP 668 拒绝——回执只剩一句「No matching distribution found」，
// 用户看到的是「装了两次都失败」。
//
// 判据用 pyvenv.cfg 的 home：那是创建时的解释器目录，容器里不存在就说明这个 venv
// 不属于当前镜像，删掉重建（一次性，约 2s）。正常运行只有 sed + test 两个进程。
//
// 失败不阻断：PIP_BREAK_SYSTEM_PACKAGES=1 兜底，pip 至少能装进可写层。
// 不在沙箱里（本地调试、单测）时 WA_SPACE_DIR 不存在，整段跳过，绝不在宿主上动 /pkg。
const venvBootstrap = `if [ -d "$WA_SPACE_DIR" ] && [ -d /pkg ]; then ` +
	`vh=$(sed -n 's/^home *= *//p' /pkg/venv/pyvenv.cfg 2>/dev/null); ` +
	`if [ -z "$vh" ] || [ ! -d "$vh" ]; then ` +
	`rm -rf /pkg/venv; python3 -m venv /pkg/venv >/dev/null 2>&1 || true; fi; ` +
	`fi; `

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
