package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// 内置路由名，与 Harness 的调用点一一对应。
const (
	RouteChat      = "chat"
	RouteSummary   = "summary"
	RouteEmbedding = "embedding"
)

// Route 是一条模型路由：主模型 + 降级链 + 能力要求。
type Route struct {
	Primary   string
	Fallbacks []string
	// Model 覆盖 Provider 的默认模型（摘要、轻量任务用小模型）
	Model string
	// Required 是必须具备的能力标签，缺任何一个都不参与路由
	Required []Capability
}

// Router 是 ModelRouter（§5.7.3）：按路由选模型，主模型失败时切备模型。
//
// 降级约束：只有协议族一致（schema 兼容）才降级，否则报错而非静默降质；
// 流式响应已向前端输出内容后不再降级，避免重复文本。
type Router struct {
	providers map[string]Provider
	routes    map[string]Route
	log       *slog.Logger
}

// NewRouter 构造空路由表。
func NewRouter(log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{
		providers: make(map[string]Provider),
		routes:    make(map[string]Route),
		log:       log,
	}
}

// Register 注册一个 Provider；同名覆盖。
func (r *Router) Register(p Provider) {
	if p == nil {
		return
	}
	r.providers[p.Info().Name] = p
}

// SetRoute 设置路由。
func (r *Router) SetRoute(name string, route Route) { r.routes[name] = route }

// Provider 返回已注册的 Provider。
func (r *Router) Provider(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Ready 报告 chat 路由是否至少有一个可用 Provider。
func (r *Router) Ready() bool {
	route, ok := r.routes[RouteChat]
	if !ok {
		return false
	}
	for _, name := range append([]string{route.Primary}, route.Fallbacks...) {
		if _, ok := r.providers[name]; ok {
			return true
		}
	}
	return false
}

// Model 返回路由实际使用的模型名，用于前端提示。
func (r *Router) Model(routeName string) string {
	route, ok := r.routes[routeName]
	if !ok || route.Primary == "" {
		return ""
	}
	if route.Model != "" {
		return route.Model
	}
	if p, ok := r.providers[route.Primary]; ok {
		return p.Info().Model
	}
	return ""
}

// Chat 按路由发起流式对话，返回实际命中的 Provider 名。
func (r *Router) Chat(ctx context.Context, routeName string, req ChatRequest, onDelta DeltaFunc) (Result, string, error) {
	route, ok := r.routes[routeName]
	if !ok {
		return Result{}, "", fmt.Errorf("未配置模型路由：%s", routeName)
	}

	candidates := make([]string, 0, len(route.Fallbacks)+1)
	candidates = append(candidates, route.Primary)
	candidates = append(candidates, route.Fallbacks...)

	var lastErr error
	for idx, name := range candidates {
		provider, ok := r.providers[name]
		if !ok || provider == nil {
			continue
		}

		info := provider.Info()
		if missing := missingCaps(info, route.Required); len(missing) > 0 {
			lastErr = fmt.Errorf("模型 %s 缺少能力 %v", name, missing)
			r.log.Warn("模型能力不足，跳过", "provider", name, "missing", missing)
			continue
		}

		// 同协议族才允许降级：anthropic 与 openai 的参数结构不同，静默切换会丢上下文
		if idx > 0 {
			primaryKind := ""
			if primary, ok := r.providers[route.Primary]; ok {
				primaryKind = primary.Info().Kind
			}
			if primaryKind != "" && info.Kind != primaryKind {
				return Result{}, name, lastErr
			}
		}

		attempt := req
		if attempt.Model == "" {
			attempt.Model = route.Model
		}

		emitted := false
		wrapped := func(d Delta) error {
			if onDelta == nil {
				return nil
			}
			if err := onDelta(d); err != nil {
				return err
			}
			emitted = true
			return nil
		}

		res, err := provider.Chat(ctx, attempt, wrapped)
		if err == nil {
			if res.Model == "" {
				res.Model = info.Model
			}
			if idx > 0 {
				r.log.Info("已降级到备用模型", "route", routeName, "provider", name, "from", route.Primary)
			}
			return res, name, nil
		}

		lastErr = err
		// 已经产出的内容无法回滚，不能再换模型重放
		if emitted {
			return Result{}, name, err
		}
		if !Retryable(err) {
			return Result{}, name, err
		}
		if idx == len(candidates)-1 {
			return Result{}, name, err
		}
		r.log.Warn("模型调用失败，尝试降级", "route", routeName, "provider", name, "err", err)
	}

	if lastErr == nil {
		lastErr = errors.New("没有可用的模型 Provider")
	}
	return Result{}, "", lastErr
}

// Embed 计算向量，仅用于 embedding 路由。
func (r *Router) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	route, ok := r.routes[RouteEmbedding]
	if !ok {
		return nil, errors.New("未配置向量模型（LLM_EMBED_MODEL）")
	}
	provider, ok := r.providers[route.Primary]
	if !ok {
		return nil, fmt.Errorf("向量 Provider 未注册：%s", route.Primary)
	}
	return provider.Embed(ctx, texts)
}

// EmbeddingReady 报告向量检索是否可用。
func (r *Router) EmbeddingReady() bool {
	route, ok := r.routes[RouteEmbedding]
	if !ok {
		return false
	}
	_, ok = r.providers[route.Primary]
	return ok
}

func missingCaps(info ProviderInfo, required []Capability) []Capability {
	if len(required) == 0 {
		return nil
	}
	missing := make([]Capability, 0, len(required))
	for _, c := range required {
		if !info.Has(c) {
			missing = append(missing, c)
		}
	}
	return missing
}

// Describe 返回路由的可读描述，用于启动日志与 /agent/info。
func (r *Router) Describe() string {
	parts := make([]string, 0, len(r.routes))
	for name, route := range r.routes {
		chain := append([]string{route.Primary}, route.Fallbacks...)
		parts = append(parts, fmt.Sprintf("%s=%s", name, strings.Join(chain, "→")))
	}
	return strings.Join(parts, ", ")
}
