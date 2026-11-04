// Package openai 实现 OpenAI 兼容协议的模型适配（技术方案 §5.7.1 L1）。
//
// 一套实现覆盖 OpenAI、DeepSeek、通义兼容模式、vLLM、Ollama：
// 只需配置 base_url / api_key / model，差异仅在协议细节的容错解析上。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/llm"
)

// Kind 是协议族标识。
const Kind = "openai"

// Config 描述一个兼容端点。
type Config struct {
	Name         string
	BaseURL      string
	APIKey       string
	Model        string
	Timeout      time.Duration
	MaxTokens    int
	Capabilities []llm.Capability
}

// Provider 是 OpenAI 兼容协议实现。
type Provider struct {
	cfg  Config
	http *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New 构造 Provider；apiKey 允许为空（本地 Ollama / vLLM）。
func New(cfg Config) *Provider {
	if cfg.Timeout <= 0 {
		// 流式请求是整请求超时，缺省与主流 SDK 的 600s 对齐
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	if len(cfg.Capabilities) == 0 {
		cfg.Capabilities = []llm.Capability{llm.CapTool, llm.CapThinking, llm.CapStructure}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Provider{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Info 实现 llm.Provider。
func (p *Provider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{
		Name:         p.cfg.Name,
		Kind:         Kind,
		Model:        p.cfg.Model,
		Capabilities: p.cfg.Capabilities,
	}
}

// Chat 发起流式对话（SSE）。
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	if p.cfg.BaseURL == "" || p.cfg.Model == "" {
		return llm.Result{}, fmt.Errorf("%w: 模型服务未配置（LLM_BASE_URL / LLM_MODEL）", llm.ErrUnsupportedProvider)
	}

	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.MaxTokens
	}

	payload := map[string]any{
		"model":    model,
		"messages": req.Messages,
		"stream":   true,
	}
	if len(req.Tools) > 0 {
		payload["tools"] = req.Tools
		payload["tool_choice"] = "auto"
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return llm.Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return llm.Result{}, ctx.Err()
		}
		return llm.Result{}, &llm.ProviderError{Provider: p.cfg.Name, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return llm.Result{}, &llm.ProviderError{
			Provider: p.cfg.Name,
			Status:   resp.StatusCode,
			Message:  strings.TrimSpace(string(snippet)),
		}
	}

	res := llm.Result{Model: model}
	// 个别兼容实现忽略 stream 直接返回 JSON，这里做兜底解析
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		if err := p.parseWholeBody(resp.Body, &res, onDelta); err != nil {
			return llm.Result{}, err
		}
		return res, nil
	}

	if err := p.parseStream(ctx, resp.Body, &res, onDelta); err != nil {
		return llm.Result{}, err
	}
	return res, nil
}

func (p *Provider) parseStream(ctx context.Context, body io.Reader, res *llm.Result, onDelta llm.DeltaFunc) error {
	calls := make(map[int]*llm.ToolCall)
	order := make([]int, 0, 2)

	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &llm.ProviderError{Provider: p.cfg.Name, Err: readErr}
		}

		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, ":") && strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				break
			}

			var chunk streamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if chunk.Error != nil && chunk.Error.Message != "" {
					return &llm.ProviderError{Provider: p.cfg.Name, Message: chunk.Error.Message}
				}
				if chunk.Usage != nil {
					res.Usage = llm.Usage{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
					}
				}
				for _, choice := range chunk.Choices {
					reasoning := choice.Delta.ReasoningContent
					if reasoning == "" {
						reasoning = choice.Delta.Reasoning
					}
					delta := llm.Delta{Reasoning: reasoning, Content: choice.Delta.Content}
					res.Reasoning += delta.Reasoning
					res.Content += delta.Content
					if onDelta != nil && (delta.Reasoning != "" || delta.Content != "") {
						if err := onDelta(delta); err != nil {
							return err
						}
					}

					for _, tc := range choice.Delta.ToolCalls {
						cur, ok := calls[tc.Index]
						if !ok {
							cur = &llm.ToolCall{Type: "function"}
							calls[tc.Index] = cur
							order = append(order, tc.Index)
						}
						if tc.ID != "" {
							cur.ID = tc.ID
						}
						cur.Function.Name += tc.Function.Name
						cur.Function.Arguments += tc.Function.Arguments
					}
				}
			}
		}

		if readErr != nil {
			break
		}
	}

	for _, idx := range order {
		if call := calls[idx]; call != nil && call.Function.Name != "" {
			res.ToolCalls = append(res.ToolCalls, *call)
		}
	}
	return nil
}

// parseWholeBody 兜底解析非流式响应。
func (p *Provider) parseWholeBody(body io.Reader, res *llm.Result, onDelta llm.DeltaFunc) error {
	var resp streamChunk
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return &llm.ProviderError{Provider: p.cfg.Name, Err: fmt.Errorf("解析模型响应失败: %w", err)}
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return &llm.ProviderError{Provider: p.cfg.Name, Message: resp.Error.Message}
	}
	if resp.Usage != nil {
		res.Usage = llm.Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}

	for _, choice := range resp.Choices {
		content, reasoning := choice.Message.Content, choice.Message.ReasoningContent
		if content == "" && reasoning == "" {
			content, reasoning = choice.Delta.Content, choice.Delta.ReasoningContent
		}
		res.Content += content
		res.Reasoning += reasoning
		for _, tc := range choice.Message.ToolCalls {
			call := llm.ToolCall{ID: tc.ID, Type: "function"}
			call.Function.Name = tc.Function.Name
			call.Function.Arguments = tc.Function.Arguments
			if call.Function.Name != "" {
				res.ToolCalls = append(res.ToolCalls, call)
			}
		}
	}

	if onDelta != nil {
		if res.Reasoning != "" {
			if err := onDelta(llm.Delta{Reasoning: res.Reasoning}); err != nil {
				return err
			}
		}
		if res.Content != "" {
			if err := onDelta(llm.Delta{Content: res.Content}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Embed 调用 /embeddings。
func (p *Provider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"model": p.cfg.Model, "input": texts})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &llm.ProviderError{Provider: p.cfg.Name, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &llm.ProviderError{
			Provider: p.cfg.Name,
			Status:   resp.StatusCode,
			Message:  strings.TrimSpace(string(snippet)),
		}
	}

	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &llm.ProviderError{Provider: p.cfg.Name, Err: err}
	}

	vectors := make([][]float32, len(texts))
	for _, item := range out.Data {
		if item.Index >= 0 && item.Index < len(vectors) {
			vectors[item.Index] = item.Embedding
		}
	}
	return vectors, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}
