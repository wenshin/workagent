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

// DefaultSecretName 是平台凭证的统一名字。
//
// 凭证名不跟 provider 绑定：同一时刻只有一个搜索源在用，一把密钥就够。
// 换源只改 provider / endpoint，密钥名与环境变量（SEARCH_API_KEY）都不用动。
const DefaultSecretName = "default"

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

// Registry 保存可用搜索源，并按「空间 + 语义 scope」路由。
//
// 它不认识厂商：每个源是一份 SourceDef（适配器 + scope + 优先级 + 作用空间），
// 内置 provider、声明式 spec、脚本 script 都只是 SourceDef.Provider 的不同实现。
type Registry struct {
	mu        sync.RWMutex
	sources   map[string]*SourceDef
	defaultID string
}

// NewRegistry 构造注册表；defaultID 是旧版单源配置里的 provider 名，
// 多源之后它只用于展示「默认源」，不再参与路由。
func NewRegistry(defaultID string) *Registry {
	return &Registry{sources: map[string]*SourceDef{}, defaultID: strings.TrimSpace(defaultID)}
}

// Register 把一个 provider 注册为全局 external 源（内置 provider 的接线方式）。
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	_ = r.UpsertSource(SourceDef{
		Name: p.Name(), Scope: ScopeExternal, Priority: 100, Provider: p, Origin: "builtin",
	})
}

// UpsertSource 注册或覆盖一个源；同名同空间覆盖，不同空间互不影响。
//
// 这是「装完即生效」的落点：插件安装/启用后由上层调用，不需要重启。
func (r *Registry) UpsertSource(def SourceDef) error {
	name := strings.TrimSpace(def.Name)
	if name == "" {
		return fmt.Errorf("搜索源缺少 name")
	}
	if def.Provider == nil {
		return fmt.Errorf("搜索源 %s 没有可用的适配器", name)
	}
	def.Name = name
	def.SpaceID = strings.TrimSpace(def.SpaceID)
	def.Scope = def.scope()
	if def.Origin == "" {
		def.Origin = "plugin"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sources == nil {
		r.sources = map[string]*SourceDef{}
	}
	copy := def
	r.sources[def.key()] = &copy
	return nil
}

// RemoveSource 移除一个源；停用/卸载插件时调用，立刻生效。
func (r *Registry) RemoveSource(spaceID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sources, strings.TrimSpace(spaceID)+"\x00"+name)
}

// Sources 返回全部已注册源的只读视图，顺序稳定（便于日志对比）。
func (r *Registry) Sources() []SourceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SourceInfo, 0, len(r.sources))
	for _, def := range r.sources {
		out = append(out, def.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SpaceID != out[j].SpaceID {
			return out[i].SpaceID < out[j].SpaceID
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Names 返回已注册的源名，供提示词与错误提示使用。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.sources))
	for _, def := range r.sources {
		out = append(out, def.Name)
	}
	sort.Strings(out)
	return out
}

// Default 返回默认源名；没有源时返回空。
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.sources[strings.TrimSpace("")+"\x00"+r.defaultID]; ok {
		return r.defaultID
	}
	names := make([]string, 0, len(r.sources))
	for _, def := range r.sources {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// Search 按空间与语义 scope 检索：scope 为空或 auto 时内网优先、公网补充。
func (r *Registry) Search(ctx context.Context, spaceID, scope string, req Request) ([]Result, error) {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return nil, fmt.Errorf("检索词不能为空")
	}
	req.Limit = clampLimit(req.Limit)
	return r.searchSources(ctx, spaceID, scope, req)
}

// SearchByName 精确调用一个源，用于试跑与诊断（不参与合并）。
func (r *Registry) SearchByName(ctx context.Context, spaceID, name string, req Request) ([]Result, error) {
	req.Limit = clampLimit(req.Limit)
	for _, def := range r.candidates(spaceID, ScopeAuto) {
		if def.Name == name {
			return callWithTimeout(ctx, def.timeout(), def.Provider, req)
		}
	}
	return nil, fmt.Errorf("没有名为 %q 的搜索源", name)
}
