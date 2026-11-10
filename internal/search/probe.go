package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/secret"
)

// ProbeInfo 是搜索源试跑的诊断回执。
//
// 用途是「对话开发」的闭环：模型写完配置后跑一次，从 TopKeys 就能看出
// items 路径该写 `hits.hits` 还是 `data.webPages.value`，不用猜。
type ProbeInfo struct {
	Method   string   `json:"method,omitempty"`
	URL      string   `json:"url,omitempty"`
	Status   int      `json:"status,omitempty"`
	TopKeys  []string `json:"top_keys,omitempty"`
	Items    string   `json:"items_path,omitempty"`
	Hit      int      `json:"hit"`
	Dropped  int      `json:"dropped"`
	Samples  []Result `json:"samples,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Prober 是可自诊断的 provider：声明式源实现它，脚本源退回普通 Search。
type Prober interface {
	Probe(ctx context.Context, req Request) (ProbeInfo, error)
}

// Probe 发一次真实请求并回执「请求长什么样、响应里有什么」。
//
// 与 Search 的区别只在可观测性：这里保留非 2xx 的状态码与响应体，
// 因为调试时 4xx 的业务错误体（{"code":401,"msg":"token 过期"}）比「失败」有用得多。
func (h *HTTPJSON) Probe(ctx context.Context, req Request) (ProbeInfo, error) {
	info := ProbeInfo{Items: h.spec.Response.Items}
	if h.guard == nil {
		return info, fmt.Errorf("搜索出网通道未初始化")
	}

	apiKey := ""
	if h.spec.needsSecret() {
		if h.key == nil {
			return info, fmt.Errorf("搜索密钥未配置")
		}
		v, err := h.key(ctx)
		if err != nil {
			return info, fmt.Errorf("搜索密钥不可用: %w", err)
		}
		apiKey = strings.TrimSpace(v)
	}
	if req.Query == "" {
		req.Query = "test"
	}
	if req.Limit <= 0 {
		req.Limit = 3
	}

	httpReq, err := h.buildRequest(ctx, req, apiKey)
	if err != nil {
		return info, err
	}
	info.Method = httpReq.Method
	info.URL = secret.Redact(httpReq.URL.String(), apiKey)

	resp, err := h.guard.DoRaw(httpReq)
	if err != nil {
		return info, fmt.Errorf("%s", secret.Redact(err.Error(), apiKey))
	}
	defer func() { _ = resp.Body.Close() }()
	info.Status = resp.StatusCode

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return info, fmt.Errorf("读取响应失败: %w", readErr)
	}

	var root any
	if err := json.Unmarshal(data, &root); err == nil {
		if m, ok := root.(map[string]any); ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			info.TopKeys = keys
		}
	}

	results, parseErr := h.parse(data, resp.StatusCode)
	info.Hit = len(results)
	if n := len(results); n > 3 {
		info.Samples = results[:3]
	} else {
		info.Samples = results
	}
	if raw, ok := lookup(root, h.spec.Response.Items).([]any); ok {
		if dropped := len(raw) - len(results); dropped > 0 {
			info.Dropped = dropped
			info.Warnings = append(info.Warnings, fmt.Sprintf("%d 条结果没有可用 URL，已丢弃", dropped))
		}
	}
	if parseErr != nil {
		// 同时把 top_keys 带出去：模型看到真实的顶层键就知道路径该怎么改。
		return info, parseErr
	}
	return info, nil
}

// probeProvider 对任意 provider 做试跑：能自诊断的走 Probe，否则退化为一次普通搜索。
func ProbeProvider(ctx context.Context, p Provider, query string, limit int) (ProbeInfo, error) {
	if p == nil {
		return ProbeInfo{}, fmt.Errorf("搜索源未就绪")
	}
	req := Request{Query: query, Limit: limit}
	if pr, ok := p.(Prober); ok {
		return pr.Probe(ctx, req)
	}
	results, err := p.Search(ctx, req)
	info := ProbeInfo{Hit: len(results)}
	if len(results) > 3 {
		info.Samples = results[:3]
	} else {
		info.Samples = results
	}
	return info, err
}
