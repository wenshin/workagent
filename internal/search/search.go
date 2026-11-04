// Package search 是「通用搜索」的能力层（技术方案 §5.6.1 search.query）。
//
// 与工具层解耦：这里只定义 provider 契约、归一化结果与出网防护，具体工具
// （web_search）在 internal/tool。新增搜索源只需实现 Provider 并注册，
// 工具、提示词、审计链路都不用改。
package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Request 是一次搜索请求；provider 可忽略自己不支持的字段。
type Request struct {
	Query string
	Limit int
	// Freshness 时间范围：空 / day / week / month
	Freshness string
}

// Result 是归一化后的搜索结果，与具体 provider 无关。
// URL 必须保留：回答里的引用靠它回溯，前端据此渲染可点链接。
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Source      string `json:"source,omitempty"`
}

// Provider 是一个可插拔的搜索实现。
//
// 约束：实现内部只能通过 Guard 出网，密钥通过 KeyFunc 从凭证存储取，
// 不得接受来自模型/配置文件的 endpoint 或密钥参数。
type Provider interface {
	Name() string
	Search(ctx context.Context, req Request) ([]Result, error)
}

// 默认与上限，和工具 schema 的说明保持一致。
const (
	defaultLimit = 8
	maxLimit     = 20
)

// Registry 保存已启用的 provider，并按名称选择。
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	defaultID string
}

// NewRegistry 构造注册表；defaultID 为空时取注册顺序里的第一个。
func NewRegistry(defaultID string) *Registry {
	return &Registry{providers: map[string]Provider{}, defaultID: strings.TrimSpace(defaultID)}
}

// Register 注册 provider，同名覆盖。
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = map[string]Provider{}
	}
	r.providers[p.Name()] = p
}

// Names 返回已注册的 provider 名，供提示词与错误提示使用。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Default 返回默认 provider 名；配置的名字不可用时回退到第一个。
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.providers[r.defaultID]; ok {
		return r.defaultID
	}
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// Search 按名称调用 provider：name 为空用默认值，未知名称返回明确错误，
// 便于模型知道「有哪几个源可用」，而不是静默换源导致结果与预期不符。
func (r *Registry) Search(ctx context.Context, name string, req Request) ([]Result, error) {
	wanted := strings.TrimSpace(name)

	r.mu.RLock()
	if wanted == "" {
		wanted = r.defaultID
	}
	p := r.providers[wanted]
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	r.mu.RUnlock()

	if p == nil {
		if len(names) == 0 {
			return nil, fmt.Errorf("没有可用的搜索源，请联系管理员配置")
		}
		sort.Strings(names)
		return nil, fmt.Errorf("没有名为 %q 的搜索源，可用：%s", wanted, strings.Join(names, " / "))
	}

	if req.Limit <= 0 {
		req.Limit = defaultLimit
	}
	if req.Limit > maxLimit {
		req.Limit = maxLimit
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return nil, fmt.Errorf("检索词不能为空")
	}
	return p.Search(ctx, req)
}
