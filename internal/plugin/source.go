package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
)

// sourceRuntime 是 kind=source 的执行体：一个搜索源。
//
// 它不面向模型，只面向 search.Registry：构造出来的 Provider 被注册进去之后，
// web_search 就能命中它。spec 与 script 两种协议在这里收敛成同一个形状。
type sourceRuntime struct {
	def search.SourceDef
}

// buildSource 构造搜索源适配器。产物缺失或配置非法时返回错误，
// 由调用方决定是「跳过加载」还是「试跑失败」。
func (m *Manager) buildSource(ctx context.Context, spaceID, slug, dir string, meta Manifest) (*sourceRuntime, error) {
	if meta.Source == nil {
		return nil, fmt.Errorf("source 插件缺少 source 段")
	}
	s := meta.Source
	guard := search.NewSourceGuard(meta.Hosts(), s.AllowPrivate)
	key := m.sourceKeyFunc(s.Secret)

	provider, err := m.newSourceProvider(ctx, spaceID, slug, dir, s, key, guard)
	if err != nil {
		return nil, err
	}
	return &sourceRuntime{def: search.SourceDef{
		Name:         slug,
		Scope:        s.Scope,
		Endpoint:     s.Endpoint,
		Secret:       s.Secret,
		AllowPrivate: s.AllowPrivate,
		Priority:     s.Priority,
		Timeout:      time.Duration(s.TimeoutSeconds) * time.Second,
		Provider:     provider,
		Origin:       "plugin",
	}}, nil
}

// newSourceProvider 按协议构造适配器：spec 走声明式解释器，script 走两段式脚本。
func (m *Manager) newSourceProvider(ctx context.Context, spaceID, slug, dir string, s *SourceSpec, key search.KeyFunc, guard *search.Guard) (search.Provider, error) {
	switch s.Protocol {
	case SourceProtocolSpec:
		raw, err := ReadFile(ctx, m.o.Storage, dir, s.Entry)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", s.Entry, err)
		}
		var spec search.Spec
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("%s 解析失败（不允许未知字段）: %w", s.Entry, err)
		}
		return search.NewHTTPJSON(slug, s.Endpoint, spec, key, guard)
	case SourceProtocolScript:
		// 脚本必须落到本地文件才能交给 deno；对象存储后端不支持脚本源。
		lp, ok := m.o.Storage.(localPathStore)
		if !ok {
			return nil, fmt.Errorf("脚本源需要本地存储后端（当前存储不提供本地路径）")
		}
		root, err := lp.LocalPath(dir)
		if err != nil {
			return nil, fmt.Errorf("解析插件目录失败: %w", err)
		}
		// 脚本源只需要读自己的目录：不把整个空间工作区挂进去，
		// 它本来也没有 fs.* 能力，多一份可读范围只是多一份风险。
		d, err := search.NewDeno(slug, filepath.Join(root, s.Entry), s.Endpoint, key, guard, m.o.Runner)
		if err != nil {
			return nil, err
		}
		// 脚本目录挂到沙箱内的固定路径：argv 与编译错误里出现的都是 /plugin/…，
		// 不是控制面的存储路径。
		d = d.WithMounts(
			[]sandbox.Mount{{Source: root, Key: dir, Target: sandbox.PluginDir}},
			path.Join(sandbox.PluginDir, filepath.ToSlash(s.Entry)),
		)
		return d, nil
	default:
		return nil, fmt.Errorf("未知的 source.protocol %q", s.Protocol)
	}
}

// sourceKeyFunc 按需取凭证：每次搜索都读一次，轮换后立即生效。
func (m *Manager) sourceKeyFunc(name string) search.KeyFunc {
	return func(ctx context.Context) (string, error) {
		if strings.TrimSpace(name) == "" {
			return "", nil
		}
		if m.o.Secrets == nil {
			return "", fmt.Errorf("凭证存储未启用，无法读取 %q", name)
		}
		return m.o.Secrets.Get(ctx, search.SecretScope, name)
	}
}

// registerSource 把一个已加载的源注册进注册表。
//
// 系统空间的安装 / 来自系统空间的全局安装都注册为全局源（SpaceID 为空），
// 其余绑到本空间：普通空间装的私有源不该被别的空间搜到。
func (m *Manager) registerSource(spaceID, sysID string, inst *instance) {
	if m.o.Sources == nil || inst == nil || inst.Source == nil {
		return
	}
	def := inst.Source.def
	if inst.Global || (sysID != "" && spaceID == sysID) {
		def.SpaceID = ""
	} else {
		def.SpaceID = spaceID
	}
	if err := m.o.Sources.UpsertSource(def); err != nil {
		m.o.Logger.Warn("注册搜索源失败", "slug", inst.Slug, "space", spaceID, "err", err)
	}
}

// DryRunJSON 是给工具层（plugin_try）用的试跑入口：回执与错误一起返回，
// 因为「试跑失败」时诊断（顶层键、状态码）才是最有价值的部分。
func (m *Manager) DryRunJSON(ctx context.Context, spaceID, slug, userID string, input json.RawMessage) (string, error) {
	res, err := m.DryRun(ctx, spaceID, slug, userID, input)
	blob, mErr := json.MarshalIndent(res, "", "  ")
	out := ""
	if mErr == nil {
		out = string(blob)
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// reloadSpace 让本空间的运行时缓存失效并立即重建。
//
// 搜索源是「装完即生效」的：启用/停用后如果不能立刻反映到 web_search，
// 用户会觉得开关没用。重载只影响当前空间，不碰其它空间的实例。
func (m *Manager) reloadSpace(ctx context.Context, spaceID string) error {
	// 重载意味着磁盘上的内容可能变了：常驻进程必须一起换掉。
	m.closeSpaceProcs(spaceID)
	m.mu.Lock()
	delete(m.inst, spaceID)
	for key := range m.mcpCs {
		if strings.HasPrefix(key, spaceID+"/") {
			delete(m.mcpCs, key)
		}
	}
	// 借用池可能引用这个空间的插件，内容一变就得整体重建。
	m.borrow = map[string][]*instance{}
	m.mu.Unlock()
	return m.ensureLoaded(ctx, spaceID)
}

// Reload 供空间服务在插件目录被直接改动（删除草稿）后调用，
// 让运行时缓存与文件事实保持一致。
func (m *Manager) Reload(ctx context.Context, spaceID string) error {
	return m.reloadSpace(ctx, spaceID)
}

// unregisterSource 在停用/卸载时把源摘掉，避免「卸载了还能搜」。
//
// global 由调用方判定（它已经查过系统空间 ID）：普通空间装过一个与全局同名的
// 私有源时，卸载它不能顺手删掉全局那份。
func (m *Manager) unregisterSource(spaceID, slug string, global bool) {
	if m.o.Sources == nil {
		return
	}
	m.o.Sources.RemoveSource(spaceID, slug)
	if global {
		m.o.Sources.RemoveSource("", slug)
	}
}

// sourceProbe 是试跑诊断：只描述「请求长什么样、响应里有什么」。
type sourceProbe struct {
	Request  string          `json:"request,omitempty"`
	Status   int             `json:"status,omitempty"`
	TopKeys  []string        `json:"top_keys,omitempty"`
	ItemsKey string          `json:"items_path,omitempty"`
	Hit      int             `json:"hit"`
	Dropped  int             `json:"dropped"`
	Samples  []search.Result `json:"samples,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}
