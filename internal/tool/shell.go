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
	"github.com/workagent/auto-agent/internal/spacelog"
)

const (
	// shell 的墙钟由这个工具自己声明（Tool.TimeoutFor），Invoker 只负责取用，
	// 不再有一套「外层 30s」把长命令掐断——否则报告出来的超时会指向错误的层。
	//
	// 默认两分钟：真实命令里最慢的是装依赖，同镜像实测 `pip install` 一个 SDK
	// 要 195s，原来的 15s 连 venv 都建不完。上限只是「最多等多久」，
	// 秒级返回的命令不会因此变慢。
	defaultShellTimeout = 120 * time.Second
	// installShellTimeout 是纯装依赖（pip install / npm i / deno cache）的默认值：
	// 下载 + 解包 + 链接，三百秒是常见 SDK 的量级。
	installShellTimeout = 300 * time.Second
	// maxShellTimeout 是模型能声明的上限。再长就该走平台任务：一轮回答的总墙钟
	// 默认两小时，单条命令不该吃掉四分之一。
	maxShellTimeout = 30 * time.Minute
	// 命令输出比文件内容更碎，给 16KB；再多对模型也没有价值。
	shellMaxResultBytes = 16 * 1024
)

// shellPluginDirName 是空间工作区下存放插件目录的子目录名。
//
// 与 plugin.DraftDir 必须是同一个值，但这里不能 import internal/plugin：
// 依赖方向是 plugin → tool，反向引用会成环。
const shellPluginDirName = "plugins"

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
		//
		// need_network 的说法要落在「命令自身要不要外网」上：写成「跑脚本就得声明
		// true」时，模型对任何脚本都会二选一——要么一个都不写（默认无网，脚本自己
		// 发请求就必然失败、再被问一次），要么处处都写（多出一堆没必要的确认）。
		Description: "在当前空间的沙箱里执行命令：只有 /workspace 可写，有超时与输出上限。" +
			"工作区跨调用保留；/tmp 与 $HOME 每次都全新，产物写到当前目录。" +
			"plugin=<slug> 只挂该插件目录到 /plugin 并以其为 cwd（装插件依赖用，如 deno cache）。" +
			"网络默认不通，need_network 表示「这条命令自身要访问外网」：" +
			"pip install / npm i / apt install 是下载，不用声明；" +
			"声明 true 且含脚本（python / node / deno / go run）或网络工具（curl / wget / nc / ssh）时先确认，拒绝即不执行；" +
			"不声明按无网执行；脚本联网失败会再问一次是否联网重跑，所以脚本要联网就一开始声明 true；" +
			"纯本地动作（列目录、读写、统计、编译）不要声明。复合命令连着写：curl -o a.json <url> && python parse.py。" +
			"venv 与 npm 全局目录已在 PATH 里。查外网资料用 web_search。" +
			"非零退出先看 stderr 再改，不要重复执行同一条失败命令。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的命令，由 sh -c 执行"},
    "need_network": {"type": "boolean", "description": "这条命令自身是否要访问外网：抓页面、调 API、下载没装的东西、脚本里会发请求时为 true。纯本地处理（列目录、读写文件、统计、编译、不联网的测试）为 false（默认）。pip install / npm i / apt install 不看这个参数"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认 120（纯装依赖 300），最大 1800。装依赖、跑测试这类慢命令按预期耗时写，别让它被默认值截断"},
    "plugin": {"type": "string", "description": "插件 slug：只挂 plugins/<slug> 到 /plugin 并以它为工作目录，工作区不可见。给插件装依赖时用（deno cache --no-config --lock=deno.lock --node-modules-dir=auto --vendor index.ts），同时要声明 need_network=true——deno cache 按脚本算，不在「安装类免声明」里"}
  },
  "required": ["command"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 卡片徽标按命令判：`ls` 不应该和 `rm -rf` 显示成同一档。
		RiskFor: shellRisk,
		// 等级之外再给一个动作标签：脚本、出网、写文件、删文件不该都写成「写入」。
		RiskLabelFor: shellRiskLabel,
		// 默认只有命中名单的命令才要确认，其余命令直接跑（沙箱已限定范围）。
		ShellApproval: DefaultApprovalCommands,
		SideEffects:   policy.SideEffectsWorkspace,
		Runtime:       "builtin",
		// 命令输出（尤其是测试日志）比文件内容更长，单独放宽回填额度
		MaxResultBytes: shellMaxResultBytes,
		// 网络判据挂在工具上：Invoker 据此决定问不问、以及要不要升级重跑。
		// 模型声明的 need_network 与命令原文一起进判据，两处各读一份就会漂移。
		NetPlan: func(args json.RawMessage) ShellNetPolicy {
			return ShellNetPlan(shellCommandOf(args), shellNeedNetwork(args))
		},
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			var in struct {
				Command        string `json:"command"`
				NeedNetwork    bool   `json:"need_network"`
				TimeoutSeconds int    `json:"timeout_seconds"`
				Plugin         string `json:"plugin"`
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

			// 插件作用域：只挂 plugins/<slug>，工作区整体不可见。
			// 装依赖的脚本（npm 的 postinstall 尤其）是这个平台里最不该看见业务数据
			// 的东西，而它恰好又是唯一需要联网的一步——把可见面压到最小。
			pluginDir, pluginKey, err := shellPluginScope(spaceKey, workDir, in.Plugin)
			if err != nil {
				return "", err
			}

			timeout := shellTimeout(command, in.TimeoutSeconds)

			// 网络由「模型声明 + 命令内容」一起定，判据只在 shellnet.go：
			//   只装依赖             有网，不确认（与声明无关）
			//   声明 true + 脚本/网络工具  先确认；拒绝不执行
			//   未声明                无网；脚本失败且像网络问题时由 Invoker 申请联网重跑
			//
			// 审批已经在 Invoker 里做完（ApprovalFor 与这里的 plan 读同一份判据），
			// env.NetGrant 就是它的结论：拿到许可才落 Spec.Network=true。
			plan := ShellNetPlan(command, in.NeedNetwork)
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
			//
			// /pkg 与 /cache 始终挂：InstallEnv 把 venv、npm 全局目录与下载缓存
			// 都指到这两个沙箱路径上，不挂的话装依赖会写到一个不存在的目录。
			mounts := []sandbox.Mount{
				{Source: pkgDir, Key: spaceKey + "/.pkg", Target: sandbox.PkgDir, Write: true},
				{Source: CacheRoot(), Root: sandbox.RootCache, Target: sandbox.CacheDir, Write: true},
			}
			dir, cmd := sandbox.WorkspaceDir, venvBootstrap+command
			if pluginDir != "" {
				// 插件作用域下不建 venv：依赖由 deno 管，venv 只会白等两秒。
				mounts = append(mounts, sandbox.Mount{
					Source: pluginDir, Key: pluginKey, Target: sandbox.PluginDir, Write: true,
				})
				dir, cmd = sandbox.PluginDir, command
			} else {
				mounts = append(mounts, sandbox.Mount{
					Source: workDir, Key: spaceKey + "/" + workspaceFilesDir, Target: sandbox.WorkspaceDir, Write: true,
				})
			}
			res, runErr := env.Shell.Run(ctx, sandbox.Spec{
				// 空间容器：每空间一个，空闲超时由执行面回收。
				Scope:   sandbox.Scope{Space: env.SpaceID, Kind: sandbox.KindShell},
				Cmd:     []string{"sh", "-c", cmd},
				Dir:     dir,
				Mounts:  mounts,
				Mode:    mode,
				Timeout: timeout,
				// NetGrant 是 Invoker 的审批结论（含「联网重跑」的第二次执行），
				// 声明了要网但被拒时这条命令根本不会走到这里。
				Network: plan.Mode == ShellNetOn || env.NetGrant,
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
			// 运行日志留证：命令、退出码、耗时与输出尾部。回执会被上下文预算截断，
			// 事后想复核「那条命令到底输出了什么」只能靠这份。
			// 顺序上先写日志再决定是否申请联网重跑：重跑是一次真实执行，也该各留一条。
			env.SpaceLog.Write(ctx, spacelog.Entry{
				SpaceID:   env.SpaceID,
				Kind:      "shell",
				Tool:      "shell_exec",
				Status:    shellLogStatus(res, runErr),
				Latency:   res.Duration,
				SessionID: env.SessionID,
				RunID:     env.RunID,
				UserID:    env.UserID,
				Command:   Mask(command),
				ExitCode:  res.ExitCode,
				Output:    Mask(res.Stdout + "\n" + res.Stderr),
			})
			// 没声明联网、无网执行失败且错误像网络不可达：交给 Invoker 去申请联网重跑。
			// 这里只判断「要不要问」，问完批不批不归工具管——执行阶段套着工具自己的
			// 墙钟（两分钟起），在工具里等人点按钮只会先被自己的超时掐断。
			if plan.Escalate && !env.NetGrant && res.ExitCode != 0 &&
				looksLikeNetworkFailure(res.Stdout, res.Stderr) {
				return out, errNetNeeded{}
			}
			return out, nil
		},
		// 墙钟由 shell 自己给：装依赖与跑测试的合理耗时差两个数量级，
		// 一个全局常量只能取最小值（于是长命令全被截断）或最大值（于是挂住的
		// 文件工具也要等半小时）。Invoker 只负责取用与外层夹逼。
		TimeoutFor: func(_ Tool, args json.RawMessage) time.Duration {
			var in struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			_ = json.Unmarshal(args, &in)
			return shellTimeout(strings.TrimSpace(in.Command), in.TimeoutSeconds)
		},
	}
}

// shellTimeout 是这条命令的墙钟：模型给了就用它的，没给按命令类别给默认值。
//
// 判据复用 shellnet.go 的分类（同一个入口、同一份命令原文），不另起一套：
// 「装依赖」和「跑脚本」的合理耗时本来就差一个数量级。
func shellTimeout(command string, seconds int) time.Duration {
	if seconds > 0 {
		d := time.Duration(seconds) * time.Second
		if d > maxShellTimeout {
			return maxShellTimeout
		}
		return d
	}
	if k := classifyCommand(command); k.install && !k.script && !k.netTool {
		return installShellTimeout
	}
	return defaultShellTimeout
}

// shellPluginScope 解析 plugin 参数，返回插件目录的真实路径与存储 key。
//
// 空参数返回空路径（走整工作区那条老路）。给了 slug 就只允许它落在
// plugins/<slug> 下：slug 是模型写的，必须当成不可信输入——带 ../ 的 slug
// 能把挂载点指到空间里任何一个目录，等于绕开「只挂插件目录」这条边界。
func shellPluginScope(spaceKey, workDir, slug string) (string, string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", "", nil
	}
	if !shellPluginSlug.MatchString(slug) {
		return "", "", fmt.Errorf("plugin 必须是插件 slug（字母、数字、- 与 _）: %q", slug)
	}
	abs := filepath.Join(workDir, shellPluginDirName, slug)
	if !strings.HasPrefix(abs, workDir+string(filepath.Separator)) {
		return "", "", fmt.Errorf("插件目录越出工作区: %q", slug)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", "", fmt.Errorf("插件目录不存在: %s/%s（先用 file_write 写 plugin.json 与入口文件）",
			shellPluginDirName, slug)
	}
	return abs, path.Join(spaceKey, workspaceFilesDir, shellPluginDirName, slug), nil
}

// shellPluginSlug 是插件 slug 的字符集：与清单校验保持一致，只用来挡住路径分隔符。
var shellPluginSlug = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

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
	// cd 只是换目录：`cd x && ls` 不该因为 cd 被判成写入
	"cd": true, "pushd": true, "popd": true,
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
	// shellFindDelete 单独认删除：`find . -name '*.tmp' -delete` 是删除动作。
	shellFindDelete = regexp.MustCompile(`\s-delete\b`)
)

// 卡片上的动作标签：比四档等级更具体地回答「这一步在做什么」。
//
// 等级只有 只读 / 查询 / 写入 / 高危 四档，`curl`、`pip install`、`python x.py`、
// `echo a > b` 全挤在「写入」里，用户看不出区别。标签只补这层说明：徽标文案用它，
// 颜色与审批判定仍按等级。
const (
	RiskLabelReadOnly   = "只读"
	RiskLabelNetwork    = "网络请求"
	RiskLabelInstall    = "安装依赖"
	RiskLabelScript     = "脚本执行"
	RiskLabelWriteFile  = "写文件"
	RiskLabelDeleteFile = "删除文件"
)

// shellFileWriteCommands 是「确定会落地文件」的命令：命中就把标签定成写文件。
//
// 名单故意短：`tar` / `git` 这类既能读又能写的命令放进来，`tar -tf` 就会被说成
// 写文件。判不准时退回等级文案「写入」，比编一个具体说法诚实。
var shellFileWriteCommands = map[string]bool{
	"mv": true, "cp": true, "ln": true, "touch": true, "mkdir": true,
	"tee": true, "chmod": true, "chown": true, "truncate": true, "dd": true,
	"patch": true, "unzip": true,
}

// shellDeleteCommands 是删除类命令：比「写文件」更该被看见。
var shellDeleteCommands = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true,
}

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

// shellRiskLabel 给出卡片上的动作标签：比四档等级更具体。
//
// 与 shellRisk 同源（先剥掉 heredoc 正文与单引号字面量，再按管道切段），但两件事
// 分开判：等级回答「有多危险」，标签回答「在做什么」。所以 `python x.py` 仍按
// 写入计提（解释器里干什么静态看不出），标签却如实写「脚本执行」。
//
// 判不出来返回空串，由前端退回等级文案——没有把握时不该给一个具体说法。
func shellRiskLabel(_ Tool, args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return ""
	}
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return ""
	}
	// 只看会被 shell 解释的部分：heredoc 正文与单引号字面量是数据。整篇写代码的
	// 命令里有 python，但那是要写进文件的内容，不是在跑脚本。
	text := shellCommandText(command)
	// 网络 / 装依赖 / 跑脚本三类复用出网判定的同一份分类，避免两套规则慢慢漂移。
	kinds := classifyCommand(text)

	segments := shellSegments(text)
	if len(segments) == 0 {
		return ""
	}
	readOnly := true
	var write, remove bool
	net, install, script := kinds.netTool, kinds.install, kinds.script
	for _, seg := range segments {
		fields := strings.Fields(seg)
		head := segmentHead(fields)
		if shellDeleteCommands[head] || shellFindDelete.MatchString(seg) {
			remove = true
		}
		if shellFileWriteCommands[head] || shellSedInPlace(fields, head) {
			write = true
		}
		// git 的动词决定它是出网、改工作区还是只读：`git push` 不是「写入文件」，
		// `git checkout .` 也不只是查询。只读动词已由 shellGitReadOnly 兜住。
		if head == "git" {
			switch gitSubcommand(fields) {
			case "clone", "fetch", "pull", "push", "ls-remote", "submodule":
				net = true
			case "checkout", "switch", "restore", "reset", "clean", "rm", "mv",
				"merge", "rebase", "cherry-pick", "stash", "apply":
				write = true
			}
		}
		if !shellSegmentReadOnly(seg) {
			readOnly = false
		}
	}
	if shellWritesByRedirect(text) {
		write = true
	}

	// 优先级按「后果有多不可逆」：删除 > 落地文件 > 出网 > 装依赖 > 跑代码。
	// 一条命令同时做几件事时只显示最该被看见的那件，徽标容不下第二个词。
	switch {
	case remove:
		return RiskLabelDeleteFile
	case write:
		return RiskLabelWriteFile
	case net:
		return RiskLabelNetwork
	case install:
		return RiskLabelInstall
	case script:
		return RiskLabelScript
	case readOnly:
		return RiskLabelReadOnly
	}
	return ""
}

// gitSubcommand 取 git 的子命令（`-C dir` 这类选项的参数会被跳过）。
func gitSubcommand(fields []string) string {
	for i, f := range fields {
		if baseName(strings.Trim(f, `"'()`)) != "git" {
			continue
		}
		return shellFirstArg(fields, i+1)
	}
	return ""
}

// shellSedInPlace 判断这一段是不是 sed 原地改文件（`sed -i`）：不带 -i 的 sed 只读。
func shellSedInPlace(fields []string, head string) bool {
	if head != "sed" {
		return false
	}
	for _, f := range fields {
		if f == "--in-place" || strings.HasPrefix(f, "--in-place=") ||
			f == "-i" || (strings.HasPrefix(f, "-i") && !strings.HasPrefix(f, "--")) {
			return true
		}
	}
	return false
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

// shellCommandText 返回命令里真正会被 shell 解释的部分：去掉 heredoc 正文与单引号字面量。
//
// 审批名单与命令分类判的是「会执行什么」，不是「要写进文件什么」。模型常用
// `cat > a.ts <<'EOF' … EOF` 整篇写代码，正文里的反引号（TS 模板字符串）与 $( )
// 在带引号的 heredoc 里是纯数据，按原文匹配会把每一次写文件都判成「命令替换」，
// 于是每改一次代码就多一次确认、多一轮续跑。带引号的单引号同理。
//
// 双引号不能去掉：双引号里的反引号与 $( ) 确实会被展开。转义的写法也保留原样，
// 由各条规则自己决定要不要命中。
func shellCommandText(command string) string {
	if !strings.ContainsRune(command, '\'') && !strings.Contains(command, "<<") {
		return command
	}
	var b strings.Builder
	var pending []heredocMark
	for _, line := range strings.SplitAfter(command, "\n") {
		body := strings.TrimSuffix(line, "\n")
		newline := len(body) != len(line)
		if len(pending) > 0 {
			probe := body
			if pending[0].stripTabs {
				probe = strings.TrimLeft(probe, "\t")
			}
			// 命中定界符行：正文到此结束，这一行本身也不留
			if strings.TrimSpace(probe) == pending[0].word {
				pending = pending[1:]
			}
			if newline {
				b.WriteByte('\n')
			}
			continue
		}
		pending = append(pending, scanHeredocMarks(body)...)
		b.WriteString(stripSingleQuoted(body))
		if newline {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// heredocMark 是一条 heredoc 的定界符。
type heredocMark struct {
	word      string
	stripTabs bool // <<- 允许用行首 tab 缩进定界符
}

// scanHeredocMarks 找出一行里引入的 heredoc（引号内的 << 不算）。
func scanHeredocMarks(line string) []heredocMark {
	var out []heredocMark
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case c == '\\' && quote == '"':
				i++
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			i++
		case '<':
			mark, next, ok := parseHeredocMark(line, i)
			if !ok {
				continue
			}
			out = append(out, mark)
			i = next - 1
		}
	}
	return out
}

// parseHeredocMark 从 i（指向 `<<`）读出定界符；不是 heredoc 时返回 false。
//
// here-string（`<<<`）与算术左移（`<<1`）都不是 heredoc：前者把输入接到命令的
// stdin、后者是数字运算，都不产生「后面若干行是数据」的结构。
func parseHeredocMark(line string, i int) (heredocMark, int, bool) {
	if i+1 >= len(line) || line[i+1] != '<' {
		return heredocMark{}, i, false
	}
	next := i + 2
	if next < len(line) && line[next] == '<' {
		return heredocMark{}, i, false
	}
	mark := heredocMark{}
	if next < len(line) && line[next] == '-' {
		mark.stripTabs = true
		next++
	}
	for next < len(line) && (line[next] == ' ' || line[next] == '\t') {
		next++
	}
	if next >= len(line) {
		return heredocMark{}, i, false
	}
	if q := line[next]; q == '\'' || q == '"' {
		end := strings.IndexByte(line[next+1:], q)
		if end <= 0 {
			return heredocMark{}, i, false
		}
		mark.word = line[next+1 : next+1+end]
		return mark, next + end + 2, true
	}
	end := next
	for end < len(line) && !strings.ContainsRune(" \t;|&<>()", rune(line[end])) {
		end++
	}
	if end == next {
		return heredocMark{}, i, false
	}
	mark.word = line[next:end]
	return mark, end, true
}

// stripSingleQuoted 去掉单引号字面量（连同引号本身）。
//
// 单引号里的反引号、$、$( ) 都不会被展开，按原文匹配就是误判。引号替换成空格，
// 让 `a'b c'd` 仍然保持两个字段，不会把前后文本粘成一个命令名。
func stripSingleQuoted(line string) string {
	if !strings.ContainsRune(line, '\'') {
		return line
	}
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(line) {
				b.WriteByte(line[i+1])
				i++
				continue
			}
			if c == '"' {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'':
			quote = '\''
			b.WriteByte(' ')
		case '"':
			quote = '"'
			b.WriteByte(c)
		case '\\':
			b.WriteByte(c)
			if i+1 < len(line) {
				b.WriteByte(line[i+1])
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
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

// shellLogStatus 把一次执行归成运行日志里的状态。
//
// 超时与输出超限单独成档：它们不是「命令返回了失败」，运维看到这两个词
// 就知道该调超时还是该换更省输出的做法，不必再点进日志正文里找。
func shellLogStatus(res sandbox.Result, runErr error) string {
	switch {
	case errors.Is(runErr, sandbox.ErrTimeout):
		return "timeout"
	case errors.Is(runErr, sandbox.ErrOutputTooLarge):
		return "output_limit"
	case runErr != nil:
		return "error"
	case res.ExitCode != 0:
		return "exit"
	default:
		return "ok"
	}
}
