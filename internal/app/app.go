package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/admin"
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
	// 平台配置：系统空间的 config.json / net_whitelist.json，读不到就回退环境变量。
	// 配置写错直接启动失败，避免「看起来改好了、实际还在用默认值」。
	platCfg, netAllow, err := confstore.NewLoader(spaceSvc).Load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("读取平台配置失败: %w", err)
	}
	searchProvider := firstNonEmpty(platCfg.Search.Provider, cfg.SearchProvider)
	searchEndpoint := firstNonEmpty(platCfg.Search.Endpoint, cfg.SearchEndpoint)
	// 凭证引用有两种写法：
	//   "default"            → 只看凭证存储里的槽位名（界面「密钥配置」里录入）
	//   "${SEARCH_API_KEY}"  → 先查凭证存储同名条目，没有再读环境变量
	//
	// ${} 的作用只是让「密钥从哪来」在配置里显式可见，不是二选一的开关：
	// 界面录入的那条同样能被 ${} 命中（且优先），这样既能在后台轮换，
	// 也能在没配 SECRET_KEK 的机器上直接用环境变量跑起来。
	searchSecret := firstNonEmpty(platCfg.Search.Secret, search.DefaultSecretName)
	searchName, searchExplicit := parseSecretRef(searchSecret)
	allow := netAllow.Domains
	if len(allow) == 0 {
		allow = cfg.SearchAllow
	}

	// 首次播种：SEARCH_API_KEY 只用于初始化，之后以凭证存储为准。
	// 写的是 ${NAME} 时由引用自身决定来源，不需要再把环境变量抄一份进存储。
	if !searchExplicit && secretStore.Enabled() && cfg.SearchAPIKey != "" {
		seedCtx := context.Background()
		if _, getErr := secretStore.Get(seedCtx, search.SecretScope, searchSecret); getErr != nil {
			if putErr := secretStore.Put(seedCtx, search.SecretScope, searchSecret, cfg.SearchAPIKey); putErr != nil {
				log.Warn("搜索密钥播种失败", "err", putErr)
			} else {
				log.Info("搜索密钥已写入凭证存储", "secret", searchSecret)
			}
		}
	}

	// 旧部署的凭证名曾是 provider 名（bocha），统一成 default 后搬一次，
	// 否则升级完 web_search 会突然说「密钥不可用」。
	if !searchExplicit && secretStore.Enabled() && searchSecret != searchProvider {
		migCtx := context.Background()
		if _, getErr := secretStore.Get(migCtx, search.SecretScope, searchSecret); errors.Is(getErr, secret.ErrNotFound) {
			if v, legacyErr := secretStore.Get(migCtx, search.SecretScope, searchProvider); legacyErr == nil && v != "" {
				if putErr := secretStore.Put(migCtx, search.SecretScope, searchSecret, v); putErr != nil {
					log.Warn("搜索密钥迁移失败", "err", putErr)
				} else {
					_ = secretStore.Delete(migCtx, search.SecretScope, searchProvider)
					log.Info("搜索密钥已迁移到统一凭证名", "from", searchProvider, "to", searchSecret)
				}
			}
		}
	}

	// 声明式搜索源：config.json 里给了 search.spec 就按配置拼请求 / 取结果，
	// 接一个新的 REST 搜索 API 不用写 Go、不用发版。
	var searchSpec *search.Spec
	if len(platCfg.Search.Spec) > 0 {
		var s search.Spec
		if err := json.Unmarshal(platCfg.Search.Spec, &s); err != nil {
			return nil, fmt.Errorf("config.json 的 search.spec 解析失败: %w", err)
		}
		searchSpec = &s
	}

	// 搜索（§5.6.1）：出网统一走 Guard。
	searchReg := search.NewRegistry(searchProvider)
	keyFunc := func(ctx context.Context) (string, error) {
		if !searchExplicit {
			return secretStore.Get(ctx, search.SecretScope, searchName)
		}
		// 存储优先：加密落盘、可在界面轮换、有指纹可审计。
		if v, err := secretStore.Get(ctx, search.SecretScope, searchName); err == nil && v != "" {
			return v, nil
		}
		// 没有同名凭证时退回环境变量，适合部署平台（k8s Secret / systemd）注入。
		if v := strings.TrimSpace(os.Getenv(searchName)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("密钥 %s 不可用：凭证存储里没有该条目，环境变量也未设置", searchName)
	}
	guard := search.NewGuard(allow)
	switch {
	case searchSpec != nil:
		provider, buildErr := search.NewHTTPJSON(searchProvider, searchEndpoint, *searchSpec, keyFunc, guard)
		if buildErr != nil {
			return nil, fmt.Errorf("config.json 的 search.spec 不可用: %w", buildErr)
		}
		searchReg.Register(provider)
	default:
		// 没有 spec 时回落到内置 provider：名字 → 构造函数由工厂表解析。
		if provider, ok := search.Build(searchProvider, searchEndpoint, keyFunc, guard); ok {
			searchReg.Register(provider)
		} else {
			log.Warn("未知的搜索 provider，web_search 将不可用",
				"provider", searchProvider, "known", search.ProviderNames())
		}
	}
	log.Info("搜索能力已装配", "provider", searchProvider, "endpoint", searchEndpoint,
		"secret", searchSecret, "available", searchReg.Names(), "allow", allow,
		"declarative", searchSpec != nil, "secret_ready", secretStore.Enabled() || searchExplicit)

	// 启动时就把「引用的密钥在不在」说清楚。
	// 引用的名字写错（凭证没录入 / 环境变量没导出）是最容易出现的错配：
	// 配置看着没问题、服务也能起，直到第一次搜索才报错。这里提前告警并给出名字。
	if secretStore.Enabled() || searchExplicit {
		if _, getErr := keyFunc(context.Background()); getErr != nil {
			log.Warn("搜索密钥不可用，web_search 会报错", "secret", searchSecret, "err", getErr)
		}
	}

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
		// 平台管理：凭证写入 + 指纹回显。密钥只进不出。
		// refs 告诉管理页「哪一条密钥真正被引用」，避免改了 A 却仍跑在 B 上。
		adminH: admin.NewHandler(spaceSvc, secretStore, []admin.SecretRef{{
			Scope:  search.SecretScope,
			Name:   searchName,
			Label:  searchRefLabel(searchProvider, searchName, searchExplicit),
			Source: searchRefSource(secretStore, search.SecretScope, searchName, searchExplicit),
		}}, authHandler.CurrentUser, log),
	}, nil
}

// Handler 返回带中间件的根 handler。前端由 CDN/Nginx 托管并经同源反向代理访问本服务，无需 CORS。
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.auth.Register(mux)
	a.spaceH.Register(mux)
	a.resourceH.Register(mux)
	a.sessionH.Register(mux)
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

// firstNonEmpty 返回第一个非空值，用于「系统空间配置优先、环境变量兜底」。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseSecretRef 解析 "${NAME}" 形式的引用，返回里面的名字与“是否显式书写”。
//
// 不带 ${} 的名字只查凭证存储；带 ${} 的先查存储、再退回同名环境变量。
// 之所以还要保留裸名字：界面上「平台当前使用」直接显示名字，最省事。
func parseSecretRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if len(ref) > 3 && strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}") {
		if inner := strings.TrimSpace(ref[2 : len(ref)-1]); inner != "" {
			return inner, true
		}
	}
	return ref, false
}

// searchRefLabel 是「密钥配置」弹窗里那句“平台当前使用”的文案。
func searchRefLabel(provider, name string, explicit bool) string {
	if explicit {
		return fmt.Sprintf("web_search（${%s}）", name)
	}
	return fmt.Sprintf("web_search（%s）", provider)
}

// searchRefSource 告诉管理页这条引用最终落在哪：能查到同名凭证就是 store，
// 否则是 env（此时界面上的录入区没有意义，显示成“来自环境变量”）。
func searchRefSource(store *secret.FileStore, scope, name string, explicit bool) string {
	if !explicit {
		return admin.SourceStore
	}
	if v, err := store.Get(context.Background(), scope, name); err == nil && v != "" {
		return admin.SourceStore
	}
	return admin.SourceEnv
}
