package plugin

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/store"
)

// SecretScope 是工具 / MCP 插件用 capabilities 里的 secret.use 声明的凭证所在的 scope。
// 搜索源不走它：那条路是 source.secret + search.SecretScope。
const SecretScope = "plugin"

// SecretKey 是凭证在「使用中」统计里的键：与 admin 接口的寻址一致，scope/name。
func SecretKey(scope, name string) string { return scope + "/" + name }

// SecretRef 是清单声明的一处凭证引用：scope 是它在凭证存储里的地址，
// name 是 source.secret / secret.use / auth.secret 里写的那个名字。
type SecretRef struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
}

// SecretRefs 返回清单声明的全部凭证（去重、稳定排序）。
//
// 三处声明都要算上，少一处就会出现「装好了但用不了，界面上却什么都不说」：
//   - source.secret：搜索源的 API Key（scope=search）
//   - capabilities[].secret.use：脚本 secret.get 能取到的名字（scope=plugin）
//   - auth.secret：MCP 握手的预置凭证（scope=plugin）
//
// auth.mode=jwt 不算：那把密钥由平台现签（mcp_jwt scope），用户不用录入。
func (m Manifest) SecretRefs() []SecretRef {
	seen := map[string]bool{}
	out := make([]SecretRef, 0, 4)
	add := func(scope, name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := SecretKey(scope, name)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, SecretRef{Scope: scope, Name: name})
	}

	if m.Source != nil {
		add(search.SecretScope, m.Source.Secret)
	}
	if m.Auth != nil && !m.Auth.JWT() {
		add(SecretScope, m.Auth.Secret)
	}
	for _, c := range m.Capabilities {
		if c.Method == MethodSecret {
			add(SecretScope, c.Name)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SecretState 是清单声明的一处凭证及其配置状态，随插件列表下发给卡片。
type SecretState struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	// Configured 为 false 时这个插件实际不可用：搜索源查不到、secret.get 失败、
	// MCP 握手失败。卡片据此显示「未启用 + 原因」并给出录入入口。
	Configured bool `json:"configured"`
}

// SecretLister 是凭证存储的「列名字」侧能力：只回答配没配，不返回明文。
type SecretLister interface {
	SecretNames(ctx context.Context, scope string) ([]string, error)
}

// secretStates 回答「清单声明的凭证都配了吗」。
//
// 只查名字、不取明文：列表每次刷新都会问，不能用 Get 去试（那会解密一遍）。
// cache 按 scope 复用，一次列表请求对每个 scope 只读一次存储。
func (m *Manager) secretStates(ctx context.Context, meta Manifest, cache map[string]map[string]bool) ([]SecretState, []string) {
	refs := meta.SecretRefs()
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]SecretState, 0, len(refs))
	missing := make([]string, 0, len(refs))
	for _, ref := range refs {
		configured := false
		if m.o.Secrets != nil {
			set, ok := cache[ref.Scope]
			if !ok {
				set = map[string]bool{}
				if names, err := secretNames(ctx, m.o.Secrets, ref.Scope); err == nil {
					for _, name := range names {
						set[name] = true
					}
				}
				cache[ref.Scope] = set
			}
			configured = set[ref.Name]
		}
		out = append(out, SecretState{Scope: ref.Scope, Name: ref.Name, Configured: configured})
		if !configured {
			missing = append(missing, SecretKey(ref.Scope, ref.Name))
		}
	}
	return out, missing
}

// secretNames 只列名字。存储没实现列举能力（或未启用）时返回错误，
// 调用方按「一条都没有」处理——相比用 Get 逐个试，宁可不解密。
func secretNames(ctx context.Context, store Secrets, scope string) ([]string, error) {
	if lister, ok := store.(SecretLister); ok {
		return lister.SecretNames(ctx, scope)
	}
	return nil, errors.New("凭证存储不支持列举凭证名")
}

// SecretUsage 汇总「当前正在生效的插件」声明了哪些凭证。
//
// 为什么现算而不是启动时缓存一份：安装 / 停用 / 卸载都会改变引用关系，
// 缓存只会让密钥配置页上的「使用中」长期是错的。口径是已启用的安装记录
// （取安装版本自己的清单，而不是最新版）+ 已启用的本地草稿。
func (m *Manager) SecretUsage(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	collect := func(meta Manifest) {
		for _, ref := range meta.SecretRefs() {
			out[SecretKey(ref.Scope, ref.Name)] = true
		}
	}

	installs, err := m.o.Store.ListAllPluginInstalls(ctx)
	if err != nil {
		return out, err
	}
	for _, in := range installs {
		if in.Status != store.PluginInstallEnabled {
			continue
		}
		ver, err := m.o.Store.GetPluginVersion(ctx, in.PluginID, in.Version)
		if err != nil {
			continue
		}
		meta, err := Parse(ver.Manifest, true)
		if err != nil {
			m.o.Logger.Warn("统计凭证引用时跳过无法解析的安装清单",
				"space", in.SpaceID, "slug", in.Slug, "err", err)
			continue
		}
		collect(meta)
	}

	cands, err := m.o.Store.ListPluginCandidates(ctx)
	if err != nil {
		// 草稿读不到不影响已发布件的统计
		return out, nil
	}
	for _, c := range cands {
		if c.Status != store.PluginCandidateEnabled {
			continue
		}
		meta, err := m.readDraft(ctx, c.SpaceID, c.Slug)
		if err != nil {
			continue
		}
		collect(meta)
	}
	return out, nil
}
