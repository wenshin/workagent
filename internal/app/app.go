package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/workagent/auto-agent/internal/admin"
	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agent/builtin"
	// 副作用导入：注册 AGENT_IMPL=direct 的直答实现（只有配置选中时才实例化）
	_ "github.com/workagent/auto-agent/internal/agent/direct"
	"github.com/workagent/auto-agent/internal/auth"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/identity"
	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/plugin"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/resource"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/session"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// App 组装路由与服务。
type App struct {
	cfg         *config.Config
	log         *slog.Logger
	store       store.Store
	auth        *auth.Handler
	spaceSvc    *space.Service
	spaceH      *space.Handler
	resourceSvc *resource.Service
	resourceH   *resource.Handler
	sessionSvc  *session.Service
	sessionH    *session.Handler
	pluginSvc   *plugin.Manager
	pluginH     *plugin.Handler
	adminH      *admin.Handler
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
	// 管理员可能尚未登录过：启动时按配置工号尽量把系统空间准备好
	spaceSvc.BootstrapSystemSpace(context.Background())

	authHandler := auth.NewHandler(auth.NewService(st, cfg, idp, spaceSvc, log), cfg, log)

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
	memory := ctxmgr.New(st, router, log, ctxmgr.DefaultBudget())

	// 资源发布：把空间草稿按版本快照发布到独立目录，供 Agent 按需检索
	resourceSvc := resource.NewService(st, fs, spaceSvc, log)

	// 凭证存储（§5.5.2 的 MVP 形态）：信封加密落盘，明文只在用到的那一刻取出
	secretStore, err := secret.NewFileStore(filepath.Join(cfg.StorageRoot, "secrets"), cfg.SecretKEK)
	if err != nil {
		return nil, fmt.Errorf("初始化凭证存储失败: %w", err)
	}
	if !secretStore.Enabled() {
		log.Warn("凭证存储未启用（未配置 SECRET_KEK），需要密钥的能力不可用")
	}
	// 命令执行沙箱：按平台挑最强后端（bwrap > sandbox-exec > local）。
	// 降级到 local 就是没有任何隔离，必须显式告警——让它出现在日志里，
	// 而不是等到出事才发现「沙箱」其实是裸跑。
	shellRunner := sandbox.Detect()
	// 生产环境应当打开 SANDBOX_REQUIRE：探测不到隔离后端就启动失败。
	// 默认关闭是为了让开发机（可能没有 bwrap）能起来，但降级一定会留下告警。
	if err := sandbox.Ensure(shellRunner, cfg.SandboxRequire); err != nil {
		return nil, fmt.Errorf("命令执行沙箱不可用: %w", err)
	}
	if shellRunner.Name() == sandbox.NameLocal {
		log.Warn("没有可用的沙箱后端，shell_exec 将不受隔离（仅适合开发机）", "require", cfg.SandboxRequire)
	} else {
		log.Info("命令执行沙箱已就绪", "backend", shellRunner.Name())
	}

	// 搜索注册表先于插件管理器构造：kind=source 的插件在安装/启用时直接注册进来，
	// 装完即生效，不需要重启服务。
	searchReg := search.NewRegistry("")

	// 插件体系（docs/插件体系设计.md）：五类插件共用清单与生命周期。
	// 脚本插件跑在同一个沙箱里，密钥只经宿主替换，插件进程里没有明文。
	pluginSvc, err := plugin.New(plugin.Options{
		Store:          st,
		Storage:        fs,
		Runner:         shellRunner,
		Secrets:        secretStore,
		Logger:         log,
		Invoker:        invoker,
		RuntimeDir:     filepath.Join(cfg.StorageRoot, "plugin-runtime"),
		DefaultInstall: cfg.PluginsDefaultInstall,
		Sources:        searchReg,
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

	sessionSvc := session.NewService(st, spaceSvc, resourceSvc, agentSvc, fs, memory, searchReg, shellRunner, cfg.LLMMaxWallClock, log)
	// 智能体也可以是插件（kind=agent）：系统空间安装的智能体对所有空间可选。
	sessionSvc.SetAgentPlugins(pluginSvc)
	// 插件试跑：会话里写完草稿直接验配置
	sessionSvc.SetPluginTry(pluginSvc)
	// 凭证存储：会话内的 secret_request 只查「配过没有」，值不进模型上下文
	sessionSvc.SetSecrets(secretStore)

	return &App{
		cfg:         cfg,
		log:         log,
		store:       st,
		auth:        authHandler,
		spaceSvc:    spaceSvc,
		spaceH:      space.NewHandler(spaceSvc, authHandler.CurrentUser, log),
		resourceSvc: resourceSvc,
		resourceH:   resource.NewHandler(resourceSvc, authHandler.CurrentUser, log),
		sessionSvc:  sessionSvc,
		sessionH:    session.NewHandler(sessionSvc, authHandler.CurrentUser, log),
		pluginSvc:   pluginSvc,
		pluginH:     plugin.NewHandler(pluginSvc, st, spaceSvc, authHandler.CurrentUser, log),
		// 平台管理：凭证写入 + 指纹回显。密钥只进不出。
		// refs 传空：搜索凭证由 source 插件清单里的 source.secret 声明，
		// 属于运行期状态（随插件安装/卸载变化），启动时无法静态列出。
		adminH: admin.NewHandler(spaceSvc, secretStore, nil, authHandler.CurrentUser, log),
	}, nil
}

// Handler 返回带中间件的根 handler。前端由 CDN/Nginx 托管并经同源反向代理访问本服务，无需 CORS。
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.auth.Register(mux)
	a.spaceH.Register(mux)
	a.resourceH.Register(mux)
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


