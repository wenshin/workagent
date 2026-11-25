package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/admin"
	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agent/builtin"
	// 副作用导入：注册 AGENT_IMPL=direct 的直答实现（只有配置选中时才实例化）
	_ "github.com/workagent/auto-agent/internal/agent/direct"
	"github.com/workagent/auto-agent/internal/auth"
	"github.com/workagent/auto-agent/internal/cache"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/golden"
	"github.com/workagent/auto-agent/internal/identity"
	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/plugin"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/sandbox/k8s"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/securityscan"
	"github.com/workagent/auto-agent/internal/session"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/spacelog"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// App 组装路由与服务。
type App struct {
	cfg   *config.Config
	log   *slog.Logger
	store store.Store
	// cache 是登录态与会话流的缓存连接，随 App 关闭一起释放
	cache      cache.Client
	auth       *auth.Handler
	spaceSvc   *space.Service
	spaceH     *space.Handler
	sessionSvc *session.Service
	sessionH   *session.Handler
	pluginSvc  *plugin.Manager
	pluginH    *plugin.Handler
	adminH     *admin.Handler
}

// New 构造应用。
func New(cfg *config.Config, st store.Store, fs storage.Storage, log *slog.Logger) (*App, error) {
	// 身份接入（§5.1.1）：按 IDENTITY_PROVIDER 选择实现，切换 IdP 不改业务代码
	idp := identity.New(cfg, log)
	if !idp.Ready() {
		log.Warn("身份接入未就绪，登录功能不可用",
			"provider", idp.Name(), "display", idp.DisplayName(), "missing", idp.Missing())
	} else {
		log.Info("身份接入已就绪", "provider", idp.Name(), "display", idp.DisplayName())
	}
	spaceSvc := space.NewService(st, fs, log, cfg.AdminJobs)
	// 上传上限按 MB 配置，0 表示沿用默认值（100MB）
	spaceSvc.SetUploadMaxBytes(int64(cfg.UploadMaxFileMB) << 20)
	// 管理员可能尚未登录过：启动时按配置工号尽量把系统空间准备好
	spaceSvc.BootstrapSystemSpace(context.Background())

	// 登录态与会话流都放在缓存层：配了 REDIS_URL 走 Redis，留空退回进程内存。
	cacheClient, err := cache.Open(cfg.RedisURL, cfg.RunStreamTTL)
	if err != nil {
		return nil, err
	}

	authHandler := auth.NewHandler(auth.NewService(st, cfg, idp, spaceSvc, cacheClient, log), cfg, log)

	router := buildRouter(cfg, log)

	// Tool 三层模型：内置能力 + MCP 远端工具，统一走 ToolInvoker 策略链
	mcpManager := mcp.NewManager(st, log)
	if err := mcpManager.Sync(context.Background()); err != nil {
		log.Warn("MCP 工具同步失败，本次仅提供内置工具", "err", err)
	}

	if err := tool.RegisterCapabilities(context.Background(), st); err != nil {
		log.Warn("内置能力注册失败", "err", err)
	}

	registry := tool.NewRegistry(st, mcpManager, log)
	invoker := tool.NewInvoker(policy.RBACEngine{}, st, log)
	// 单次调用参数上限跟着单次输出预算走：调大 LLM_MAX_TOKENS 就能一次写更大的文件，
	// 不用再改工具代码（见 tool.DefaultMaxArgBytes）。
	invoker.SetMaxArgBytes(tool.DefaultMaxArgBytes(cfg.LLMMaxTokens))

	// Agent 运行时（§5.7.7）：编排实现按 AGENT_IMPL 选择；框架依赖只允许出现在实现子包内，
	// 本层只依赖 agent.Agent 契约。配置写错时回退内置实现，避免整个服务起不来。
	agentDeps := agent.Deps{Config: cfg, LLM: router, Registry: registry, Invoker: invoker, Log: log}
	agentSvc, err := agent.New(cfg.AgentImpl, agentDeps)
	if err != nil {
		log.Warn("Agent 实现不可用，回退内置实现",
			"impl", cfg.AgentImpl, "fallback", builtin.Name, "err", err, "available", agent.Available())
		if agentSvc, err = agent.New(builtin.Name, agentDeps); err != nil {
			return nil, err
		}
	}
	log.Info("Agent 运行时已就绪",
		"impl", agentSvc.Name(), "capabilities", agentSvc.Capabilities(), "available", agent.Available())

	if !cfg.LLMReady() {
		log.Warn("模型服务未配置，对话接口将不可用", "missing", cfg.MissingLLM())
	} else {
		log.Info("模型服务已就绪",
			"provider", cfg.LLMProvider,
			"model", cfg.LLMModel,
			"routes", router.Describe(),
			"embedding", router.EmbeddingReady())
	}

	// 上下文管理：摘要压缩 + 向量检索（§5.7.2 / §5.7.4）
	memory := ctxmgr.New(st, router, log, ctxmgr.Budget{
		Window:       cfg.CtxWindow,
		MaxRunes:     cfg.CtxMaxRunes,
		TopK:         cfg.CtxTopK,
		SummaryRunes: cfg.CtxSummaryRunes,
		MinScore:     0.35,
	})

	// 凭证存储（§5.5.2 的 MVP 形态）：信封加密落盘，明文只在用到的那一刻取出
	secretStore, err := secret.NewFileStore(filepath.Join(cfg.StorageRoot, "secrets"), cfg.SecretKEK)
	if err != nil {
		return nil, fmt.Errorf("初始化凭证存储失败: %w", err)
	}
	if !secretStore.Enabled() {
		log.Warn("凭证存储未启用（未配置 SECRET_KEK），需要密钥的能力不可用")
	}
	// 安装源在进程内是全局状态：工具注入环境变量、提示词展示「安装源」都读它。
	// 启动时按部署配置覆盖一次，之后不再改——它不是每空间配置。
	tool.SetInstallMirrors(tool.InstallMirrors{
		NPM:  cfg.InstallMirrorNPM,
		PyPI: cfg.InstallMirrorPyPI,
		Node: cfg.InstallMirrorNode,
		Go:   cfg.InstallMirrorGo,
	})
	// 打出实际生效的值：配置项拼错时，日志里一眼能看出还是默认源，
	// 而不是等模型装上依赖慢到怀疑人生。
	mirrors := tool.CurrentInstallMirrors()
	log.Info("安装源已配置",
		"npm", mirrors.NPM, "pip", mirrors.PyPI, "node", mirrors.Node, "go", mirrors.Go)

	// 命令执行沙箱：两种形态，二选一。
	//   local  —— 在本进程里跑（只能是 bwrap；探测不到就失败），客户端 runtime
	//   remote —— 容器化部署：执行体在租户容器里，控制面只发请求
	// 两者都要求「探测不到隔离就报错」，且都以后端自己报的名字为准：
	// 远端执行体降级成 local 时，控制面必须知道，否则审计里会写成 bwrap。
	shellRunner, err := newShellRunner(cfg, st, log)
	if err != nil {
		return nil, err
	}

	// 搜索注册表先于插件管理器构造：kind=source 的插件在安装/启用时直接注册进来，
	// 装完即生效，不需要重启服务。
	searchReg := search.NewRegistry("")

	// 空间运行日志：插件与 shell 的每次执行各留一条，写在空间隐藏目录 files/.logs，
	// 按天分文件、历史压缩。暂不对用户与 Agent 开放，只作为排障留证。
	spaceLog := spacelog.New(fs, spacelog.Options{
		RetainDays:   cfg.SpaceLogRetentionDays,
		MaxFileBytes: int64(cfg.SpaceLogMaxFileMB) << 20,
		Logger:       log,
	})

	// 插件体系（docs/插件体系设计.md）：五类插件共用清单与生命周期。
	// 脚本插件跑在同一个沙箱里，密钥只经宿主替换，插件进程里没有明文。
	// 沙箱是白名单挂载：运行时装在非标准位置时，必须显式把安装前缀挂进去。
	sandbox.SetExtraRead(cfg.SandboxReadExtra)

	// 租户级缓存：apt 索引与 deb、npm / pip / go 的下载、Deno 的 DENO_DIR。
	// 部署时挂到同一块盘上——它不保证"装好了"，只保证"重装不用下载"。
	// 缺这个，Pod 重建后每次 apt install 都要重新走一遍网络。
	if dir := strings.TrimSpace(os.Getenv("WA_CACHE_DIR")); dir != "" {
		tool.SetCacheRoot(dir)
		log.Info("租户缓存目录", "dir", tool.CacheRoot())
	}
	// 缓存保留天数，0 表示不回收。默认 30 天。
	if days := strings.TrimSpace(os.Getenv("WA_CACHE_TTL_DAYS")); days != "" {
		if n, err := strconv.Atoi(days); err == nil {
			if n <= 0 {
				tool.SetCacheTTL(0)
			} else {
				tool.SetCacheTTL(time.Duration(n) * 24 * time.Hour)
			}
		}
	}
	tool.StartCacheGC(log)

	// 每空间磁盘软配额，单位 MiB；0 或空表示不限制。
	// 软配额的语义是「执行前算一次，超了拒绝启动新命令」，
	// 不做硬配额是因为硬配额要文件系统或 cgroup 配合，改不动。
	if mb := strings.TrimSpace(os.Getenv("WA_SPACE_QUOTA_MB")); mb != "" {
		if n, err := strconv.ParseInt(mb, 10, 64); err == nil {
			tool.SetSpaceQuota(n << 20)
			log.Info("空间磁盘配额", "limit", mb+"MiB")
		}
	}

	// 发布扫描：规则与漏洞库都放在存储目录里，改规则不需要重启（每次发布前比对一次）。
	scanner, err := securityscan.New(securityscan.Config{
		RulesDir: cfg.ScanRulesDir,
		OsvMode:  securityscan.OsvMode(cfg.ScanOsvMode),
		OsvDir:   osvDir(cfg),
		Logger:   log,
		Allow:    allowRules(cfg.ScanAllowRules),
	})
	if err != nil {
		return nil, fmt.Errorf("初始化发布扫描器失败: %w", err)
	}
	log.Info("发布扫描已就绪", "rules", scanner.RulesVersion(), "osv", string(cfg.ScanOsvMode))

	pluginSvc, err := plugin.New(plugin.Options{
		Store:           st,
		Storage:         fs,
		Runner:          shellRunner,
		Secrets:         secretStore,
		Logger:          log,
		Invoker:         invoker,
		RuntimeDir:      filepath.Join(cfg.StorageRoot, "plugin-runtime"),
		DefaultInstall:  cfg.PluginsDefaultInstall,
		Sources:         searchReg,
		Scanner:         scanner,
		ProcMax:         cfg.PluginProcMax,
		ProcMaxRestarts: cfg.PluginProcMaxRestarts,
		// 插件在沙箱里不知道平台在哪，这个值由宿主注入（wa.config().host_base），
		// 不让使用者填：抄错的唯一表现是投递 401，排查方向还全错。
		// 用 PluginHostBase 而不是 AppBaseURL：对外地址沙箱里往往不可达
		// （容器里 127.0.0.1 就是自己），两者必须是两个可分别配置的值。
		HostBase: cfg.PluginHostBase,
		// 插件身份密钥：渠道插件没有登录态，回调 /inbox 时带平台签发的令牌，
		// 宿主用同一份密钥验签，验不过就是 401。
		TokenSecret: cfg.PluginTokenSecret,
		// 插件执行留证：与 shell 共用同一个写入端，落在各空间的 files/.logs
		SpaceLog: spaceLog,
		// 发布门禁跑插件自带的黄金用例：确定性用例总跑，行为用例用平台预算跑
		Evals: golden.Runner{Options: golden.Options{
			MaxRounds:    cfg.LLMMaxRounds,
			MaxToolCalls: cfg.LLMMaxToolCalls,
			StallRounds:  cfg.AgentStallRounds,
			MaxDepth:     cfg.AgentMaxDepth,
		}},
		OnChange: func(ctx context.Context, spaceID string) {
			if err := spaceSvc.RebuildIndex(ctx, spaceID); err != nil {
				log.Warn("重建空间索引失败", "space", spaceID, "err", err)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化插件管理器失败: %w", err)
	}
	// 插件工具与内置工具平级，统一走 Invoker 的策略链与审计。
	registry.SetPluginProvider(pluginSvc)
	// 已安装插件（含未落副本的预置件）进检索池；published/... 路径由插件层解析。
	spaceSvc.SetInstalledSource(pluginSvc.InstalledSources)
	// 停用是运行期状态，文件里看不出来：索引合并时必须按它筛掉，
	// 否则停在系统空间的内置技能照样能被 plugin_search 搜到。
	spaceSvc.SetDisabledPluginSource(pluginSvc.DisabledSlugs)
	spaceSvc.SetPublishedReader(pluginSvc.ReadPublished)
	// 本地插件必须启用才进检索池（未启用的草稿仍可在文件树里编辑）。
	spaceSvc.SetLocalPluginSource(pluginSvc.LocalEnabledMap)
	// 直接删 plugins/<slug>/ 是文件操作，绕过了插件接口：删完要让运行时缓存失效。
	spaceSvc.SetPluginChanged(pluginSvc.Reload)
	// 新空间创建后补装部署者配置的默认安装集；失败只影响该空间的预装。
	spaceSvc.SetSpaceCreated(func(ctx context.Context, spaceID string) {
		if err := pluginSvc.EnsureBootstrap(ctx, spaceID); err != nil {
			log.Warn("初始化空间默认安装集失败", "space", spaceID, "err", err)
		}
	})
	// 启动期 reconcile：发布平台预置插件、一次性迁移旧技能目录、补默认安装集。
	if err := pluginSvc.Bootstrap(context.Background()); err != nil {
		log.Warn("插件启动初始化失败（不影响服务启动）", "err", err)
	}

	// 搜索源只有插件一条路：kind=source 的插件在安装/启用时注册进 searchReg，
	// 平台不再从 config.json 或环境变量拼装内置源——那会让「用哪个搜索服务」
	// 散落在配置、代码和环境变量三处，且换源必须发版或改文件。

	// 智能体设定：从系统空间 agents/ 读取，只追加提示词，不影响权限

	sessionSvc := session.NewService(st, spaceSvc, agentSvc, fs, memory, searchReg, shellRunner, cacheClient, cfg.LLMMaxWallClock, cfg.RunIdleTimeout, log)

	// 启动清扫：上一个进程留下的 running 不会再有人写终态，留着只会让前端
	// 一直显示「生成中」。断点保留，用户打开会话就能点「继续」。
	if _, err := sessionSvc.SweepStaleRuns(context.Background()); err != nil {
		log.Warn("清扫上次未完成的执行失败", "err", err)
	}
	// 智能体也可以是插件（kind=agent）：系统空间安装的智能体对所有空间可选。
	sessionSvc.SetAgentPlugins(pluginSvc)
	// 插件试跑：会话里写完草稿直接验配置
	sessionSvc.SetPluginTry(pluginSvc)
	// 常驻进程重启：改完草稿在会话里直接换进程，不用去面板关开一次
	sessionSvc.SetPluginRestart(pluginSvc)
	// 插件调用记录：同一份 Manager 持有的内存缓冲，会话侧只读
	sessionSvc.SetPluginLogs(pluginSvc)
	// 空间运行日志：shell 与插件共用，工具侧通过 Env.SpaceLog 拿到
	sessionSvc.SetSpaceLog(spaceLog)
	// 凭证存储：会话内的 secret_request 只查「配过没有」，值不进模型上下文
	sessionSvc.SetSecrets(secretStore)
	// 平台部署事实：租户兜底名来自配置（登录用户的通讯录资料更准），
	// IM 以当前选定的身份接入实现为准；这些会随系统提示词注入，不做成工具
	platform := agent.PlatformFacts{
		Tenant:       cfg.DingTalkCorpName,
		IMName:       idp.DisplayName(),
		IMConfigured: idp.Ready(),
	}
	// 执行环境由执行体上报（只有容器化部署才有）。本地后端没有「镜像」这个概念，
	// 断言失败就整段省略：提示词里宁可不说，也不要写一句猜出来的边界。
	if p, ok := shellRunner.(sandbox.EnvProvider); ok {
		if f := p.Env(); !f.Empty() {
			platform.Exec = toExecFacts(f)
			log.Info("执行环境已注入提示词",
				"image", f.Image, "tools", len(f.Tools), "env_hash", f.Hash())
		}
	}
	sessionSvc.SetPlatformFacts(platform)

	// IM 渠道桥：渠道插件不持任何人身份，只能把「谁发来的什么」交给宿主，
	// 由宿主投进那个人固定的渠道会话。反向注入（插件层拿得到会话层能力）是
	// 单向的：插件层只看到 IMSubmitter 这两个方法，读不到任何会话内容。
	pluginSvc.SetIM(sessionSvc)

	app := &App{
		cfg:        cfg,
		log:        log,
		store:      st,
		cache:      cacheClient,
		auth:       authHandler,
		spaceSvc:   spaceSvc,
		spaceH:     space.NewHandler(spaceSvc, authHandler.CurrentUser, log),
		sessionSvc: sessionSvc,
		sessionH:   session.NewHandler(sessionSvc, authHandler.CurrentUser, log),
		pluginSvc:  pluginSvc,
		pluginH:    plugin.NewHandler(pluginSvc, st, spaceSvc, authHandler.CurrentUser, log),
		// 平台管理：凭证写入 + 指纹回显。密钥只进不出。
		// refs 传空：搜索凭证由 source 插件清单里的 source.secret 声明，
		// 属于运行期状态（随插件安装/卸载变化），启动时无法静态列出；
		// 「使用中」标记因此也改成每次现算，而不是启动时定死。
		adminH: admin.NewHandler(spaceSvc, secretStore, nil, secretUsage(searchReg, pluginSvc, log),
			authHandler.CurrentUser, log),
	}
	// 应用密钥页（系统空间入口）：归属与密钥都在插件层，这里只把视图接上，
	// 鉴权仍由 admin 统一做（系统管理员 = 系统空间成员）。
	app.adminH.SetMCPApps(pluginSvc)
	return app, nil
}

// secretUsage 现算「哪些凭证正在被使用」：搜索源看注册表（装/停即变），
// 插件声明的 secret.use 看已启用安装件的清单。
func secretUsage(reg *search.Registry, plugins *plugin.Manager, log *slog.Logger) admin.Usage {
	return func(ctx context.Context) map[string]bool {
		usage := map[string]bool{}
		for _, src := range reg.Sources() {
			if name := src.Secret; name != "" {
				usage[search.SecretScope+"/"+name] = true
			}
		}
		pluginUsage, err := plugins.SecretUsage(ctx)
		if err != nil {
			log.Warn("统计插件声明的凭证失败", "err", err)
			return usage
		}
		for key := range pluginUsage {
			usage[key] = true
		}
		return usage
	}
}

// osvDir 决定本地漏洞库目录：显式配置优先，否则用存储根下的 osv/。
func osvDir(cfg *config.Config) string {
	if cfg.ScanOsvDir != "" {
		return cfg.ScanOsvDir
	}
	if cfg.ScanOsvMode == string(securityscan.OsvLocal) {
		return filepath.Join(cfg.StorageRoot, "osv")
	}
	return ""
}

func allowRules(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// Handler 返回带中间件的根 handler。前端由 CDN/Nginx 托管并经同源反向代理访问本服务，无需 CORS。
// Close 释放后台资源。插件常驻进程必须显式关掉，否则服务重启后
// 会留下上一版的 deno 子进程（它们没有 --die-with-parent 之外的生命周期）。
func (a *App) Close() {
	tool.StopCacheGC()
	if a.pluginSvc != nil {
		a.pluginSvc.Close()
	}
	if a.cache != nil {
		_ = a.cache.Close()
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.auth.Register(mux)
	a.spaceH.Register(mux)
	a.sessionH.Register(mux)
	a.pluginH.Register(mux)
	a.adminH.Register(mux)

	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /agent/info", a.agentInfo)
	mux.HandleFunc("GET /{$}", a.index)

	var h http.Handler = mux
	h = platform.AccessLog(a.log)(h)
	h = platform.Recover(a.log)(h)
	h = platform.RequestID(h)
	return h
}

// agentInfo 供前端展示当前模型与可用状态。
func (a *App) agentInfo(w http.ResponseWriter, r *http.Request) {
	if _, err := a.auth.CurrentUser(r); err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"ready":        a.sessionSvc.AgentReady(),
		"model":        a.sessionSvc.AgentModel(),
		"impl":         a.sessionSvc.AgentImpl(),
		"capabilities": a.sessionSvc.AgentCapabilities(),
		// 前端用它显示上传上限，避免文案与 UPLOAD_MAX_FILE_MB 不一致
		"upload_max_file_mb": a.spaceSvc.UploadMaxBytes() >> 20,
	})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	platform.WriteJSON(w, http.StatusOK, map[string]string{
		"service": "auto-agent-api",
		"status":  "ok",
		"web":     a.cfg.WebBaseURL,
	})
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		platform.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "db": "down"})
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "ok"})
}

// newShellRunner 按运行形态构造执行面。
//
// 判定口径是「谁在执行 agent」，不是「这是不是开发环境」：
//
//	server —— 控制面自己建容器（k8s）。服务端进程（生产、CI、本机调试一视同仁）
//	          执行 agent，节点的内核、镜像与安全上下文都不由我们决定，不能假设
//	          bwrap 可用；执行体每次握手都校验隔离后端，没有隔离就直接失败，
//	          不存在「容器是唯一边界，先跑起来」的降级档。
//	client —— 只能本进程起 bwrap。装在用户机器上的 runtime 必须自带真隔离，
//	          连容器等于把用户目录交给另一个进程。
func newShellRunner(cfg *config.Config, st store.Store, log *slog.Logger) (sandbox.Runner, error) {
	switch cfg.Role {
	case config.RoleServer:
		return newServerRunner(cfg, st, log)
	case config.RoleClient:
		return newClientRunner(cfg, log)
	default:
		return nil, fmt.Errorf("未知运行形态 %q：只能是 server / client（dev 已删除，本机调试同样连容器化执行体）", cfg.Role)
	}
}

// newServerRunner 构造服务端执行面：控制面自己建容器，
// 每空间一个空间容器（空闲销毁），每租户一个插件容器（常驻）。
//
// 只有 k8s 一档：不再保留「连一个现成执行体」的 remote 档——那会让所有空间
// 共用同一个 /usr（一个空间装包改变另一个空间的环境），而且开发与生产的
// 隔离形态不一致，测出来的东西不是跑起来的东西。
func newServerRunner(cfg *config.Config, st store.Store, log *slog.Logger) (sandbox.Runner, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.SandboxBackend)) {
	case "k8s", "":
		runner, err := k8s.New(k8s.Config{
			Namespace:      cfg.SandboxNamespace,
			Image:          cfg.SandboxImage,
			Token:          cfg.SandboxRunnerToken,
			TokenSecret:    cfg.SandboxTokenSecret,
			TokenKey:       cfg.SandboxTokenKey,
			Kubeconfig:     cfg.SandboxKubeconfig,
			StorageClass:   cfg.SandboxStorageClass,
			StoragePVCName: cfg.SandboxStoragePVC,
			StoragePVCSize: cfg.SandboxStoragePVCSize,
			CachePVCSize:   cfg.SandboxCachePVCSize,
			ShellMemory:    cfg.SandboxShellMemory,
			PluginMemory:   cfg.SandboxPluginMemory,
			ShellIdle:      cfg.SandboxShellIdle,
			ReadyTimeout:   cfg.SandboxReadyTimeout,
			CacheShared:    cfg.SandboxCacheShared,
			InitImage:      cfg.SandboxInitImage,
			TenantOf:       tenantResolver(st),
			Log:            log,
		})
		if err != nil {
			return nil, fmt.Errorf("k8s 执行面不可用: %w", err)
		}
		log.Info("执行面已就绪", "role", config.RoleServer, "transport", runner.Transport(),
			"namespace", cfg.SandboxNamespace, "image", cfg.SandboxImage)
		return runner, nil

	default:
		return nil, fmt.Errorf(
			"服务端（WA_ROLE=server）只能用容器化执行体：SANDBOX_BACKEND 只能是 k8s（当前 %q）",
			cfg.SandboxBackend)
	}
}

// tenantResolver 把空间解析成租户（空间的属主所属企业）。
//
// 插件容器按租户共享，这个值决定哪些空间共用一个 Pod。解析失败时执行面
// 退化为「每空间一个插件容器」——多起容器不影响正确性。
func tenantResolver(st store.Store) func(context.Context, string) (string, error) {
	if st == nil {
		return nil
	}
	return func(ctx context.Context, spaceID string) (string, error) {
		sp, err := st.GetSpace(ctx, spaceID)
		if err != nil {
			return "", err
		}
		if sp.OwnerID == "" {
			return "", nil
		}
		u, err := st.GetUser(ctx, sp.OwnerID)
		if err != nil {
			return "", err
		}
		return u.CorpName, nil
	}
}

// newClientRunner 只接受本进程的 bwrap。
func newClientRunner(cfg *config.Config, log *slog.Logger) (sandbox.Runner, error) {
	if !strings.EqualFold(strings.TrimSpace(cfg.SandboxBackend), "local") {
		return nil, fmt.Errorf(
			"客户端（WA_ROLE=client）只能用本机沙箱：SANDBOX_BACKEND 必须为 local（当前 %q）",
			cfg.SandboxBackend)
	}
	runner, err := sandbox.NewBwrap()
	if err != nil {
		return nil, fmt.Errorf(
			"客户端要求 bwrap：%w（客户端形态只在 Linux 且内核允许 user namespace 的机器上成立）", err)
	}
	log.Info("本地沙箱已就绪", "role", config.RoleClient, "backend", runner.Name())
	return runner, nil
}

// toExecFacts 把执行体的自述转成契约层的结构。
//
// 两个包各自定义一份而不是直接复用，是为了让 internal/agent 不依赖 sandbox：
// 契约层只描述「提示词需要什么」，执行层才关心「怎么探测出来的」。
func toExecFacts(f sandbox.EnvFacts) agent.ExecFacts {
	// 只转「写进提示词的两项」：可写目录、出网形态与容器持久性不再进提示词，
	// 它们由 shell_exec 的描述承载（见 internal/tool/shell.go）。
	out := agent.ExecFacts{Image: f.Image, OS: f.OS}
	if len(f.Tools) > 0 {
		out.Tools = make([]agent.ExecTool, 0, len(f.Tools))
		for _, t := range f.Tools {
			out.Tools = append(out.Tools, agent.ExecTool{Name: t.Name, Version: t.Version})
		}
	}
	return out
}
