package llm

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
)

// Client 是 OpenAI 兼容协议的流式对话客户端。
//
// 一套实现覆盖 DeepSeek、通义的兼容模式、vLLM、Ollama 等服务：
// 只需配置 base_url / api_key / model。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New 构造客户端；apiKey 允许为空（本地 Ollama 等无鉴权服务）。
func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: timeout},
	}
}

// Model 返回当前模型名。
func (c *Client) Model() string { return c.model }

// ChatRequest 是一次对话的入参。
type ChatRequest struct {
	Messages    []Message
	Tools       []Tool
	Temperature float64
}

// Chat 发起一次流式对话。
//
// onDelta 在收到思考或正文增量时回调；当模型决定调用工具时，本轮可能只有 tool_calls
// 而没有正文，调用方需根据返回的 Result.ToolCalls 执行工具后再发起下一轮。
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (Result, error) {
	if c.baseURL == "" || c.model == "" {
		return Result{}, errors.New("模型服务未配置：请设置 LLM_BASE_URL 与 LLM_MODEL")
	}

	payload := map[string]any{
		"model":    c.model,
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

	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("连接模型服务失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Result{}, fmt.Errorf("模型服务返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// 个别兼容实现会忽略 stream 参数直接返回 JSON，这里做一次兜底解析
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		return parseWholeBody(resp.Body, onDelta)
	}

	return parseStream(resp.Body, onDelta)
}

func parseStream(body io.Reader, onDelta func(Delta) error) (Result, error) {
	var res Result
	calls := make(map[int]*ToolCall)
	order := make([]int, 0, 2)

	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Result{}, readErr
		}

		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, ":") && strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}

			var chunk streamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if chunk.Error != nil && chunk.Error.Message != "" {
					return Result{}, errors.New(chunk.Error.Message)
				}
				for _, choice := range chunk.Choices {
					reasoning := choice.Delta.ReasoningContent
					if reasoning == "" {
						reasoning = choice.Delta.Reasoning
					}
					delta := Delta{Reasoning: reasoning, Content: choice.Delta.Content}
					res.Reasoning += delta.Reasoning
					res.Content += delta.Content
					if onDelta != nil && (delta.Reasoning != "" || delta.Content != "") {
						if err := onDelta(delta); err != nil {
							return Result{}, err
						}
					}
					for _, tc := range choice.Delta.ToolCalls {
						cur, ok := calls[tc.Index]
						if !ok {
							cur = &ToolCall{Type: "function"}
							calls[tc.Index] = cur
							order = append(order, tc.Index)
						}
						if tc.ID != "" {
							cur.ID = tc.ID
						}
						if tc.Function.Name != "" {
							cur.Function.Name += tc.Function.Name
						}
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
	return res, nil
}

// parseWholeBody 兜底解析非流式响应。
func parseWholeBody(body io.Reader, onDelta func(Delta) error) (Result, error) {
	var resp streamChunk
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&resp); err != nil {
		return Result{}, fmt.Errorf("解析模型响应失败：%w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Result{}, errors.New(resp.Error.Message)
	}

	var res Result
	for _, choice := range resp.Choices {
		content, reasoning := choice.Message.Content, choice.Message.ReasoningContent
		if content == "" && reasoning == "" {
			content, reasoning = choice.Delta.Content, choice.Delta.ReasoningContent
		}
		res.Content += content
		res.Reasoning += reasoning
	}
	if onDelta != nil {
		if res.Reasoning != "" {
			if err := onDelta(Delta{Reasoning: res.Reasoning}); err != nil {
				return Result{}, err
			}
		}
		if res.Content != "" {
			if err := onDelta(Delta{Content: res.Content}); err != nil {
				return Result{}, err
			}
		}
	}
	return res, nil
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
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}
