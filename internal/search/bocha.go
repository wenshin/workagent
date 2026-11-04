package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/workagent/auto-agent/internal/secret"
)

// SecretScope 是搜索凭证在 secret.Store 里的 scope 名。
const SecretScope = "search"

// KeyFunc 返回 provider 的密钥。由装配层注入（读凭证存储），
// 密钥只在本包内使用，不出现在工具参数、工具结果、日志与审计里。
type KeyFunc func(ctx context.Context) (string, error)

// Bocha 是博查（Bocha）AI 搜索的 provider。
//
// endpoint 与密钥都由平台配置决定，skill / 模型无法指定——这是白名单之外的第二道约束：
// 即使有人往配置里塞了别的域名，白名单和凭证绑定也会拦住。
type Bocha struct {
	name     string
	endpoint string
	key      KeyFunc
	guard    *Guard
}

func init() {
	RegisterFactory("bocha", func(endpoint string, key KeyFunc, guard *Guard) Provider {
		return NewBocha(endpoint, key, guard)
	})
}

// NewBocha 构造博查 provider。endpoint 必须已被 Guard 的白名单覆盖。
func NewBocha(endpoint string, key KeyFunc, guard *Guard) *Bocha {
	return &Bocha{
		name:     "bocha",
		endpoint: strings.TrimSpace(endpoint),
		key:      key,
		guard:    guard,
	}
}

// Name 实现 Provider。
func (b *Bocha) Name() string { return b.name }

type bochaItem struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	Summary       string `json:"summary"`
	DatePublished string `json:"datePublished"`
	SiteName      string `json:"siteName"`
}

type bochaResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		WebPages struct {
			Value []bochaItem `json:"value"`
		} `json:"webPages"`
	} `json:"data"`
}

// Search 实现 Provider。
func (b *Bocha) Search(ctx context.Context, req Request) ([]Result, error) {
	if b.guard == nil {
		return nil, fmt.Errorf("搜索出网通道未初始化")
	}
	if b.key == nil {
		return nil, fmt.Errorf("搜索密钥未配置")
	}
	apiKey, err := b.key(ctx)
	if err != nil {
		return nil, fmt.Errorf("搜索密钥不可用: %w", err)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("搜索密钥为空")
	}

	body := map[string]any{
		"query":   req.Query,
		"count":   req.Limit,
		"summary": true,
	}
	if req.Freshness != "" {
		body["freshness"] = req.Freshness
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(payload))
	if err != nil {
		// 这里可能带出 endpoint，但不会带出密钥
		return nil, fmt.Errorf("构造搜索请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := b.guard.Do(httpReq)
	if err != nil {
		// 统一兜底：任何错误文本里都不能出现密钥原文
		return nil, fmt.Errorf("%s", secret.Redact(err.Error(), apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s", secret.Redact(fmt.Sprintf("读取搜索响应失败: %v", err), apiKey))
	}

	var out bochaResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("搜索响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	if out.Code != 0 && out.Code != 200 {
		return nil, fmt.Errorf("搜索服务返回错误：code=%d msg=%s", out.Code, secret.Redact(out.Msg, apiKey))
	}

	items := out.Data.WebPages.Value
	results := make([]Result, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		snippet := item.Summary
		if strings.TrimSpace(snippet) == "" {
			snippet = item.Snippet
		}
		results = append(results, Result{
			Title:       truncateRunes(stripTags(item.Name), 120),
			URL:         item.URL,
			Snippet:     truncateRunes(stripTags(snippet), 400),
			PublishedAt: item.DatePublished,
			Source:      b.name,
		})
	}
	return results, nil
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// stripTags 去掉摘要里的 HTML 标签：搜索结果会原样进模型上下文，粘上标签既浪费 token
// 也可能被当成结构指令。
func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	return strings.TrimSpace(s)
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
