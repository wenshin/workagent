// Package mcp 实现 MCP Server 客户端与工具发现（技术方案 §5.3.1 / §5.4）。
//
// 只支持 streamable_http 传输：JSON-RPC 2.0 over HTTP POST，
// 响应可能是 application/json，也可能是 text/event-stream。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// protocolVersion 使用当前主流版本，服务端不接受时以其返回值为准。
const protocolVersion = "2024-11-05"

// Client 是一个 MCP Server 的连接。
type Client struct {
	name       string
	endpoint   string
	authHeader string
	secret     string
	http       *http.Client
	sessionID  string
	nextID     atomic.Int64
}

// NewClient 构造客户端；secret 为空表示不需要鉴权。
func NewClient(name, endpoint, authHeader, secret string) *Client {
	return &Client{
		name:       name,
		endpoint:   endpoint,
		authHeader: authHeader,
		secret:     secret,
		http:       &http.Client{Timeout: 20 * time.Second},
	}
}

// ToolDef 是远端工具的描述。
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Initialize 完成握手并保存会话 ID。
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "workagent", "version": "0.1.0"},
	}
	if _, err := c.do(ctx, "initialize", params); err != nil {
		return fmt.Errorf("MCP 初始化失败（%s）: %w", c.name, err)
	}
	// 握手完成的确认通知，无返回体
	_ = c.notify(ctx, "notifications/initialized", map[string]any{})
	return nil
}

// ListTools 拉取工具清单。
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	raw, err := c.do(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 tools/list 失败: %w", err)
	}
	return out.Tools, nil
}

// CallTool 调用远端工具，返回拼接后的文本结果。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return c.callTool(ctx, name, authOverride{}, args)
}

// CallToolWithAuth 用本次调用专属的鉴权头覆盖构造时的静态凭证。
//
// MCP 插件的 auth.mode=jwt 走这里：每次调用都按当前用户现签一个短寿命
// 令牌，连接本身仍复用（session 已经握过手），只有这一个请求换成用户身份。
func (c *Client) CallToolWithAuth(ctx context.Context, name, header, value string, args json.RawMessage) (string, error) {
	return c.callTool(ctx, name, authOverride{header: header, value: value}, args)
}

func (c *Client) callTool(ctx context.Context, name string, auth authOverride, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var arguments map[string]any
	if err := json.Unmarshal(args, &arguments); err != nil {
		return "", fmt.Errorf("工具参数不是合法 JSON 对象: %w", err)
	}

	raw, err := c.doWith(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, auth)
	if err != nil {
		return "", err
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 tools/call 失败: %w", err)
	}

	parts := make([]string, 0, len(out.Content))
	for _, item := range out.Content {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	result := strings.Join(parts, "\n")
	if out.IsError {
		return result, fmt.Errorf("MCP 工具返回错误: %s", result)
	}
	if result == "" {
		result = string(raw)
	}
	return result, nil
}

func (c *Client) do(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.doWith(ctx, method, params, authOverride{})
}

func (c *Client) doWith(ctx context.Context, method string, params any, auth authOverride) (json.RawMessage, error) {
	id := int(c.nextID.Add(1))
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	resp, err := c.postWith(ctx, payload, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("MCP 服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return json.RawMessage(`{}`), nil
	}

	data := body
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = extractSSE(body)
	}

	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &rpc); err != nil {
		return nil, fmt.Errorf("解析 MCP 响应失败: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("MCP 错误 %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if len(rpc.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return rpc.Result, nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	resp, err := c.post(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

func (c *Client) post(ctx context.Context, payload any) (*http.Response, error) {
	return c.postWith(ctx, payload, authOverride{})
}

// authOverride 是单次请求的鉴权覆盖；两个字段都为空时沿用构造时的静态凭证。
type authOverride struct {
	header string
	value  string
}

func (c *Client) postWith(ctx context.Context, payload any, auth authOverride) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.authHeader != "" && c.secret != "" {
		req.Header.Set(c.authHeader, c.secret)
	}
	// 覆盖必须放在静态凭证之后：同一 header 名以本次调用的身份为准。
	if auth.header != "" && auth.value != "" {
		req.Header.Set(auth.header, auth.value)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	return c.http.Do(req)
}

// extractSSE 从事件流中取出最后一个 JSON-RPC 数据帧。
func extractSSE(body []byte) []byte {
	var last []byte
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if strings.HasPrefix(data, "{") {
			last = []byte(data)
		}
	}
	if last == nil {
		return body
	}
	return last
}
