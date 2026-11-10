package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 搜索源的语义范围。
//
// 让模型用「内网 / 公网」这种能力语义选源，而不是厂商名：公司今天用博查、
// 明天换 Tavily，scope 不变，工具 schema 与提示词都不用改。
const (
	ScopeInternal = "internal"
	ScopeExternal = "external"
	// ScopeAuto 表示按策略自动路由：内网优先，内网没有再用公网。
	ScopeAuto = "auto"
)

// SourceDef 是一个可用的搜索源。
//
// 来源有两类，形状完全一致，只是谁能看见不同：
//   - 平台配置（config.json 的 search.sources）：全局，部署级；
//   - source 插件（kind=source）：装到系统空间即全局，装到普通空间只对该空间生效。
type SourceDef struct {
	// Name 是源标识（插件 slug 或配置里的 name），用于日志、审计与前端展示
	Name string
	// Scope 是能力语义：internal / external
	Scope string
	// Endpoint 是出网终点；Guard 的白名单由它的 host 推导
	Endpoint string
	// Secret 是凭证名（search scope），空表示这个源不需要密钥
	Secret string
	// AllowPrivate 放开「http + 私网地址」，只用于内网搜索源
	AllowPrivate bool
	// Priority 越小越优先
	Priority int
	// Timeout 是单次搜索的墙钟，零值取默认
	Timeout time.Duration
	// Provider 是已构造好的适配器（内置 / 声明式 / 脚本）
	Provider Provider
	// SpaceID 为空表示全局可用
	SpaceID string
	// Origin 是来源标记：config / plugin / builtin，仅用于展示与排错
	Origin string
}

func (d SourceDef) scope() string {
	s := strings.ToLower(strings.TrimSpace(d.Scope))
	if s == "" {
		return ScopeExternal
	}
	return s
}

func (d SourceDef) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 8 * time.Second
}

func (d SourceDef) key() string { return strings.TrimSpace(d.SpaceID) + "\x00" + d.Name }

// SourceInfo 是源的只读视图，给管理界面与启动日志用；不含密钥与适配器实例。
type SourceInfo struct {
	Name         string `json:"name"`
	Scope        string `json:"scope"`
	Endpoint     string `json:"endpoint"`
	Secret       string `json:"secret,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
	Priority     int    `json:"priority"`
	SpaceID      string `json:"space_id,omitempty"`
	Origin       string `json:"origin"`
}

// Info 转成只读视图。
func (d SourceDef) Info() SourceInfo {
	return SourceInfo{
		Name: d.Name, Scope: d.scope(), Endpoint: d.Endpoint, Secret: d.Secret,
		AllowPrivate: d.AllowPrivate, Priority: d.Priority, SpaceID: d.SpaceID, Origin: d.Origin,
	}
}

// Normalize 是结果归一化的唯一出口：spec 与 script 两条路径都走它。
//
// 两条路径各自截断会让「换实现方式」悄悄改变模型看到的内容，所以必须收口。
// 三条硬约束：URL 必须能回溯（否则丢弃），标题空则退化用 URL，其余字段可空。
func Normalize(r Result) (Result, bool) {
	url := strings.TrimSpace(r.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return Result{}, false
	}
	title := strings.TrimSpace(stripTags(r.Title))
	if title == "" {
		title = url
	}
	return Result{
		Title:       truncateRunes(title, 300),
		URL:         truncateRunes(url, 2048),
		Snippet:     truncateRunes(strings.TrimSpace(stripTags(r.Snippet)), 2000),
		PublishedAt: truncateRunes(strings.TrimSpace(r.PublishedAt), 64),
		Source:      truncateRunes(strings.TrimSpace(r.Source), 64),
	}, true
}

// candidates 收集本次调用可用的源：本空间 + 全局，空间同名覆盖全局。
//
// 排序决定「内网优先、公网补充」：scope=auto 时 internal 全部排在前面，
// 各自再按 priority、name 稳定排序，避免同一份配置两次搜索顺序不同。
func (r *Registry) candidates(spaceID, scope string) []SourceDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byName := map[string]SourceDef{}
	for key, def := range r.sources {
		owner := ""
		if i := strings.IndexByte(key, 0); i >= 0 {
			owner = key[:i]
		}
		// 全局源先放进去，空间自己的同名源覆盖它。
		if owner == "" {
			byName[def.Name] = *def
		}
	}
	if sid := strings.TrimSpace(spaceID); sid != "" {
		for key, def := range r.sources {
			if strings.HasPrefix(key, sid+"\x00") {
				byName[def.Name] = *def
			}
		}
	}

	want := strings.ToLower(strings.TrimSpace(scope))
	out := make([]SourceDef, 0, len(byName))
	for _, def := range byName {
		switch want {
		case "", ScopeAuto:
			// 全都要
		case def.scope():
			// 命中
		default:
			continue
		}
		out = append(out, def)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if want == "" || want == ScopeAuto {
			si, sj := scopeRank(out[i].scope()), scopeRank(out[j].scope())
			if si != sj {
				return si < sj
			}
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func scopeRank(scope string) int {
	if scope == ScopeInternal {
		return 0
	}
	return 1
}

// searchSources 按候选顺序依次调用，边取边去重，凑够 limit 就停。
//
// 不用并发：企业场景一个 scope 下通常 1-2 个源，串行换来的行为可预期
// （内网一定先出结果）比省那几百毫秒重要。单源失败不打断整体，
// 只有全部失败且没有结果时才把错误汇总抛给模型。
func (r *Registry) searchSources(ctx context.Context, spaceID, scope string, req Request) ([]Result, error) {
	defs := r.candidates(spaceID, scope)
	if len(defs) == 0 {
		return nil, fmt.Errorf("没有可用的搜索源，请管理员在插件市场安装或配置搜索源")
	}
	limit := clampLimit(req.Limit)
	req.Limit = limit

	out := make([]Result, 0, limit)
	seen := map[string]bool{}
	errs := make([]string, 0, len(defs))
	for _, def := range defs {
		if len(out) >= limit {
			break
		}
		if def.Provider == nil {
			continue
		}
		sub := req
		sub.Limit = limit - len(out)

		results, err := callWithTimeout(ctx, def.timeout(), def.Provider, sub)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", def.Name, err))
			continue
		}
		for _, item := range results {
			norm, ok := Normalize(item)
			if !ok {
				continue
			}
			if seen[norm.URL] {
				continue
			}
			seen[norm.URL] = true
			out = append(out, norm)
			if len(out) >= limit {
				break
			}
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("搜索失败：%s", strings.Join(errs, "；"))
	}
	return out, nil
}

func clampLimit(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// callWithTimeout 给单个源套上自己的墙钟：一个源卡住不应该拖垮整轮搜索。
func callWithTimeout(ctx context.Context, d time.Duration, p Provider, req Request) ([]Result, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return p.Search(cctx, req)
}
