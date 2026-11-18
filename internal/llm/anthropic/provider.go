// Package anthropic 实现 Anthropic Messages 原生协议适配（技术方案 §5.7.1 L1）。
//
// 与 OpenAI 兼容协议的差异：system 是顶层参数；工具参数是 JSON 对象（tool_use.input）；
// 工具结果是 user 角色的 tool_result 块且需合并；流式事件为 content_block_*。
package anthropic

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
const Kind = "anthropic"

const apiVersion = "2023-06-01"

// Config 描述一个 Anthropic 端点。
type Config struct {
	Name         string
	BaseURL      string // 例如 https://api.anthropic.com
	APIKey       string
	Model        string
	Timeout      time.Duration
	MaxTokens    int
	Capabilities []llm.Capability
}

// Provider 是 Anthropic 协议实现。
type Provider struct {
	cfg  Config
	http *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New 构造 Provider。
func New(cfg Config) *Provider {
	if cfg.Timeout <= 0 {
		// 流式请求是整请求超时，缺省与主流 SDK 的 600s 对齐
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Name == "" {
		cfg.Name = "anthropic"
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if len(cfg.Capabilities) == 0 {
		cfg.Capabilities = []llm.Capability{llm.CapTool, llm.CapThinking, llm.CapVision, llm.CapStructure}
	}
	cfg.BaseURL = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
	return &Provider{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Info 实现 llm.Provider。
func (p *Provider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: p.cfg.Name, Kind: Kind, Model: p.cfg.Model, Capabilities: p.cfg.Capabilities}
}

// Chat 发起流式对话。
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	if p.cfg.BaseURL == "" || p.cfg.Model == "" {
		return llm.Result{}, fmt.Errorf("%w: Anthropic 端点未配置", llm.ErrUnsupportedProvider)
	}

	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.MaxTokens
	}

	system, messages := convertMessages(req.Messages)
	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   messages,
		"stream":     true,
	}
	if system != "" {
		payload["system"] = system
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if tools := convertTools(req.Tools); len(tools) > 0 {
		payload["tools"] = tools
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return llm.Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return llm.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("x-api-key", p.cfg.APIKey)
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
	if err := p.parseStream(ctx, resp.Body, &res, onDelta); err != nil {
		return llm.Result{}, err
	}
	return res, nil
}

func (p *Provider) parseStream(ctx context.Context, body io.Reader, res *llm.Result, onDelta llm.DeltaFunc) error {
	tools := make(map[int]*toolBufferRef)
	order := make([]int, 0, 2)

	reader := bufio.NewReaderSize(body, 64*1024)
	eventName := ""

	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &llm.ProviderError{Provider: p.cfg.Name, Err: readErr}
		}

		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			// 事件边界
		case strings.HasPrefix(trimmed, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data != "" {
				if err := p.handleEvent(eventName, data, res, onDelta, tools, &order); err != nil {
					return err
				}
			}
		}

		if readErr != nil {
			break
		}
	}

	for _, idx := range order {
		buf := tools[idx]
		if buf == nil || buf.name == "" {
			continue
		}
		args := buf.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		call := llm.ToolCall{ID: buf.id, Type: "function"}
		call.Function.Name = buf.name
		call.Function.Arguments = args
		res.ToolCalls = append(res.ToolCalls, call)
	}
	return nil
}

func (p *Provider) handleEvent(
	eventName, data string,
	res *llm.Result,
	onDelta llm.DeltaFunc,
	tools map[int]*toolBufferRef,
	order *[]int,
) error {
	var payload ssePayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}
	if payload.Error != nil && payload.Error.Message != "" {
		return &llm.ProviderError{Provider: p.cfg.Name, Message: payload.Error.Message}
	}

	switch eventName {
	case "message_start":
		if payload.Message != nil {
			if payload.Message.Model != "" {
				res.Model = payload.Message.Model
			}
			res.Usage.InputTokens = payload.Message.Usage.InputTokens
		}
	case "content_block_start":
		if payload.ContentBlock != nil && payload.ContentBlock.Type == "tool_use" {
			tools[payload.Index] = &toolBufferRef{
				id:   payload.ContentBlock.ID,
				name: payload.ContentBlock.Name,
			}
			*order = append(*order, payload.Index)
		}
	case "content_block_delta":
		if payload.Delta == nil {
			return nil
		}
		switch payload.Delta.Type {
		case "text_delta":
			res.Content += payload.Delta.Text
			if onDelta != nil && payload.Delta.Text != "" {
				return onDelta(llm.Delta{Content: payload.Delta.Text})
			}
		case "thinking_delta":
			res.Reasoning += payload.Delta.Thinking
			if onDelta != nil && payload.Delta.Thinking != "" {
				return onDelta(llm.Delta{Reasoning: payload.Delta.Thinking})
			}
		case "input_json_delta":
			if buf, ok := tools[payload.Index]; ok {
				buf.args.WriteString(payload.Delta.PartialJSON)
			}
		}
	case "message_delta":
		if payload.Usage != nil {
			res.Usage.OutputTokens = payload.Usage.OutputTokens
		}
		if payload.Delta != nil && payload.Delta.StopReason != "" {
			res.FinishReason = normalizeStopReason(payload.Delta.StopReason)
		}
	}
	return nil
}

// normalizeStopReason 把 Anthropic 的停止原因对齐成 llm.Result.FinishReason 的取值：
// 上限截断在这边叫 max_tokens，与 openai 的 length 是同一件事。
func normalizeStopReason(s string) string {
	if s == "max_tokens" {
		return llm.FinishLength
	}
	return s
}

// Embed Anthropic 无 Embedding 接口，需单独配置向量 Provider。
func (p *Provider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("%w: Anthropic 无 Embedding 接口，请配置 LLM_EMBED_*", llm.ErrUnsupportedProvider)
}

// ---- 协议结构 ----

type toolBufferRef struct {
	id   string
	name string
	args strings.Builder
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type apiMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type apiTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ssePayload struct {
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		// StopReason 在 message_delta 事件里给出：max_tokens 表示被上限截断
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message *struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// convertMessages 把统一消息格式转成 Anthropic 的 system 与 messages。
func convertMessages(msgs []llm.Message) (string, []apiMessage) {
	system := make([]string, 0, 1)
	out := make([]apiMessage, 0, len(msgs))

	appendBlocks := func(role string, blocks ...contentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, apiMessage{Role: role, Content: blocks})
	}

	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case llm.RoleUser:
			appendBlocks("user", contentBlock{Type: "text", Text: m.Content})
		case llm.RoleAssistant:
			blocks := make([]contentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage("{}")
				if args := strings.TrimSpace(tc.Function.Arguments); args != "" && json.Valid([]byte(args)) {
					input = json.RawMessage(args)
				}
				blocks = append(blocks, contentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			appendBlocks("assistant", blocks...)
		case llm.RoleTool:
			appendBlocks("user", contentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content})
		}
	}

	return strings.Join(system, "\n\n"), out
}

func convertTools(tools []llm.Tool) []apiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]apiTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, apiTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}
