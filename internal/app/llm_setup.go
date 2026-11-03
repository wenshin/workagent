package app

import (
	"log/slog"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/llm/anthropic"
	"github.com/workagent/auto-agent/internal/llm/openai"
)

// buildRouter 按配置装配 Provider 与路由（技术方案 §5.7.1 适配层、§5.7.3 多模型路由）。
//
// 装配放在应用层而非 llm 包，避免 llm 与具体实现子包互相 import：
// llm 只定义接口与路由，openai / anthropic 依赖 llm，app 负责把两者接起来。
func buildRouter(cfg *config.Config, log *slog.Logger) *llm.Router {
	router := llm.NewRouter(log)

	newProvider := func(name, kind, baseURL, apiKey, model string, maxTokens int, caps []llm.Capability) llm.Provider {
		if baseURL == "" || model == "" {
			return nil
		}
		switch kind {
		case anthropic.Kind:
			return anthropic.New(anthropic.Config{
				Name:         name,
				BaseURL:      baseURL,
				APIKey:       apiKey,
				Model:        model,
				Timeout:      cfg.LLMTimeout,
				MaxTokens:    maxTokens,
				Capabilities: caps,
			})
		default: // openai 兼容协议覆盖 OpenAI / DeepSeek / 通义 / vLLM / Ollama
			return openai.New(openai.Config{
				Name:         name,
				BaseURL:      baseURL,
				APIKey:       apiKey,
				Model:        model,
				Timeout:      cfg.LLMTimeout,
				MaxTokens:    maxTokens,
				Capabilities: caps,
			})
		}
	}

	chatCaps := []llm.Capability{llm.CapTool, llm.CapThinking, llm.CapStructure}

	if p := newProvider("primary", cfg.LLMProvider, cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMMaxTokens, chatCaps); p != nil {
		router.Register(p)
	}

	chatRoute := llm.Route{Primary: "primary", Required: []llm.Capability{llm.CapTool}}
	if cfg.LLMFallbackBaseURL != "" && cfg.LLMFallbackModel != "" {
		kind := cfg.LLMFallbackProvider
		if kind == "" {
			kind = cfg.LLMProvider
		}
		if p := newProvider("fallback", kind, cfg.LLMFallbackBaseURL, cfg.LLMFallbackAPIKey, cfg.LLMFallbackModel, cfg.LLMMaxTokens, chatCaps); p != nil {
			router.Register(p)
			chatRoute.Fallbacks = []string{"fallback"}
		}
	}
	router.SetRoute(llm.RouteChat, chatRoute)

	// 摘要用小模型（§5.7.2 上下文压缩）；未单独配置时复用主模型
	summaryRoute := llm.Route{Primary: "primary"}
	if cfg.LLMSummaryModel != "" {
		if p := newProvider("summary", cfg.LLMProvider, cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMSummaryModel, 1024, nil); p != nil {
			router.Register(p)
			summaryRoute = llm.Route{Primary: "summary", Model: cfg.LLMSummaryModel}
		}
	}
	router.SetRoute(llm.RouteSummary, summaryRoute)

	// 向量模型（§5.7.4）：统一走 OpenAI 兼容的 /embeddings
	if cfg.LLMEmbedBaseURL != "" && cfg.LLMEmbedModel != "" {
		if p := newProvider("embedding", "openai", cfg.LLMEmbedBaseURL, cfg.LLMEmbedAPIKey, cfg.LLMEmbedModel, 0, []llm.Capability{llm.CapEmbedding}); p != nil {
			router.Register(p)
			router.SetRoute(llm.RouteEmbedding, llm.Route{
				Primary:  "embedding",
				Required: []llm.Capability{llm.CapEmbedding},
			})
		}
	}

	return router
}
