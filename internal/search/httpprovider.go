package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/workagent/auto-agent/internal/secret"
)

// HTTPJSON 是配置驱动的搜索源（见 Spec）：按配置拼请求、按配置取结果。
//
// 它的存在是为了让「接一个新的 REST 搜索 API」变成改配置，而不是写 Go。
// 出网仍然只能走 Guard，密钥仍然由 KeyFunc 从凭证存储取——配置能改的只有
// 参数名与字段路径，改不了信任边界。
type HTTPJSON struct {
	name     string
	endpoint string
	spec     Spec
	key      KeyFunc
	guard    *Guard
}

// NewHTTPJSON 构造配置驱动的搜索源；配置不合法时返回错误，由启动流程直接失败。
func NewHTTPJSON(name, endpoint string, spec Spec, key KeyFunc, guard *Guard) (*HTTPJSON, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil, fmt.Errorf("endpoint 为空")
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("endpoint 不是合法 URL: %q", ep)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint 只允许 https，收到 %q", u.Scheme)
	}
	return &HTTPJSON{name: name, endpoint: ep, spec: spec, key: key, guard: guard}, nil
}

// Name 实现 Provider。
func (h *HTTPJSON) Name() string { return h.name }

// Search 实现 Provider。
func (h *HTTPJSON) Search(ctx context.Context, req Request) ([]Result, error) {
	if h.guard == nil {
		return nil, fmt.Errorf("搜索出网通道未初始化")
	}

	apiKey := ""
	if h.spec.needsSecret() {
		if h.key == nil {
			return nil, fmt.Errorf("搜索密钥未配置")
		}
		v, err := h.key(ctx)
		if err != nil {
			return nil, fmt.Errorf("搜索密钥不可用: %w", err)
		}
		apiKey = strings.TrimSpace(v)
		if apiKey == "" {
			return nil, fmt.Errorf("搜索密钥为空")
		}
	}

	httpReq, err := h.buildRequest(ctx, req, apiKey)
	if err != nil {
		return nil, err
	}

	resp, err := h.guard.Do(httpReq)
	if err != nil {
		// 任何错误文本里都不能出现密钥原文
		return nil, fmt.Errorf("%s", secret.Redact(err.Error(), apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s", secret.Redact(fmt.Sprintf("读取搜索响应失败: %v", err), apiKey))
	}
	results, err := h.parse(data, resp.StatusCode)
	if err != nil {
		return nil, fmt.Errorf("%s", secret.Redact(err.Error(), apiKey))
	}
	return results, nil
}

func (h *HTTPJSON) buildRequest(ctx context.Context, req Request, apiKey string) (*http.Request, error) {
	rs := h.spec.Request
	method := strings.ToUpper(strings.TrimSpace(rs.Method))
	if method == "" {
		method = http.MethodPost
	}

	vars := map[string]any{"query": req.Query, "limit": req.Limit}
	if req.Freshness != "" {
		vars["freshness"] = req.Freshness
	}
	if apiKey != "" {
		vars["secret"] = apiKey
	}

	headers := make(map[string]string, len(rs.Headers)+1)
	for k, v := range rs.Headers {
		headers[k] = expandString(v, vars)
	}
	if method == http.MethodPost {
		if _, ok := headerLookup(headers, "Content-Type"); !ok {
			headers["Content-Type"] = "application/json"
		}
	}
	auth := rs.Auth
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.In), "header") {
		headers[auth.Name] = auth.Prefix + apiKey
	}

	target := h.endpoint
	var body io.Reader
	if method == http.MethodGet {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("endpoint 解析失败: %w", err)
		}
		q := u.Query()
		for k, v := range rs.Params {
			q.Set(k, expandString(v, vars))
		}
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.In), "query") {
			q.Set(auth.Name, auth.Prefix+apiKey)
		}
		u.RawQuery = q.Encode()
		target = u.String()
	} else {
		payload, ok := expand(rs.Body, vars).(map[string]any)
		if !ok {
			payload = map[string]any{}
		}
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.In), "body") {
			payload[auth.Name] = auth.Prefix + apiKey
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("构造请求体失败: %w", err)
		}
		body = bytes.NewReader(b)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		// 可能带出 endpoint，但不会带出密钥
		return nil, fmt.Errorf("构造搜索请求失败: %w", err)
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}

// parse 按 ResponseSpec 从响应里取结果；取不到数组视为配置错误，明确报出来。
func (h *HTTPJSON) parse(data []byte, status int) ([]Result, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("搜索响应不是合法 JSON（HTTP %d）", status)
	}
	rs := h.spec.Response

	if rs.ErrorCode != "" {
		if v := lookup(root, rs.ErrorCode); v != nil && !h.okCode(v) {
			msg := ""
			if rs.ErrorMessage != "" {
				msg = text(lookup(root, rs.ErrorMessage))
			}
			if msg == "" {
				msg = text(v)
			}
			return nil, fmt.Errorf("搜索服务返回错误：%s", msg)
		}
	}

	raw, ok := lookup(root, rs.Items).([]any)
	if !ok {
		return nil, fmt.Errorf("响应里没找到结果数组（response.items=%s）", rs.Items)
	}

	results := make([]Result, 0, len(raw))
	for _, item := range raw {
		link := text(lookup(item, rs.URL))
		if strings.TrimSpace(link) == "" {
			continue
		}
		results = append(results, Result{
			Title:       truncateRunes(stripTags(text(lookup(item, rs.Title))), 120),
			URL:         link,
			Snippet:     truncateRunes(stripTags(text(lookup(item, rs.Snippet))), 400),
			PublishedAt: text(lookup(item, rs.PublishedAt)),
			Source:      h.name,
		})
	}
	return results, nil
}

// okCode 判断业务错误码是否表示成功；未配置 error_ok 时默认认 0 与 200。
func (h *HTTPJSON) okCode(v any) bool {
	got := text(v)
	if len(h.spec.Response.ErrorOK) == 0 {
		return got == "0" || got == "200"
	}
	for _, ok := range h.spec.Response.ErrorOK {
		if text(ok) == got {
			return true
		}
	}
	return false
}

// headerLookup 做大小写不敏感的查找：配置里写 content-type 也要认。
func headerLookup(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}
