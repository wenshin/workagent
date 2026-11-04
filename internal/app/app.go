package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agent/builtin"
	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/confstore"
	// 副作用导入：注册 AGENT_IMPL=direct 的直答实现（只有配置选中时才实例化）
	_ "github.com/workagent/auto-agent/internal/agent/direct"
	"github.com/workagent/auto-agent/internal/auth"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/identity"
	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/resource"
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
	// 平台配置：系统空间的 config.json / net_whitelist.json，读不到就回退环境变量。
	// 配置写错直接启动失败，避免「看起来改好了、实际还在用默认值」。
	platCfg, netAllow, err := confstore.NewLoader(spaceSvc).Load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("读取平台配置失败: %w", err)
	}
	searchProvider := firstNonEmpty(platCfg.Search.Provider, cfg.SearchProvider)
	searchEndpoint := firstNonEmpty(platCfg.Search.Endpoint, cfg.SearchEndpoint)
	searchSecret := firstNonEmpty(platCfg.Search.Secret, searchProvider)
	allow := netAllow.Domains
	if len(allow) == 0 {
		allow = cfg.SearchAllow
	}

	// 首次播种：SEARCH_API_KEY 只用于初始化，之后以凭证存储为准
	if secretStore.Enabled() && cfg.SearchAPIKey != "" {
		seedCtx := context.Background()
		if _, getErr := secretStore.Get(seedCtx, search.SecretScope, searchSecret); getErr != nil {
			if putErr := secretStore.Put(seedCtx, search.SecretScope, searchSecret, cfg.SearchAPIKey); putErr != nil {
				log.Warn("搜索密钥播种失败", "err", putErr)
			} else {
				log.Info("搜索密钥已写入凭证存储", "secret", searchSecret)
			}
		}
	}

	// 搜索（§5.6.1）：出网统一走 Guard，provider 只认识平台配置的 endpoint
	searchReg := search.NewRegistry(searchProvider)
	switch searchProvider {
	case "bocha":
		searchReg.Register(search.NewBocha(searchEndpoint, func(ctx context.Context) (string, error) {
			return secretStore.Get(ctx, search.SecretScope, searchSecret)
		}, search.NewGuard(allow)))
	default:
		log.Warn("未知的搜索 provider，web_search 将不可用", "provider", searchProvider)
	}
	log.Info("搜索能力已装配", "provider", searchProvider, "endpoint", searchEndpoint,
		"available", searchReg.Names(), "allow", allow, "secret_ready", secretStore.Enabled())

	// 智能体设定：从系统空间 agents/ 读取，只追加提示词，不影响权限
	agentLoader := agentspec.NewLoader(spaceSvc)

	sessionSvc := session.NewService(st, spaceSvc, resourceSvc, agentSvc, fs, memory, agentLoader, searchReg, cfg.LLMMaxWallClock, log)

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
	}, nil
}

// Handler 返回带中间件的根 handler。前端由 CDN/Nginx 托管并经同源反向代理访问本服务，无需 CORS。
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.auth.Register(mux)
	a.spaceH.Register(mux)
	a.resourceH.Register(mux)
	a.sessionH.Register(mux)

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

// firstNonEmpty 返回第一个非空值，用于「系统空间配置优先、环境变量兜底」。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
