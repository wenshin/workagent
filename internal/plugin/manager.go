package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/securityscan"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// 错误分类：HTTP 层据此决定状态码。
var (
	ErrForbidden  = errors.New("没有权限执行该操作")
	ErrConflict   = errors.New("插件状态冲突")
	ErrReferenced = errors.New("插件正在被引用")
)

// Options 是装配参数。
type Options struct {
	Store   store.Store
	Storage storage.Storage
	Runner  sandbox.Runner
	Secrets Secrets
	Logger  *slog.Logger
	// Invoker 是统一的工具执行链：组件取数必须经过它，不能自己直调。
	Invoker *tool.Invoker
	// RuntimeDir 存放注入插件的 host_sdk.ts / bootstrap.ts（必须是本地目录）。
	RuntimeDir string
	// DefaultInstall 是部署者配置的默认安装集，空间首次初始化时执行一次。
	DefaultInstall []string
	// Sources 是搜索源注册表（internal/search）。kind=source 的插件在安装/启用时
	// 注册进去、停用/卸载时摘掉，因此「装完即生效」，不需要重启。
	Sources *search.Registry
	// OnChange 在安装/启停/卸载/本地启用状态变化后触发，宿主用来重建空间索引。
	OnChange func(ctx context.Context, spaceID string)
	// Scanner 是发布前的安全扫描器（docs/插件安全基线.md）。为 nil 时不扫描，
	// 也不落扫描记录——「没配」必须和「扫过且干净」区分开。
	Scanner *securityscan.Engine
	// ProcIdle 是插件常驻进程的空闲回收时间，<=0 用 DefaultProcIdle。
	ProcIdle time.Duration
	// ProcMax 是常驻进程上限，<=0 用 DefaultProcMax。
	ProcMax int
	// Evals 用于跑插件自带的 agent 类黄金用例；为 nil 时这类用例跳过并记入报告，
	// 不阻塞发布——发布门禁不能依赖外部模型可用。
	Evals EvalRunner
}

// Manager 管理四类插件的完整生命周期。
type Manager struct {
	o       Options
	deno    string
	rt      *toolRuntime
	invoker *tool.Invoker

	mu    sync.RWMutex
	inst  map[string]map[string]*instance // spaceID → slug → 已启用实例
	mcpCs map[string]*mcpRuntime
	// borrow 缓存跨空间借用的实例：key = 会话空间 | 来源引用集合。
	// 与 inst 分开存：借用只加载指定插件，不能污染来源空间的完整快照。
	borrow map[string][]*instance
	boot   map[string]bool
}

type instance struct {
	SpaceID  string
	PluginID string
	// Local 表示空间本地开发插件，不对应全局发布件。
	Local       bool
	Slug        string
	Version     string
	Manifest    Manifest
	Dir         string
	Risk        int
	SideEffects string
	Tools       []tool.Tool
	MCP         *mcpRuntime
	// Source 是 kind=source 的搜索源执行体；其它类型为 nil。
	Source *sourceRuntime
	// Global 表示来自系统空间的「全局安装」，对本空间只读可用。
	Global bool
}

// ManifestOptions 返回已安装清单（HTTP 层透出用）。
type Item struct {
	Slug            string   `json:"slug"`
	Kind            string   `json:"kind"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Version         string   `json:"version"`
	Status          string   `json:"status"` // draft | enabled | installed | disabled
	Origin          string   `json:"origin"`
	Risk            int      `json:"risk"`
	Installed       bool     `json:"installed"`
	Local           bool     `json:"local"`
	Enabled         bool     `json:"enabled"`
	Mutable         bool     `json:"mutable"`
	Published       bool     `json:"published"`
	HasDraft        bool     `json:"has_draft"`
	Extensible      bool     `json:"extensible"`
	UpdateAvailable bool     `json:"update_available"`
	LatestVersion   string   `json:"latest_version"`
	BaselineVersion string   `json:"baseline_version,omitempty"`
	LocalModified   bool     `json:"local_modified"`
	Tools           []string `json:"tools,omitempty"`
	// Permissions 是清单声明的权限（人类可读），安装确认时展示给用户看。
	// 文案由 Manifest.Permissions 统一生成，前端不自己拼。
	Permissions []string `json:"permissions,omitempty"`
	// LatestPermissions 是待升级版本的权限：升级同样要让人看见权限变化，
	// 否则「加一个 fs.write 再发版」就是静默提权。
	LatestPermissions []string `json:"latest_permissions,omitempty"`
	// Secrets 是清单声明的凭证与当前配置状态：缺凭证的插件装上也用不了，
	// 卡片据此显示原因并给录入入口，而不是等调用时才发现。
	Secrets []SecretState `json:"secrets,omitempty"`
	// Blocked 表示声明的东西没齐（目前只有缺凭证），前端按「未启用」呈现。
	Blocked bool `json:"blocked"`
	// BlockedReason 是未启用原因（人类可读），空表示没有阻塞。
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// DryRunResult 是试运行的回执：给作者看「它到底干了什么」。
type DryRunResult struct {
	Kind     Kind   `json:"kind"`
	Output   string `json:"output,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Duration string `json:"duration"`
	Risk     int    `json:"risk"`
	// Source 是搜索源试跑的诊断：请求行、状态码、响应顶层键、命中与样例。
	Source *search.ProbeInfo `json:"source,omitempty"`
}

// New 构造管理器。deno 缺失不阻断启动：脚本插件会以明确错误失败，
// 技能与组件不受影响。
func New(o Options) (*Manager, error) {
	if o.Store == nil || o.Storage == nil {
		return nil, errors.New("插件管理器缺少存储依赖")
	}
	if o.Runner == nil {
		return nil, errors.New("插件管理器缺少沙箱")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	deno, _ := exec.LookPath("deno")
	if deno == "" {
		o.Logger.Warn("找不到 deno，脚本插件（kind=tool）不可用")
	}
	if o.RuntimeDir != "" {
		if err := installSDK(o.RuntimeDir); err != nil {
			o.Logger.Warn("写入插件运行时 SDK 失败，脚本插件可能不可用", "err", err)
		}
	}
	m := &Manager{
		o:       o,
		deno:    deno,
		invoker: o.Invoker,
		inst:    map[string]map[string]*instance{},
		mcpCs:   map[string]*mcpRuntime{},
		borrow:  map[string][]*instance{},
		boot:    map[string]bool{},
	}
	m.rt = &toolRuntime{
		runner: o.Runner, deno: deno, runtimeDir: o.RuntimeDir,
		secrets: o.Secrets, fs: o.Storage, logger: o.Logger,
		egress: newEgressPool(o.Logger),
	}
	// 每个插件一个常驻进程：安装/停用/卸载/重载都会把对应进程关掉，
	// 保证「跑着的代码 = 现在磁盘上的代码」。
	m.rt.pool = newResidentPool(o.ProcIdle, o.ProcMax, o.Logger)
	return m, nil
}

// Close 关闭所有插件常驻进程。服务退出时调用；不调用也不会泄漏文件，
// 但会留下一堆孤儿 deno 进程，直到父进程回收。
func (m *Manager) Close() {
	if m.rt != nil && m.rt.pool != nil {
		m.rt.pool.CloseAll()
	}
	if m.rt != nil && m.rt.egress != nil {
		m.rt.egress.Close()
	}
}

// closeSlugProcs 关闭某个插件的常驻进程。
//
// 安装、升级、停用、卸载、草稿变更都要调用：常驻进程里跑的是**启动那一刻**的代码，
// 不关掉就会出现「文件改了、发布的也是新版本，但实际执行还是旧的」。
func (m *Manager) closeSlugProcs(spaceID, slug string) {
	if m.rt != nil && m.rt.pool != nil {
		m.rt.pool.closePrefix(spaceID + "|" + slug + "|")
	}
}

// closeSpaceProcs 关闭整个空间的常驻进程（空间重载 / 卸载）。
func (m *Manager) closeSpaceProcs(spaceID string) {
	if m.rt != nil && m.rt.pool != nil {
		m.rt.pool.closePrefix(spaceID + "|")
	}
}

// ---------------------------------------------------------------- 读取与索引

// List 返回空间内可见的插件：已安装的 + 空间里的草稿。
func (m *Manager) List(ctx context.Context, spaceID string) ([]Item, error) {
	if err := m.EnsureBootstrap(ctx, spaceID); err != nil {
		m.o.Logger.Warn("初始化默认安装集失败", "space", spaceID, "err", err)
	}
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, err
	}

	items := map[string]*Item{}
	// metas 收集每个 slug「当前生效的那份清单」：草稿用草稿，已安装用安装版本快照。
	// 凭证声明与权限都从这一份读，避免出现「面板说用 A 版本、实际跑 B 版本」。
	metas := map[string]Manifest{}
	cands, err := m.o.Store.ListPluginCandidatesBySpace(ctx, spaceID)
	if err != nil {
		m.o.Logger.Warn("读取本地插件状态失败", "space", spaceID, "err", err)
	}
	bySlug := make(map[string]store.PluginCandidate, len(cands))
	for _, c := range cands {
		bySlug[c.Slug] = c
	}
	drafts, err := m.listDrafts(ctx, spaceID)
	if err != nil {
		m.o.Logger.Warn("扫描插件草稿失败", "space", spaceID, "err", err)
	}
	for _, d := range drafts {
		cand := bySlug[d.Slug]
		enabled := cand.Status == store.PluginCandidateEnabled
		status := "draft"
		if enabled {
			status = "enabled"
		}
		item := &Item{
			Slug: d.Slug, Kind: string(d.Kind), Name: d.Name, Description: d.Description,
			Status: status, Origin: store.PluginOriginInternal, Local: true,
			Enabled: enabled, Mutable: true, HasDraft: true, Extensible: d.IsExtensible(),
			Risk: riskOf(d), Tools: d.ExposedTools(), Permissions: d.Permissions(),
		}
		if d.DerivedFrom != nil {
			item.BaselineVersion = d.DerivedFrom.Version
		}
		// 草稿可能已经发布过：要有 latest_version 才能从面板里装回来，
		// 同时用内容 hash 判断「有新版本待发布」。
		if p, err := m.o.Store.GetPluginBySlug(ctx, d.Slug); err == nil {
			item.LatestVersion = p.LatestVersion
			item.Published = true
			if v, verr := m.o.Store.GetPluginVersion(ctx, p.ID, p.LatestVersion); verr == nil {
				item.UpdateAvailable = v.SHA256 != cand.ManifestHash
			}
		}
		items[d.Slug] = item
		metas[d.Slug] = d
	}

	installs, err := m.o.Store.ListPluginInstalls(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, in := range installs {
		item, ok := items[in.Slug]
		if !ok {
			item = &Item{Slug: in.Slug}
			items[in.Slug] = item
		}
		item.Installed = true
		item.Version = in.Version
		item.Risk = in.Risk
		item.Local = false
		item.Enabled = in.Status == store.PluginInstallEnabled
		item.Mutable = false
		item.Status = "installed"
		if !item.Enabled {
			item.Status = "disabled"
		}
		// 权限与凭证都从已安装的版本快照读：用户要看到的是「这个版本实际会拿到什么」。
		if ver, verr := m.o.Store.GetPluginVersion(ctx, in.PluginID, in.Version); verr == nil {
			if mm, perr := Parse(ver.Manifest, true); perr == nil {
				metas[in.Slug] = mm
				item.Permissions = mm.Permissions()
			}
		}
		if p, err := m.o.Store.GetPluginBySlug(ctx, in.Slug); err == nil {
			if item.Name == "" {
				item.Name, item.Kind, item.Description = p.Name, p.Kind, p.Description
				item.Extensible = p.Extensible
			}
			item.Origin = p.Origin
			item.LatestVersion = p.LatestVersion
			// 升级目标版本的权限也要给出来：版本之间权限可以变。
			if p.LatestVersion != "" && p.LatestVersion != in.Version {
				if lv, verr := m.o.Store.GetPluginVersion(ctx, p.ID, p.LatestVersion); verr == nil {
					if mm, perr := Parse(lv.Manifest, true); perr == nil {
						item.LatestPermissions = mm.Permissions()
					}
				}
			}
			// 安装副本只读，不再有「本地改动/基线合并」的概念，只提示上游版本。
			item.UpdateAvailable = p.LatestVersion != "" && p.LatestVersion != in.Version
		}
	}

	out := make([]Item, 0, len(items))
	for _, item := range items {
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Slug < out[j].Slug
	})

	// 缺凭证 = 装上也用不了：卡片要显示「未启用 + 原因」，并给出录入入口。
	// 一次列表请求里每个 scope 只读一次凭证存储（cache 复用）。
	cache := map[string]map[string]bool{}
	secretsOff := m.secretsUnavailable()
	for i := range out {
		meta, ok := metas[out[i].Slug]
		if !ok {
			continue
		}
		states, missing := m.secretStates(ctx, meta, cache)
		out[i].Secrets = states
		if len(missing) == 0 {
			continue
		}
		out[i].Blocked = true
		out[i].BlockedReason = "缺少凭证 " + strings.Join(missing, "、") + "，配置后才会生效"
		if secretsOff {
			out[i].BlockedReason += "（凭证存储未启用：后端没读到 SECRET_KEK）"
		}
	}
	return out, nil
}

// secretsUnavailable 判断凭证存储是不是根本不可用：不可用时声明的凭证全都算缺，
// 但原因要写清是「没配 KEK」而不是「管理员忘了录」。
func (m *Manager) secretsUnavailable() bool {
	if m.o.Secrets == nil {
		return true
	}
	if e, ok := m.o.Secrets.(interface{ Enabled() bool }); ok {
		return !e.Enabled()
	}
	return false
}

// Detail 返回单个插件的清单；优先取草稿（作者正在改的那份）。
func (m *Manager) Detail(ctx context.Context, spaceID, slug string) (Item, Manifest, error) {
	list, err := m.List(ctx, spaceID)
	if err != nil {
		return Item{}, Manifest{}, err
	}
	for _, item := range list {
		if item.Slug != slug {
			continue
		}
		if item.HasDraft {
			d, err := m.readDraft(ctx, spaceID, slug)
			if err != nil {
				return Item{}, Manifest{}, err
			}
			return item, d, nil
		}
		in, err := m.installOf(ctx, spaceID, slug)
		if err != nil {
			return Item{}, Manifest{}, err
		}
		ver, err := m.o.Store.GetPluginVersion(ctx, in.PluginID, in.Version)
		if err != nil {
			return Item{}, Manifest{}, err
		}
		mm, err := Parse(ver.Manifest, true)
		if err != nil {
			return Item{}, Manifest{}, err
		}
		return item, mm, nil
	}
	return Item{}, Manifest{}, store.ErrNotFound
}

func (m *Manager) listDrafts(ctx context.Context, spaceID string) ([]Manifest, error) {
	root := path.Join(m.o.Storage.SpaceKey(spaceID), "files", DraftDir)
	entries, err := m.o.Storage.List(ctx, root)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Manifest, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		slug := path.Base(e.Key)
		// 安装副本也在 plugins/<slug>，但不是草稿：不能出现在开发/发布入口。
		if _, err := m.installOf(ctx, spaceID, slug); err == nil {
			continue
		}
		d, err := m.readDraft(ctx, spaceID, slug)
		if err != nil {
			m.o.Logger.Warn("跳过无法解析的插件草稿", "space", spaceID, "slug", slug, "err", err)
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (m *Manager) readDraft(ctx context.Context, spaceID, slug string) (Manifest, error) {
	b, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug), false)
	if err != nil {
		return Manifest{}, err
	}
	return b.Meta, nil
}

// ---------------------------------------------------------------- 开发：试运行

// DryRun 用样例输入跑一次，返回输出与风险，不产生副作用。
func (m *Manager) DryRun(ctx context.Context, spaceID, slug, userID string, input json.RawMessage) (DryRunResult, error) {
	b, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug), false)
	if err != nil {
		return DryRunResult{}, err
	}
	risk, _ := Derive(b.Meta)
	start := time.Now()
	res := DryRunResult{Kind: b.Meta.Kind, Risk: risk}

	switch b.Meta.Kind {
	case KindTool:
		allow, verbs, secrets := b.Meta.HostAllow()
		fs, spaceDir, err := m.sandboxFS(spaceID, b.Dir, b.Meta)
		if err != nil {
			return res, err
		}
		out, err := m.rt.Call(ctx, invokeInput{
			Dir: b.Dir, Entry: b.Meta.Runtime.Entry,
			Limits: b.Meta.Limits, SpaceID: spaceID, UserID: userID, Slug: slug,
			FS: fs, SpaceRoot: spaceDir, NetHosts: b.Meta.NetHosts(),
			WSHosts: b.Meta.WSNetHosts(), OneShot: true,
			Allow: allow, Verbs: verbs, Secrets: secrets,
		}, input)
		if err != nil {
			return res, err
		}
		res.Output = out
	case KindMCP:
		rt, err := newMCPRuntime(ctx, b.Meta, m.o.Secrets, m.o.Store, m.o.Logger, m.mcpCachePath(b.Meta.Slug))
		if err != nil {
			return res, err
		}
		res.Detail = fmt.Sprintf("连接成功，可用工具 %d 个：%s", len(rt.tools), strings.Join(rt.names(), ", "))
		if b.Meta.Auth.JWT() {
			res.Detail += fmt.Sprintf("；鉴权方式 jwt（应用 %s，调用时按用户现签）", b.Meta.Auth.AppName(b.Meta.Slug))
		}
	case KindSkill:
		body, err := ReadFile(ctx, m.o.Storage, b.Dir, b.Meta.Skill.Entry)
		if err != nil {
			return res, err
		}
		res.Detail = fmt.Sprintf("正文 %d 字节，首行：%s", len(body), firstLine(body))
	case KindAgent:
		entry := AgentEntry
		if b.Meta.Agent != nil && b.Meta.Agent.Entry != "" {
			entry = b.Meta.Agent.Entry
		}
		body, err := ReadFile(ctx, m.o.Storage, b.Dir, entry)
		if err != nil {
			return res, err
		}
		detail := fmt.Sprintf("提示词 %d 字节，首行：%s", len(body), firstLine(body))
		if b.Meta.Agent != nil && b.Meta.Agent.Extends != "" {
			detail += fmt.Sprintf("；继承 %s", b.Meta.Agent.Extends)
		}
		if b.Meta.Agent != nil && len(b.Meta.Agent.Requires) > 0 {
			detail += fmt.Sprintf("；依赖 %d 个插件", len(b.Meta.Agent.Requires))
		}
		res.Detail = detail
	case KindComponent:
		body, ok := b.Files[b.Meta.UI.Entry]
		if !ok {
			return res, fmt.Errorf("找不到组件 DSL %s", b.Meta.UI.Entry)
		}
		height, err := ValidateDSL(body, b.Meta.DataSources)
		if err != nil {
			return res, err
		}
		res.Detail = fmt.Sprintf("DSL 校验通过（%d 字节，渲染高度 %dpx）", len(body), height)
	case KindSource:
		var in struct {
			Query string `json:"query"`
		}
		if len(input) > 0 {
			if err := json.Unmarshal(input, &in); err != nil {
				return res, fmt.Errorf("试跑参数必须是 {\"query\": \"...\"}: %w", err)
			}
		}
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return res, fmt.Errorf("搜索源试跑需要提供 query（拿一个真实检索词跑一次）")
		}
		src, err := m.buildSource(ctx, spaceID, slug, b.Dir, b.Meta)
		if err != nil {
			return res, err
		}
		probe, probeErr := search.ProbeProvider(ctx, src.def.Provider, query, 5)
		res.Source = &probe
		res.Detail = sourceDetail(b.Meta, &probe)
		if probeErr != nil {
			return res, probeErr
		}
	}
	res.Duration = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

// sourceDetail 把诊断压成一行给前端表格与模型看。
func sourceDetail(meta Manifest, p *search.ProbeInfo) string {
	parts := []string{}
	if meta.Source != nil {
		parts = append(parts, fmt.Sprintf("%s/%s", meta.Source.Protocol, meta.Source.Scope))
	}
	if p.Status > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", p.Status))
	}
	if len(p.TopKeys) > 0 {
		parts = append(parts, "顶层键 "+strings.Join(p.TopKeys, ", "))
	}
	parts = append(parts, fmt.Sprintf("命中 %d 条", p.Hit))
	if p.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("丢弃 %d 条（无 URL）", p.Dropped))
	}
	return strings.Join(parts, "；")
}

// ---------------------------------------------------------------- 发布

// validateBundleDSL 校验组件包里的 DSL；发布与试运行共用同一条规则。
func validateBundleDSL(b *Bundle) error {
	body, ok := b.Files[b.Meta.UI.Entry]
	if !ok {
		return fmt.Errorf("找不到组件 DSL %s（ui.entry 必须指向目录里真实存在的文件）", b.Meta.UI.Entry)
	}
	if _, err := ValidateDSL(body, b.Meta.DataSources); err != nil {
		return err
	}
	return nil
}

// Publish 把草稿打成不可变版本。version 为空时自动递增 patch。
//
// 发布权只在系统管理员：空间管理员只能启用本地插件，让其成为市场的
// 「待发布」候选。发布前要求候选仍启用、内容 hash 与启用时一致。
//
// jwt 模式的 MCP 插件还要确认应用名归属（见 confirmAppKey）：应用名不是
// 发布者创建时，appKey 必须对得上已存的签名密钥，否则拒绝——发布响应会
// 回显密钥，不确认等于把别人的生产密钥直接交出去。
func (m *Manager) Publish(ctx context.Context, spaceID, slug, userID, version, notes, appKey string, isAdmin bool) (store.PluginVersion, error) {
	if !isAdmin {
		return store.PluginVersion{}, fmt.Errorf("%w：只有系统管理员可以发布插件", ErrForbidden)
	}
	cand, err := m.o.Store.GetPluginCandidate(ctx, spaceID, slug)
	if err != nil {
		return store.PluginVersion{}, fmt.Errorf("%w：本地插件未启用，不能发布", ErrConflict)
	}
	if cand.Status != store.PluginCandidateEnabled {
		return store.PluginVersion{}, fmt.Errorf("%w：本地插件已停用，不能发布", ErrConflict)
	}
	b, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug), false)
	if err != nil {
		return store.PluginVersion{}, err
	}
	// DSL 在发布时就要校验：坏组件不能进发布区，否则「渲染不出来」会被当成
	// 运行期故障排查，而真正的问题是它根本不该被发布。
	if b.Meta.Kind == KindComponent {
		if err := validateBundleDSL(b); err != nil {
			return store.PluginVersion{}, err
		}
	}
	// 智能体没有正文就是空壳：宁可发布失败，也不要装出一个「什么都不追加」的智能体。
	if b.Meta.Kind == KindAgent {
		entry := AgentEntry
		if b.Meta.Agent != nil && b.Meta.Agent.Entry != "" {
			entry = b.Meta.Agent.Entry
		}
		if body, err := ReadFile(ctx, m.o.Storage, b.Dir, entry); err != nil {
			return store.PluginVersion{}, fmt.Errorf("智能体缺少提示词正文 %s: %w", entry, err)
		} else if strings.TrimSpace(body) == "" {
			return store.PluginVersion{}, fmt.Errorf("%w：智能体提示词正文不能为空", ErrConflict)
		}
	}
	risk, _ := Derive(b.Meta)
	if risk >= policy.RiskSensitive && !isAdmin {
		return store.PluginVersion{}, fmt.Errorf("%w：该插件同时使用密钥与外部写，需要系统管理员审批", ErrForbidden)
	}
	existing, gerr := m.o.Store.GetPluginBySlug(ctx, slug)
	if gerr == nil {
		if existing.OwnerSpaceID != "" && existing.OwnerSpaceID != spaceID {
			return store.PluginVersion{}, fmt.Errorf("%w：slug %q 已被其他空间占用", ErrConflict, slug)
		}
		if existing.Kind != string(b.Meta.Kind) {
			return store.PluginVersion{}, fmt.Errorf("%w：slug 已作为 %s 发布，不能改成 %s", ErrConflict, existing.Kind, b.Meta.Kind)
		}
	} else if !errors.Is(gerr, store.ErrNotFound) {
		return store.PluginVersion{}, gerr
	}

	if version == "" {
		latest := ""
		if gerr == nil {
			latest = existing.LatestVersion
		}
		version = nextVersion(latest)
	}
	b.Meta.Version = version

	// jwt 模式的 MCP 插件没有密钥就等于装不上：在写任何数据之前就生成，
	// 失败则拒绝发布，避免出现「发布成功但调用必然 401」的版本。
	if b.Meta.Kind == KindMCP && b.Meta.Auth.JWT() {
		app := b.Meta.Auth.AppName(b.Meta.Slug)
		// 先确认归属：别人的应用名必须出示密钥，否则发布响应等于把他们的
		// 生产密钥交出去，前面按创建者收口的规则也就白做了。
		if err := m.confirmAppKey(ctx, userID, spaceID, app, appKey); err != nil {
			return store.PluginVersion{}, err
		}
		if _, err := ensureAppKey(ctx, m.o.Secrets, app); err != nil {
			return store.PluginVersion{}, err
		}
		// 顺手记下归属：应用名是全局唯一的，发布是「认领」它的自然时点。
		// 已经归属别人的不会被改写（ClaimMCPApp 不覆盖），这里只补历史的空记录。
		if _, _, err := m.o.Store.ClaimMCPApp(ctx, app, userID, spaceID); err != nil {
			m.o.Logger.Warn("记录 MCP 应用归属失败", "app", app, "err", err)
		}
	}

	// 安全扫描在写任何数据之前：被阻断时不产生任何副作用，
	// 草稿、候选状态、插件记录都保持原样。
	scan, blocked := m.scanDraft(ctx, b, version)
	if blocked {
		m.recordAudit(ctx, userID, "plugin.publish.blocked", slug, version, map[string]any{
			"rules":        scan.RulesVersion,
			"status":       scan.Status,
			"max_severity": string(scan.MaxSeverity),
			"space":        spaceID,
		})
		return store.PluginVersion{}, &ScanBlockedError{Report: scan}
	}

	// 黄金用例在落库之前跑：作者声明了「这个插件该怎么用」，
	// 发布就是把这份声明变成对所有人的承诺，没跑通就不该发。
	evals, evalErr := m.evalDraft(ctx, b, spaceID, slug, userID)
	if evalErr != nil {
		m.recordAudit(ctx, userID, "plugin.publish.eval_failed", slug, version, map[string]any{
			"space":    spaceID,
			"evals":    evals.Summary(),
			"failures": strings.Join(evals.Failures, "；"),
		})
		return store.PluginVersion{}, evalErr
	}
	if evals.Present {
		m.recordAudit(ctx, userID, "plugin.publish.eval_passed", slug, version, map[string]any{
			"space": spaceID,
			"evals": evals.Summary(),
		})
	}

	manifestRaw, err := json.Marshal(b.Meta)
	if err != nil {
		return store.PluginVersion{}, err
	}

	p, err := m.o.Store.UpsertPlugin(ctx, store.Plugin{
		Slug: slug, Kind: string(b.Meta.Kind), Name: b.Meta.Name,
		Description: b.Meta.Description, Visibility: store.PluginVisibilityPublic,
		Status: store.PluginStatusEnabled, OwnerSpaceID: spaceID,
		Origin:     store.PluginOriginInternal,
		Extensible: b.Meta.IsExtensible(), LatestVersion: version,
	})
	if err != nil {
		return store.PluginVersion{}, err
	}

	target := PublishedKey(p.ID, version)
	if err := b.WriteTo(ctx, m.o.Storage, target); err != nil {
		return store.PluginVersion{}, err
	}
	caps, _ := json.Marshal(b.Meta.Capabilities)
	v, err := m.o.Store.AddPluginVersion(ctx, store.PluginVersion{
		PluginID: p.ID, Version: version, Manifest: manifestRaw,
		ArtifactPath: target, SHA256: b.Hash, Capabilities: caps,
		Risk: risk, Notes: notes, PublishedBy: userID,
	}, version)
	if err != nil {
		return store.PluginVersion{}, err
	}
	// 扫描结论是元数据：落库失败只告警，不回滚已经完成的发布。
	m.saveScan(ctx, p.ID, b.Hash, scan, userID)

	cand.ManifestHash = b.Hash
	cand.Status = store.PluginCandidateEnabled
	if _, err := m.o.Store.UpsertPluginCandidate(ctx, cand); err != nil {
		m.o.Logger.Warn("刷新候选内容 hash 失败", "slug", slug, "err", err)
	}
	if err := m.o.Store.MarkPluginCandidatePublished(ctx, spaceID, slug, p.ID); err != nil {
		m.o.Logger.Warn("更新候选发布状态失败", "slug", slug, "err", err)
	}
	m.recordAudit(ctx, userID, "plugin.publish", slug, version, map[string]any{"risk": risk, "sha256": b.Hash, "space": spaceID})
	return v, nil
}

// ---------------------------------------------------------------- 安装 / 卸载

// InstallInput 是安装参数。
type InstallInput struct {
	SpaceID string
	Slug    string
	Version string
	UserID  string
	Perm    string
	IsAdmin bool
}

// Install 安装一个已发布版本。安装即授权：写入安装记录、注册工具、建立绑定。
func (m *Manager) Install(ctx context.Context, in InstallInput) (store.PluginInstall, error) {
	return m.install(ctx, in, 0)
}

func (m *Manager) install(ctx context.Context, in InstallInput, depth int) (store.PluginInstall, error) {
	if !in.IsAdmin && in.Perm != "manage" {
		return store.PluginInstall{}, fmt.Errorf("%w：安装需要空间 manage 权限", ErrForbidden)
	}
	if depth > 4 {
		return store.PluginInstall{}, fmt.Errorf("%w：依赖嵌套过深", ErrConflict)
	}
	p, err := m.o.Store.GetPluginBySlug(ctx, in.Slug)
	if err != nil {
		return store.PluginInstall{}, err
	}
	if p.Status == store.PluginStatusDeprecated {
		return store.PluginInstall{}, fmt.Errorf("%w：插件已弃用", ErrConflict)
	}
	// 平台内置件只安装在系统空间：那份安装记录就是「全局可用」，
	// 普通空间再装一次没有意义，也没法删。
	sysID, _ := m.systemSpaceID(ctx)
	globalScope := sysID != "" && in.SpaceID == sysID
	if p.Origin == store.PluginOriginSystem && !globalScope {
		return store.PluginInstall{}, fmt.Errorf("%w：平台内置插件已全局可用，无需安装到空间", ErrConflict)
	}
	if in.Version == "" {
		in.Version = p.LatestVersion
	}
	v, err := m.o.Store.GetPluginVersion(ctx, p.ID, in.Version)
	if err != nil {
		return store.PluginInstall{}, err
	}
	meta, err := Parse(v.Manifest, true)
	if err != nil {
		return store.PluginInstall{}, err
	}
	// 智能体随包声明依赖：先把技能/工具/MCP 装进同一空间，
	// 否则「装上智能体」会得到一个没有能力可用的空壳。
	if meta.Kind == KindAgent && meta.Agent != nil {
		for _, dep := range meta.Agent.Requires {
			dp, err := m.o.Store.GetPluginBySlug(ctx, dep)
			if err != nil {
				return store.PluginInstall{}, fmt.Errorf("智能体依赖 %s 未发布: %w", dep, err)
			}
			if dp.Origin == store.PluginOriginSystem {
				continue // 内置依赖已全局可用
			}
			if _, err := m.install(ctx, InstallInput{
				SpaceID: in.SpaceID, Slug: dep, UserID: in.UserID,
				Perm: in.Perm, IsAdmin: in.IsAdmin,
			}, depth+1); err != nil {
				return store.PluginInstall{}, fmt.Errorf("安装智能体依赖 %s 失败: %w", dep, err)
			}
		}
	}
	risk, _ := Derive(meta)
	// L4 需要系统管理员：能力清单里同时出现「密钥」和「外部写」时，
	// 空间管理员不足以承担这个授权。
	if risk >= policy.RiskSensitive && !in.IsAdmin {
		return store.PluginInstall{}, fmt.Errorf("%w：该插件需要系统管理员审批", ErrForbidden)
	}

	// 升级失败时不能把原来能用的安装记录一起抹掉：先记住旧记录，失败就回滚。
	prev, prevErr := m.installOf(ctx, in.SpaceID, in.Slug)
	rollback := func() {
		if prevErr == nil {
			if _, err := m.o.Store.UpsertPluginInstall(ctx, prev); err != nil {
				m.o.Logger.Warn("回滚安装记录失败", "slug", in.Slug, "err", err)
			}
			return
		}
		_ = m.o.Store.DeletePluginInstall(ctx, in.SpaceID, p.ID)
	}

	install, err := m.o.Store.UpsertPluginInstall(ctx, store.PluginInstall{
		SpaceID: in.SpaceID, PluginID: p.ID, Slug: in.Slug, Version: v.Version,
		Status: store.PluginInstallEnabled, Risk: risk, InstalledBy: in.UserID,
	})
	if err != nil {
		return store.PluginInstall{}, err
	}
	install.Slug = in.Slug

	// 安装 = 在空间里生成派生副本（方案 A）：副本独立可改，不自动合并上游。
	// 先落副本，再建运行时，保证运行时读到的就是用户将来编辑的那份内容。
	// 不可扩展的内置件不落副本：它们只在插件市场可见，运行时直接读发布件，
	// 避免每个空间都存一份不可改的内容、也避免用户以为改了会生效。
	// 系统空间安装要落副本：`系统空间/plugins/<slug>/` 就是「全局安装目录」，
	// 管理员在文件树里能直接看到全局装了哪些插件（内容仍只读）。
	if meta.IsExtensible() || globalScope {
		if err := m.ensureCopy(ctx, in.SpaceID, p.ID, in.Slug, v.Version, v.ArtifactPath, meta); err != nil {
			rollback()
			return store.PluginInstall{}, err
		}
	}
	inst, err := m.buildInstance(ctx, in.SpaceID, p.ID, in.Slug, v.Version, meta, v.ArtifactPath)
	if err != nil {
		// 运行时起不来就不要留下「已安装」的假象
		rollback()
		return store.PluginInstall{}, err
	}
	if err := m.registerTools(ctx, inst); err != nil {
		rollback()
		return store.PluginInstall{}, err
	}

	// 升级会换掉实例：旧进程必须关掉，否则新版本装上了、跑的还是旧代码。
	m.closeSlugProcs(in.SpaceID, in.Slug)

	m.mu.Lock()
	if m.inst[in.SpaceID] == nil {
		m.inst[in.SpaceID] = map[string]*instance{}
	}
	m.inst[in.SpaceID][in.Slug] = inst
	if inst.MCP != nil {
		m.mcpCs[in.SpaceID+"/"+in.Slug] = inst.MCP
	}
	m.mu.Unlock()

	if inst.Source != nil {
		m.registerSource(in.SpaceID, sysID, inst)
	}
	m.recordAudit(ctx, in.UserID, "plugin.install", in.Slug, v.Version,
		map[string]any{"risk": risk, "space": in.SpaceID})
	return install, nil
}

// Uninstall 卸载：先查引用，再摘运行时、删内容，审计保留。
func (m *Manager) Uninstall(ctx context.Context, userID, spaceID, slug string) error {
	if p, err := m.o.Store.GetPluginBySlug(ctx, slug); err == nil && p.Origin == store.PluginOriginSystem {
		return fmt.Errorf("%w：平台内置插件由平台统一提供，不能卸载", ErrForbidden)
	}
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	refs, err := m.references(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return fmt.Errorf("%w：%s", ErrReferenced, strings.Join(refs, "；"))
	}
	if err := m.o.Store.DeletePluginInstall(ctx, spaceID, in.PluginID); err != nil {
		return err
	}
	m.closeSlugProcs(spaceID, slug)
	m.mu.Lock()
	delete(m.inst[spaceID], slug)
	delete(m.mcpCs, spaceID+"/"+slug)
	// 借用池可能持有这个插件的工具，状态一变就整体丢弃。
	m.borrow = map[string][]*instance{}
	m.mu.Unlock()
	// 系统空间的那份是全局源；普通空间卸载同名私有源时不能连带删掉全局
	global := false
	if sysID, err := m.systemSpaceID(ctx); err == nil && sysID != "" && sysID == spaceID {
		global = true
	}
	m.unregisterSource(spaceID, slug, global)

	if err := RemoveDir(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug)); err != nil {
		m.o.Logger.Warn("清理插件内容失败", "slug", slug, "err", err)
	}
	if err := m.o.Store.DeletePluginToolRecords(ctx, slug); err != nil {
		m.o.Logger.Warn("清理插件工具记录失败", "slug", slug, "err", err)
	}
	m.recordAudit(ctx, userID, "plugin.uninstall", slug, in.Version, map[string]any{"space": spaceID})
	m.notify(ctx, spaceID)
	return nil
}

// SetEnabled 是 kill switch：立即拒绝新调用，不改内容、不回滚版本。
func (m *Manager) SetEnabled(ctx context.Context, userID, spaceID, slug string, enabled bool) error {
	if p, err := m.o.Store.GetPluginBySlug(ctx, slug); err == nil && p.Origin == store.PluginOriginSystem {
		// 内置件只有一份安装（系统空间），停用就是全局停用：开关放在系统空间的
		// 插件页，由系统管理员操作，不用重启。普通空间里的内置件是「并入」的，
		// 不是自己的安装记录，改不了也不该改。
		sysID, err := m.systemSpaceID(ctx)
		if err != nil || sysID == "" || sysID != spaceID {
			return fmt.Errorf("%w：平台内置插件只能在系统空间的插件页停用", ErrForbidden)
		}
		// base 是新建会话的兜底智能体：停掉它等于让之后的会话没有行为设定，
		// 这不是「暂时关掉一个能力」，是把自己锁在门外，所以单独拦住。
		if p.Kind == string(KindAgent) && slug == agentspec.BaseID {
			return fmt.Errorf("%w：平台默认智能体 %s 不能停用（新建会话以它为兜底）", ErrForbidden, agentspec.BaseID)
		}
	}
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	status := store.PluginInstallDisabled
	if enabled {
		status = store.PluginInstallEnabled
	}
	in.Status = status
	if _, err := m.o.Store.UpsertPluginInstall(ctx, in); err != nil {
		return err
	}
	// 停用是 kill switch：进程必须立刻杀掉，不能等空闲回收。
	m.closeSlugProcs(spaceID, slug)
	m.mu.Lock()
	delete(m.inst[spaceID], slug)
	delete(m.mcpCs, spaceID+"/"+slug)
	// 借用池可能持有这个插件的工具，状态一变就整体丢弃。
	m.borrow = map[string][]*instance{}
	m.mu.Unlock()
	// 系统空间的开关是全局开关：内置件只有一份安装，停用/启用对每个空间都成立。
	global := false
	if sysID, err := m.systemSpaceID(ctx); err == nil && sysID != "" && sysID == spaceID {
		global = true
	}
	if enabled {
		// 启用即生效：重载本空间，让搜索源等运行时重新注册
		if err := m.reloadSpace(ctx, spaceID); err != nil {
			m.o.Logger.Warn("重载空间失败", "space", spaceID, "err", err)
		}
	} else {
		// 停用是 kill switch：先把源摘掉，不等下一次重载
		m.unregisterSource(spaceID, slug, global)
	}
	if global {
		// 其它空间手里还攥着开关之前的实例：作废，下次访问自己重建。
		m.evictSpaces(spaceID)
	}
	m.recordAudit(ctx, userID, "plugin.enable", slug, strconv.FormatBool(enabled), map[string]any{"space": spaceID})
	m.notify(ctx, spaceID)
	return nil
}

// DisabledSlugs 返回对该空间「已被停用」的插件 slug：本空间停用的安装
// + 系统空间停用的全局安装（后者对所有空间生效）。
//
// 空间索引是按文件现状建的，停用之后 plugins/<slug>/ 副本仍然在树里；
// 不在读取时筛一遍，停用对技能/组件这类「靠索引进检索池」的插件就等于没生效。
func (m *Manager) DisabledSlugs(ctx context.Context, spaceID string) (map[string]bool, error) {
	out := map[string]bool{}
	collect := func(id string) error {
		if id == "" {
			return nil
		}
		installs, err := m.o.Store.ListPluginInstalls(ctx, id)
		if err != nil {
			return err
		}
		for _, in := range installs {
			if in.Status == store.PluginInstallDisabled {
				out[in.Slug] = true
			}
		}
		return nil
	}
	if err := collect(spaceID); err != nil {
		return nil, err
	}
	if sysID, err := m.systemSpaceID(ctx); err == nil && sysID != "" && sysID != spaceID {
		if err := collect(sysID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// evictSpaces 把除 keep 之外的空间运行时缓存全部作废，下次访问时按最新
// 安装状态重建。全局开关只改一条记录却影响所有空间，逐个重载会把一次
// 管理操作放大成 N 次初始化（还带 MCP 握手），所以只作废、不重建。
func (m *Manager) evictSpaces(keep string) {
	m.mu.Lock()
	others := make([]string, 0, len(m.inst))
	for id := range m.inst {
		if id != keep {
			others = append(others, id)
		}
	}
	for _, id := range others {
		delete(m.inst, id)
		for key := range m.mcpCs {
			if strings.HasPrefix(key, id+"/") {
				delete(m.mcpCs, key)
			}
		}
	}
	// 借用池按「会话空间 + 来源引用」缓存，来源空间或插件状态一变就可能过期，
	// 直接整体丢弃比逐个判断前缀安全。
	m.borrow = map[string][]*instance{}
	m.mu.Unlock()
	for _, id := range others {
		m.closeSpaceProcs(id)
	}
}

func (m *Manager) installOf(ctx context.Context, spaceID, slug string) (store.PluginInstall, error) {
	installs, err := m.o.Store.ListPluginInstalls(ctx, spaceID)
	if err != nil {
		return store.PluginInstall{}, err
	}
	for _, in := range installs {
		if in.Slug == slug {
			return in, nil
		}
	}
	return store.PluginInstall{}, store.ErrNotFound
}

// installAnywhere 把「系统空间的全局安装」也算作已安装：
// 内置件与全局智能体没有落到普通空间的安装记录，但内容对它们可读。
func (m *Manager) installAnywhere(ctx context.Context, spaceID, slug string) (store.PluginInstall, error) {
	in, err := m.installOf(ctx, spaceID, slug)
	if err == nil {
		return in, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.PluginInstall{}, err
	}
	sysID, sysErr := m.systemSpaceID(ctx)
	if sysErr != nil || sysID == "" || sysID == spaceID {
		return store.PluginInstall{}, err
	}
	return m.installOf(ctx, sysID, slug)
}

// references 检查卸载前的引用：会话绑定的技能、组件的 data_sources。
func (m *Manager) references(ctx context.Context, spaceID, slug string) ([]string, error) {
	var out []string
	n, err := m.o.Store.CountSessionsUsingSkill(ctx, spaceID, path.Join(DraftDir, slug)+"/")
	if err == nil && n > 0 {
		out = append(out, fmt.Sprintf("%d 个会话绑定了该技能", n))
	}
	for otherSlug, inst := range m.snapshot(spaceID) {
		if otherSlug == slug {
			continue
		}
		if inst.Manifest.Kind == KindAgent && inst.Manifest.Agent != nil {
			for _, dep := range inst.Manifest.Agent.Requires {
				if dep == slug {
					out = append(out, fmt.Sprintf("智能体 %s 依赖它", otherSlug))
				}
			}
		}
		if inst.Manifest.Kind != KindComponent {
			continue
		}
		for _, ds := range inst.Manifest.DataSources {
			if ds.Plugin == slug {
				out = append(out, fmt.Sprintf("组件 %s 的数据源 %s 依赖它", otherSlug, ds.ID))
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 工具暴露

// InstalledSources 把已安装插件映射成能力索引条目（space.SetInstalledSource）。
//
// 已安装但未落副本的预置件不在空间文件树里，只有这张表能让它进检索池；
// 路径统一为 published/<kind>/<slug>/<version>/<entry>，由 ReadPublished 解析。
func (m *Manager) InstalledSources(ctx context.Context, spaceID string) ([]space.IndexEntry, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, err
	}
	out := make([]space.IndexEntry, 0)
	for _, inst := range m.snapshot(spaceID) {
		if inst.Local {
			// 本地开发插件由空间文件索引负责，路径是 plugins/...，不走 published。
			continue
		}
		// 智能体不是「可检索能力」：它由会话选择器使用，正文不该被 plugin_search 暴出来。
		if inst.Manifest.Kind == KindAgent {
			continue
		}
		// 只有技能正文需要模型读；工具/组件/MCP 读清单就够（产物是脚本，不供阅读）。
		kind := "tools"
		entry := ManifestFile
		switch inst.Manifest.Kind {
		case KindSkill:
			kind = "skills"
			entry = inst.Manifest.Skill.Entry
			if entry == "" {
				entry = "SKILL.md"
			}
		case KindMCP:
			kind = "mcps"
		case KindComponent:
			kind = "components"
		}
		out = append(out, space.IndexEntry{
			Kind:        kind,
			Name:        inst.Manifest.Name,
			Description: inst.Manifest.Description,
			Path:        path.Join("published", kind, inst.Slug, inst.Version, entry),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadPublished 读已发布件里的文件（plugin_read 的 published/... 路径）。
//
// 访问控制：本空间已安装的插件，或 public 发布件。版本固定，内容不可变，
// 所以不需要按草稿可见性再判一次。
func (m *Manager) ReadPublished(ctx context.Context, spaceID, rel string) (string, error) {
	parts := strings.SplitN(rel, "/", 5)
	if len(parts) != 5 || parts[0] != "published" {
		return "", fmt.Errorf("发布件路径应形如 published/<kind>/<slug>/<version>/<文件>：%s", rel)
	}
	kind, slug, version, file := parts[1], parts[2], parts[3], parts[4]
	if file == "" || strings.Contains(file, "..") {
		return "", fmt.Errorf("发布件路径非法：%s", rel)
	}
	p, err := m.o.Store.GetPluginBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 模型偶尔会照着 skill 只造路径：直接说清「路径要来自检索结果」
			return "", fmt.Errorf("没有 slug 为 %s 的插件，路径请取自 plugin_search 的结果，不要自行拼造", slug)
		}
		return "", err
	}
	// 只有安装到当前空间（或系统空间的全局安装）才能读发布件内容；市场浏览不暴露正文。
	if _, err := m.installAnywhere(ctx, spaceID, slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("插件 %s 未安装到当前空间或系统空间，不能读取发布件", slug)
		}
		return "", err
	}
	if want := string(p.Kind) + "s"; kind != want {
		return "", fmt.Errorf("路径里的类型 %s 与插件 %s 的实际类型 %s 不符", kind, slug, want)
	}
	v, err := m.o.Store.GetPluginVersion(ctx, p.ID, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("插件 %s 没有版本 %s（可能已升级），请重新 plugin_search 取回最新路径", slug, version)
		}
		return "", err
	}
	body, err := ReadFile(ctx, m.o.Storage, v.ArtifactPath, file)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("发布件 %s 里没有 %s，可用 file_list 查看 plugins/%s 下的实际文件", rel, file, slug)
		}
		return "", err
	}
	return clipText(body, 512<<10), nil
}

// Tools 实现 tool.PluginProvider：返回空间内已安装插件的工具。
func (m *Manager) Tools(ctx context.Context, spaceID string) ([]tool.Tool, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, err
	}
	var out []tool.Tool
	for _, inst := range m.snapshot(spaceID) {
		out = append(out, inst.Tools...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ToolsFor 加载「借用插件」的工具：子会话继承发起方空间的智能体时使用。
//
// refs 里的插件按来源空间读文件与安装配置，实例归属 spaceID，所以执行期的文件
// 沙箱与策略仍按当前会话空间。某个依赖坏掉只跳过它，不让整个会话没有工具。
func (m *Manager) ToolsFor(ctx context.Context, spaceID string, refs []tool.PluginRef) ([]tool.Tool, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	key := borrowKey(spaceID, refs)
	m.mu.RLock()
	cached, ok := m.borrow[key]
	m.mu.RUnlock()
	if ok {
		return flattenTools(cached), nil
	}

	insts := make([]*instance, 0, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.SpaceID) == "" || strings.TrimSpace(ref.Slug) == "" {
			continue
		}
		inst, err := m.buildBorrowed(ctx, spaceID, ref.SpaceID, ref.Slug)
		if err != nil {
			m.o.Logger.Warn("借用插件失败",
				"space", spaceID, "source", ref.SpaceID, "slug", ref.Slug, "err", err)
			continue
		}
		insts = append(insts, inst)
	}

	m.mu.Lock()
	if m.borrow == nil {
		m.borrow = map[string][]*instance{}
	}
	m.borrow[key] = insts
	m.mu.Unlock()
	return flattenTools(insts), nil
}

// borrowKey 对引用集合排序，调用方传参顺序不影响缓存命中。
func borrowKey(spaceID string, refs []tool.PluginRef) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, r.SpaceID+"/"+r.Slug)
	}
	sort.Strings(parts)
	return spaceID + "|" + strings.Join(parts, ",")
}

func flattenTools(insts []*instance) []tool.Tool {
	var out []tool.Tool
	for _, inst := range insts {
		out = append(out, inst.Tools...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// buildBorrowed 优先用发布件（安装记录），其次用来源空间已启用的本地草稿。
// 来源插件的「启用」以它自己空间的记录为准，不看借用方空间有没有装过。
func (m *Manager) buildBorrowed(ctx context.Context, runSpaceID, sourceSpaceID, slug string) (*instance, error) {
	in, err := m.installOf(ctx, sourceSpaceID, slug)
	switch {
	case err == nil:
		if in.Status != store.PluginInstallEnabled {
			return nil, fmt.Errorf("插件未启用")
		}
		p, err := m.o.Store.GetPluginBySlug(ctx, slug)
		if err != nil {
			return nil, err
		}
		v, err := m.o.Store.GetPluginVersion(ctx, p.ID, in.Version)
		if err != nil {
			return nil, err
		}
		meta, err := Parse(v.Manifest, true)
		if err != nil {
			return nil, err
		}
		return m.buildInstanceAt(ctx, runSpaceID, sourceSpaceID, p.ID, slug, in.Version, meta, v.ArtifactPath)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	enabled, err := m.LocalEnabledMap(ctx, sourceSpaceID)
	if err != nil {
		return nil, err
	}
	if !enabled[slug] {
		return nil, fmt.Errorf("插件未启用")
	}
	meta, err := m.readDraft(ctx, sourceSpaceID, slug)
	if err != nil {
		return nil, err
	}
	return m.buildInstanceAt(ctx, runSpaceID, sourceSpaceID, "",
		slug, "", meta, DraftKey(m.o.Storage, sourceSpaceID, slug))
}

func (m *Manager) buildInstance(ctx context.Context, spaceID, pluginID, slug, version string, meta Manifest, dir string) (*instance, error) {
	return m.buildInstanceAt(ctx, spaceID, spaceID, pluginID, slug, version, meta, dir)
}

// buildInstanceAt 用 sourceSpaceID 读插件文件与安装配置，实例归属 runSpaceID。
//
// 跨空间借用（子会话继承发起方空间的智能体）走这条路径：插件内容、MCP 地址与
// 空间级配置都来自 A，但 inst.SpaceID 是 B，于是工具执行时的文件沙箱、凭证作用域
// 与策略判定仍落在当前会话空间，不会把 B 的文件读写带进 A。
func (m *Manager) buildInstanceAt(ctx context.Context, runSpaceID, sourceSpaceID, pluginID, slug, version string, meta Manifest, dir string) (*instance, error) {
	// 运行以空间副本为准：用户改完文件即生效，不用等发布；
	// 副本不存在（例如已被清理）才回落到发布件，保证已安装插件仍然可用。
	if draft := DraftKey(m.o.Storage, sourceSpaceID, slug); draft != "" {
		if _, err := m.o.Storage.Stat(ctx, path.Join(draft, ManifestFile)); err == nil {
			dir = draft
		}
	}
	// 安装级配置在这里合并：MCP 的服务地址/凭证按来源空间覆盖，发布快照不动。
	meta = m.applyInstallConfig(ctx, sourceSpaceID, slug, meta)
	risk, effects := Derive(meta)
	inst := &instance{
		SpaceID: runSpaceID, PluginID: pluginID, Slug: slug, Version: version,
		Manifest: meta, Dir: dir, Risk: risk, SideEffects: effects,
	}
	switch meta.Kind {
	case KindTool, KindMCP:
		// 平台内置工具已在能力注册表里，插件只做全局安装与展示，不能重复注册同名工具。
		if meta.Builtin {
			break
		}
		if meta.Kind == KindMCP {
			rt, err := newMCPRuntime(ctx, meta, m.o.Secrets, m.o.Store, m.o.Logger, m.mcpCachePath(meta.Slug))
			if err != nil {
				return nil, err
			}
			inst.MCP = rt
			inst.Tools = m.mcpTools(inst)
		} else {
			inst.Tools = m.toolTools(inst)
		}
	case KindSource:
		// 源的构造失败不应该让整个空间加载失败：其余插件与内置能力照常可用，
		// 这个源在 web_search 里表现为「没有可用源」或缺失，日志里有原因。
		src, err := m.buildSource(ctx, sourceSpaceID, slug, dir, meta)
		if err != nil {
			m.o.Logger.Warn("搜索源初始化失败", "slug", slug, "space", sourceSpaceID, "err", err)
			break
		}
		inst.Source = src
	}
	return inst, nil
}

func (m *Manager) toolTools(inst *instance) []tool.Tool {
	if inst.Manifest.Kind != KindTool {
		return nil
	}
	meta := inst.Manifest
	name := ToolName(inst.Slug, inst.Slug)
	return []tool.Tool{{
		Name:           name,
		Capability:     meta.Capability,
		Description:    meta.Description,
		Parameters:     meta.InputSchema,
		Risk:           inst.Risk,
		SideEffects:    inst.SideEffects,
		Runtime:        "plugin",
		NetworkWrite:   meta.WritesRemote(),
		MaxResultBytes: meta.Limits.MaxResultBytes,
		Invoke: func(ctx context.Context, args json.RawMessage, env tool.Env) (string, error) {
			return m.invokeTool(ctx, inst, args, env)
		},
	}}
}

// MCPAppKey 返回插件绑定的应用名与该应用的 JWT 签名密钥（明文）。
//
// 只对 auth.mode=jwt 的 MCP 插件有效。密钥已存在就复用：应用名是密钥的
// 唯一标识，重新发布不会让已经配好的 MCP Server 突然验签失败。
//
// 这是给部署者配置到 MCP Server 侧的物料，不进模型上下文，也不写审计详情。
func (m *Manager) MCPAppKey(ctx context.Context, spaceID, slug string) (app, key string, err error) {
	_, meta, err := m.Detail(ctx, spaceID, slug)
	if err != nil {
		return "", "", err
	}
	if meta.Kind != KindMCP || !meta.Auth.JWT() {
		return "", "", fmt.Errorf("%w：该插件没有声明 auth.mode=jwt", ErrConflict)
	}
	app = meta.Auth.AppName(meta.Slug)
	key, err = ensureAppKey(ctx, m.o.Secrets, app)
	if err != nil {
		return "", "", err
	}
	return app, key, nil
}

func (m *Manager) mcpTools(inst *instance) []tool.Tool {
	if inst.MCP == nil {
		return nil
	}
	out := make([]tool.Tool, 0, len(inst.MCP.tools))
	for _, exposed := range inst.MCP.names() {
		def := inst.MCP.tools[exposed]
		remote := inst.MCP.remote[exposed]
		name := ToolName(inst.Slug, exposed)
		schema := def.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, tool.Tool{
			Name:         name,
			Capability:   exposed, // 远端工具名：组件 data_sources 与工具寻址都按它对齐
			Description:  fmt.Sprintf("[插件:%s] %s", inst.Manifest.Slug, def.Description),
			Parameters:   schema,
			Risk:         inst.Risk,
			SideEffects:  inst.SideEffects,
			Runtime:      "plugin",
			NetworkWrite: inst.Manifest.WritesRemote(),
			ServerID:     inst.PluginID,
			ServerName:   inst.Slug,
			Invoke: func(ctx context.Context, args json.RawMessage, env tool.Env) (string, error) {
				return inst.MCP.call(ctx, remote, args, env)
			},
		})
	}
	return out
}

func (m *Manager) invokeTool(ctx context.Context, inst *instance, args json.RawMessage, env tool.Env) (string, error) {
	allow, verbs, secrets := inst.Manifest.HostAllow()
	fs, spaceDir, fsErr := m.sandboxFS(inst.SpaceID, inst.Dir, inst.Manifest)
	if fsErr != nil {
		return "", fsErr
	}
	out, err := m.rt.Call(ctx, invokeInput{
		Dir: inst.Dir, Entry: inst.Manifest.Runtime.Entry,
		Limits: inst.Manifest.Limits, SpaceID: inst.SpaceID,
		UserID: env.UserID, Slug: inst.Slug, FS: fs, SpaceRoot: spaceDir,
		NetHosts: inst.Manifest.NetHosts(), WSHosts: inst.Manifest.WSNetHosts(),
		Allow: allow, Verbs: verbs, Secrets: secrets,
		// 默认一次性：跑完即退，不占常驻池。声明 runtime.resident 才复用进程。
		OneShot: !inst.Manifest.Runtime.Resident,
	}, args)
	if err != nil {
		return "", err
	}
	return truncate(out, inst.Manifest.Limits.MaxResultBytes), nil
}

// registerTools 把插件工具注册进能力与工具两层，让策略与 Invoker 自动生效。
//
// 这里没有「绑定」这一步：工具能出现在列表里，就说明所属插件已安装/启用
// （MCP 是已注册），安装动作本身就是授权。
func (m *Manager) registerTools(ctx context.Context, inst *instance) error {
	for _, t := range inst.Tools {
		if err := m.o.Store.UpsertCapability(ctx, store.Capability{
			Name: t.Name, Description: t.Description, InputSchema: t.Parameters,
			RiskLevel: t.Risk, SideEffects: t.SideEffects,
		}); err != nil {
			return err
		}
		if err := m.o.Store.UpsertTool(ctx, store.ToolRecord{
			Name: t.Name, Capability: t.Name, Runtime: "plugin", RiskLevel: t.Risk,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- 加载缓存

func (m *Manager) ensureLoaded(ctx context.Context, spaceID string) error {
	m.mu.RLock()
	_, ok := m.inst[spaceID]
	m.mu.RUnlock()
	if ok {
		return nil
	}

	loaded := map[string]*instance{}
	// seen 记录「有明确安装记录」的 slug，不论启用还是停用。下面的兜底只补
	// 「系统空间还没建好 / 安装记录还没写」那一段，不能把管理员刚停用的内置件
	// 当成没装过又装回来——否则停用只是一个标签。
	seen := map[string]bool{}
	// loadInstall 把一个安装记录变成运行时实例。global 表示这条记录来自系统空间：
	// 它对本空间是只读的全局能力，但同样进入检索池与工具列表。
	loadInstall := func(in store.PluginInstall, global bool) {
		if in.Status != store.PluginInstallEnabled {
			return
		}
		if _, exists := loaded[in.Slug]; exists {
			return
		}
		p, err := m.o.Store.GetPluginBySlug(ctx, in.Slug)
		if err != nil {
			return
		}
		v, err := m.o.Store.GetPluginVersion(ctx, p.ID, in.Version)
		if err != nil {
			m.o.Logger.Warn("插件版本丢失，跳过加载", "slug", in.Slug, "version", in.Version, "err", err)
			return
		}
		meta, err := Parse(v.Manifest, true)
		if err != nil {
			m.o.Logger.Warn("插件清单非法，跳过加载", "slug", in.Slug, "err", err)
			return
		}
		inst, err := m.buildInstance(ctx, spaceID, p.ID, in.Slug, in.Version, meta, v.ArtifactPath)
		if err != nil {
			// MCP 服务暂时不可用不应该让整个空间没有工具
			m.o.Logger.Warn("插件运行时初始化失败", "slug", in.Slug, "err", err)
			inst = &instance{
				SpaceID: spaceID, PluginID: p.ID, Slug: in.Slug, Version: in.Version,
				Manifest: meta, Dir: v.ArtifactPath, Risk: in.Risk,
			}
		}
		inst.Global = global
		loaded[in.Slug] = inst
	}

	installs, err := m.o.Store.ListPluginInstalls(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, in := range installs {
		seen[in.Slug] = true
		loadInstall(in, false)
	}

	// 系统空间的安装记录就是「全局安装」：不往每个空间写副本，但所有空间都能用。
	// 平台内置技能/工具在启动时安装在系统空间，因此这一步同时解决了「全局默认可用」。
	sysID, sysErr := m.systemSpaceID(ctx)
	if sysErr == nil && sysID != "" && sysID != spaceID {
		globals, err := m.o.Store.ListPluginInstalls(ctx, sysID)
		if err != nil {
			m.o.Logger.Warn("读取全局安装失败", "space", spaceID, "err", err)
		} else {
			for _, in := range globals {
				seen[in.Slug] = true
				loadInstall(in, true)
			}
		}
	}

	// 空间本地插件：只有候选状态 enabled 才进入运行池。
	// 内容以空间目录为准，不写 published，也没有 install 记录。
	cands, err := m.o.Store.ListPluginCandidatesBySpace(ctx, spaceID)
	if err != nil {
		m.o.Logger.Warn("读取本地插件失败", "space", spaceID, "err", err)
	} else {
		for _, c := range cands {
			if c.Status != store.PluginCandidateEnabled {
				continue
			}
			if _, exists := loaded[c.Slug]; exists {
				continue
			}
			b, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, c.Slug), false)
			if err != nil {
				m.o.Logger.Warn("本地插件加载失败", "slug", c.Slug, "err", err)
				continue
			}
			inst, err := m.buildInstance(ctx, spaceID, "", c.Slug, "local", b.Meta, DraftKey(m.o.Storage, spaceID, c.Slug))
			if err != nil {
				m.o.Logger.Warn("本地插件运行时初始化失败", "slug", c.Slug, "err", err)
				continue
			}
			inst.Local = true
			loaded[c.Slug] = inst
		}
	}

	// 兜底：系统空间还没创建（管理员从未登录过）、或全局安装记录尚未写入时，
	// 按发布件把内置件当全局可用。初始化顺序问题不能让「基础能力」缺失，
	// 下次启动会把它们补成正式的系统空间安装。
	builtins, err := m.o.Store.ListPlugins(ctx)
	if err != nil {
		m.o.Logger.Warn("读取平台内置插件失败", "space", spaceID, "err", err)
	}
	for _, p := range builtins {
		if p.Origin != store.PluginOriginSystem || p.Status != store.PluginStatusEnabled {
			continue
		}
		if _, exists := loaded[p.Slug]; exists || seen[p.Slug] || p.LatestVersion == "" {
			continue
		}
		loadInstall(store.PluginInstall{
			Slug: p.Slug, Version: p.LatestVersion, Status: store.PluginInstallEnabled,
		}, true)
	}

	m.mu.Lock()
	m.inst[spaceID] = loaded
	for slug, inst := range loaded {
		if inst.MCP != nil {
			m.mcpCs[spaceID+"/"+slug] = inst.MCP
		}
	}
	m.mu.Unlock()

	// 搜索源在加载完成后统一注册：系统空间的那份是全局源（SpaceID 为空），
	// 普通空间自己装/启用的只对本空间生效。loadInstall 已按 Global 标记过，
	// 但系统空间自己加载时 Global=false，所以要再按空间类型判一次。
	for _, inst := range loaded {
		if inst.Source != nil {
			m.registerSource(spaceID, sysID, inst)
		}
	}
	return nil
}

// systemSpaceID 返回系统空间 ID；尚未初始化时返回错误（调用方按「无全局安装」降级）。
func (m *Manager) systemSpaceID(ctx context.Context) (string, error) {
	sp, err := m.o.Store.GetSpaceByType(ctx, space.SpaceTypeSystem)
	if err != nil {
		return "", err
	}
	return sp.ID, nil
}

// mcpCachePath 是 MCP 工具清单快照的落盘位置（宿主目录，不进空间文件区）。
func (m *Manager) mcpCachePath(slug string) string {
	if m.o.RuntimeDir == "" {
		return ""
	}
	return filepath.Join(m.o.RuntimeDir, "mcp", slug+".json")
}

func (m *Manager) snapshot(spaceID string) map[string]*instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*instance, len(m.inst[spaceID]))
	for k, v := range m.inst[spaceID] {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- 组件

// ComponentView 是组件渲染所需的最小集合：清单（含数据源声明）+ 校验过的 DSL。
type ComponentView struct {
	Slug     string          `json:"slug"`
	Version  string          `json:"version"`
	Height   int             `json:"height"`
	Manifest Manifest        `json:"manifest"`
	DSL      json.RawMessage `json:"dsl"`
}

// ComponentView 读取组件 DSL。版本固定：同一条消息永远渲染同一版本。
//
// 只有「装在本空间」的组件才能渲染：渲染就要取数，而取数会带着当前用户身份
// 去调工具，未安装却可渲染等于绕过安装审批。历史版本只要发布过就允许，
// 因为产物不可变、消息里固化的是当时的版本。
func (m *Manager) ComponentView(ctx context.Context, spaceID, slug, version string) (ComponentView, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return ComponentView{}, err
	}
	inst := m.snapshot(spaceID)[slug]
	if inst == nil || inst.Manifest.Kind != KindComponent {
		return ComponentView{}, fmt.Errorf("%w：组件 %s 未安装到当前空间", store.ErrNotFound, slug)
	}
	ver := strings.TrimSpace(version)
	if ver == "" {
		ver = inst.Version
	}

	meta := inst.Manifest
	body := ""
	if ver == inst.Version {
		// 运行以空间副本为准：用户改完 DSL 不必先发布就能在会话里看到效果。
		raw, err := ReadFile(ctx, m.o.Storage, inst.Dir, inst.Manifest.UI.Entry)
		if err != nil {
			return ComponentView{}, err
		}
		body = raw
	} else {
		p, err := m.o.Store.GetPluginBySlug(ctx, slug)
		if err != nil {
			return ComponentView{}, err
		}
		v, err := m.o.Store.GetPluginVersion(ctx, p.ID, ver)
		if err != nil {
			return ComponentView{}, err
		}
		raw, err := ReadFile(ctx, m.o.Storage, v.ArtifactPath, ManifestFile)
		if err != nil {
			return ComponentView{}, err
		}
		if meta, err = Parse([]byte(raw), true); err != nil {
			return ComponentView{}, err
		}
		if body, err = ReadFile(ctx, m.o.Storage, v.ArtifactPath, meta.UI.Entry); err != nil {
			return ComponentView{}, err
		}
	}

	height, err := ValidateDSL([]byte(body), meta.DataSources)
	if err != nil {
		return ComponentView{}, err
	}
	return ComponentView{
		Slug: slug, Version: ver, Height: height, Manifest: meta,
		DSL: json.RawMessage(body),
	}, nil
}

// Data 代组件调用数据源。数据源必须指向同空间已安装的插件工具。
//
// 走 Invoker 而不是直接 invokeTool：组件是第三方代码，它发起的调用同样要过
// 策略、审批与审计，否则组件就成了绕过策略的后门。
func (m *Manager) Data(ctx context.Context, spaceID, userID, perm, component, source string, params json.RawMessage, gesture bool) (string, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return "", err
	}
	comp := m.snapshot(spaceID)[component]
	if comp == nil || comp.Manifest.Kind != KindComponent {
		return "", fmt.Errorf("组件 %s 未安装", component)
	}
	var target *DataSource
	for i := range comp.Manifest.DataSources {
		if comp.Manifest.DataSources[i].ID == source {
			target = &comp.Manifest.DataSources[i]
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("组件没有声明数据源 %q", source)
	}
	if target.Mode == "action" && !gesture {
		return "", fmt.Errorf("%w：写操作必须由用户手势触发", ErrForbidden)
	}
	dep := m.snapshot(spaceID)[target.Plugin]
	if dep == nil {
		return "", fmt.Errorf("依赖的插件 %s 未安装", target.Plugin)
	}

	// 找到被声明的那个工具：工具插件只有一个入口，MCP 要按 allow 里的远端名对齐。
	var picked *tool.Tool
	for i := range dep.Tools {
		if dep.Manifest.Kind == KindTool || target.Tool == "" ||
			dep.Tools[i].Name == target.Tool || dep.Tools[i].Capability == target.Tool {
			picked = &dep.Tools[i]
			break
		}
	}
	if picked == nil {
		return "", fmt.Errorf("插件 %s 没有可调用的工具", target.Plugin)
	}
	if m.invoker == nil {
		return "", fmt.Errorf("工具执行器未装配，组件不能取数")
	}
	// 清单里的 params 是默认参数（例如固定的数据范围），请求参数覆盖它们。
	merged := make(map[string]any, len(target.Params)+4)
	for k, v := range target.Params {
		merged[k] = v
	}
	if len(params) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(params, &extra); err != nil {
			return "", fmt.Errorf("参数必须是对象：%w", err)
		}
		for k, v := range extra {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		merged = map[string]any{}
	}
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	res := m.invoker.Invoke(ctx, *picked, llm.ToolCall{
		Type:     "function",
		Function: llm.FunctionCall{Name: picked.Name, Arguments: string(mergedRaw)},
	}, tool.Env{SpaceID: spaceID, UserID: userID, SpacePerm: perm})
	if res.Status != tool.StatusOK {
		return "", fmt.Errorf("%s", res.Content)
	}
	return res.Content, nil
}

// ---------------------------------------------------------------- 基线

// BaselineStatus 是副本与上游基线的比对结果（方案 A：不自动合并）。
type BaselineStatus struct {
	BaselineVersion string `json:"baseline_version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	LocalModified   bool   `json:"local_modified"`
	Dismissed       bool   `json:"dismissed"`
}

func (m *Manager) BaselineStatus(ctx context.Context, spaceID, slug string) (BaselineStatus, error) {
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return BaselineStatus{}, err
	}
	return m.baseline(ctx, spaceID, slug, in.DismissedVersion)
}

func (m *Manager) baseline(ctx context.Context, spaceID, slug, dismissed string) (BaselineStatus, error) {
	draft, err := m.readDraft(ctx, spaceID, slug)
	if err != nil || draft.DerivedFrom == nil {
		return BaselineStatus{}, nil
	}
	p, err := m.o.Store.GetPluginBySlug(ctx, slug)
	if err != nil {
		return BaselineStatus{}, err
	}
	st := BaselineStatus{BaselineVersion: draft.DerivedFrom.Version, LatestVersion: p.LatestVersion}
	if p.LatestVersion == "" || p.LatestVersion == draft.DerivedFrom.Version {
		return st, nil
	}
	if _, err := m.o.Store.GetPluginVersion(ctx, p.ID, p.LatestVersion); err != nil {
		return st, nil
	}
	current, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug), false)
	if err == nil {
		st.LocalModified = current.ContentHash() != draft.DerivedFrom.SHA256
	}
	st.UpdateAvailable = p.LatestVersion != dismissed
	st.Dismissed = p.LatestVersion == dismissed
	return st, nil
}

// ResetBaseline 用上游最新版覆盖本地副本；覆盖前自动备份，可一键恢复。
func (m *Manager) ResetBaseline(ctx context.Context, userID, spaceID, slug string) error {
	draft, err := m.readDraft(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	if draft.DerivedFrom == nil {
		return fmt.Errorf("%w：该插件不是从基线派生的副本", ErrConflict)
	}
	p, err := m.o.Store.GetPluginBySlug(ctx, slug)
	if err != nil {
		return err
	}
	v, err := m.o.Store.GetPluginVersion(ctx, p.ID, p.LatestVersion)
	if err != nil {
		return err
	}
	dir := DraftKey(m.o.Storage, spaceID, slug)
	backup := path.Join(m.o.Storage.SpaceKey(spaceID), "files", DraftDir, ".history",
		slug, strconv.FormatInt(time.Now().Unix(), 10))
	if err := CopyDir(ctx, m.o.Storage, dir, backup); err != nil {
		return fmt.Errorf("备份当前副本失败，已中止: %w", err)
	}
	src, err := LoadBundleRaw(ctx, m.o.Storage, v.ArtifactPath)
	if err != nil {
		return err
	}
	meta, err := Parse(src.Files[ManifestFile], true)
	if err != nil {
		return err
	}
	// 覆盖时把版本号去掉（草稿不写版本），来源指向新基线（指纹只算内容）
	meta.Version = ""
	meta.DerivedFrom = &DerivedFrom{PluginID: p.ID, Version: v.Version, SHA256: src.ContentHash()}
	raw, _ := json.Marshal(meta)
	src.Files[ManifestFile] = raw
	if err := src.WriteTo(ctx, m.o.Storage, dir); err != nil {
		return err
	}
	m.recordAudit(ctx, userID, "plugin.reset_baseline", slug, v.Version, map[string]any{"backup": backup})
	return nil
}

// DismissUpdate 让某个版本的提示不再出现（用户偏好，不是内容）。
func (m *Manager) DismissUpdate(ctx context.Context, spaceID, slug, version string) error {
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	in.DismissedVersion = version
	_, err = m.o.Store.UpsertPluginInstall(ctx, in)
	return err
}

// ---------------------------------------------------------------- 默认安装集

// EnsureBootstrap 为空间补装默认集，只执行一次（卸载后不追溯）。
func (m *Manager) EnsureBootstrap(ctx context.Context, spaceID string) error {
	if m.isBootstrapped(spaceID) {
		return nil
	}
	marked, err := m.o.Store.MarkPluginBootstrapped(ctx, spaceID)
	if err != nil {
		return err
	}
	if !marked {
		m.markBootstrapped(spaceID)
		return nil
	}
	for _, slug := range m.o.DefaultInstall {
		p, err := m.o.Store.GetPluginBySlug(ctx, slug)
		if err != nil {
			m.o.Logger.Warn("默认安装集里的插件不存在，已跳过", "slug", slug, "err", err)
			continue
		}
		if p.LatestVersion == "" {
			m.o.Logger.Warn("默认安装集里的插件没有已发布版本，已跳过", "slug", slug)
			continue
		}
		// 平台内置件是隐式全空间可用，不需要装。
		if p.Origin == store.PluginOriginSystem {
			continue
		}
		if _, err := m.Install(ctx, InstallInput{
			SpaceID: spaceID, Slug: slug, Version: p.LatestVersion,
			Perm: "manage", IsAdmin: true,
		}); err != nil {
			m.o.Logger.Warn("默认安装失败", "slug", slug, "space", spaceID, "err", err)
		}
	}
	m.markBootstrapped(spaceID)
	return nil
}

func (m *Manager) isBootstrapped(spaceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.boot[spaceID]
}

func (m *Manager) markBootstrapped(spaceID string) {
	m.mu.Lock()
	m.boot[spaceID] = true
	m.mu.Unlock()
}

// ---------------------------------------------------------------- 辅助

// ensureCopy 保证空间里有一份与指定版本匹配的副本。
//
// 三种情况：没有副本 → 落一份；已有旧版副本且本地未改 → 覆盖成新版；
// 本地已改 → 拒绝（不静默合并，也不丢用户改动）。自建草稿只要内容与发布件一致就保留。
func (m *Manager) ensureCopy(ctx context.Context, spaceID, pluginID, slug, version, artifact string, meta Manifest) error {
	draftDir := DraftKey(m.o.Storage, spaceID, slug)
	existing, err := LoadBundle(ctx, m.o.Storage, draftDir, false)
	if err != nil {
		return m.materialize(ctx, spaceID, pluginID, slug, version, artifact, meta)
	}
	if existing.Meta.DerivedFrom != nil {
		if existing.Meta.DerivedFrom.Version == version {
			return nil
		}
		if existing.ContentHash() != existing.Meta.DerivedFrom.SHA256 {
			return fmt.Errorf("%w：副本已本地修改，不能直接升级；请先合并改动或恢复基线", ErrConflict)
		}
		if err := RemoveDir(ctx, m.o.Storage, draftDir); err != nil {
			return err
		}
		return m.materialize(ctx, spaceID, pluginID, slug, version, artifact, meta)
	}
	// 自建草稿：安装的是已发布版本，草稿必须与发布件内容一致，否则装进去的是旧内容。
	src, err := LoadBundleRaw(ctx, m.o.Storage, artifact)
	if err != nil {
		return err
	}
	if existing.ContentHash() != src.ContentHash() {
		return fmt.Errorf("%w：空间里的草稿与发布件内容不一致，请先重新发布", ErrConflict)
	}
	return nil
}

// materialize 把发布件复制进空间，写成带 derived_from 的派生副本。
//
// derived_from 是基线比对的唯一依据，所以必须在清单里写：副本的版本号清空
// （草稿不写版本），来源固定为「哪个插件的哪个版本、内容哈希是多少」。
func (m *Manager) materialize(ctx context.Context, spaceID, pluginID, slug, version, artifact string, meta Manifest) error {
	dir := DraftKey(m.o.Storage, spaceID, slug)
	if _, err := m.o.Storage.Stat(ctx, path.Join(dir, ManifestFile)); err == nil {
		return nil // 空间里已有草稿，不覆盖用户改动
	}
	src, err := LoadBundleRaw(ctx, m.o.Storage, artifact)
	if err != nil {
		return err
	}
	meta.Version = ""
	// 基线指纹只算内容：副本清单会被重写，清单进指纹会让副本天生「已被修改」。
	meta.DerivedFrom = &DerivedFrom{PluginID: pluginID, Version: version, SHA256: src.ContentHash()}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	src.Files[ManifestFile] = raw
	return src.WriteTo(ctx, m.o.Storage, dir)
}

// removeMaterialized 只在内容确实来自该发布件时才删，避免删掉用户草稿。
func (m *Manager) removeMaterialized(ctx context.Context, spaceID, slug, pluginID string) error {
	dir := DraftKey(m.o.Storage, spaceID, slug)
	draft, err := m.readDraft(ctx, spaceID, slug)
	if err != nil {
		return nil
	}
	if draft.DerivedFrom == nil || draft.DerivedFrom.PluginID != pluginID {
		return nil
	}
	return RemoveDir(ctx, m.o.Storage, dir)
}

func (m *Manager) recordAudit(ctx context.Context, actor, action, object, result string, detail map[string]any) {
	body, _ := json.Marshal(detail)
	if err := m.o.Store.AppendAuditLog(ctx, store.AuditEntry{
		Actor: actor, Action: action, Object: object, Result: result, Detail: body,
	}); err != nil {
		m.o.Logger.Warn("写插件审计失败", "action", action, "object", object, "err", err)
	}
}

func riskOf(m Manifest) int {
	risk, _ := Derive(m)
	return risk
}

func nextVersion(latest string) string {
	if latest == "" {
		return "0.1.0"
	}
	parts := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	if len(parts) != 3 {
		return "0.1.0"
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return "0.1.0"
	}
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "\n…（已按插件限额截断）"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clipText(strings.TrimSpace(s), 80)
}

// installMarker 写入空间安装副本，标记来源、版本与内容 hash。
// 用户可见的 plugin.json 之外单独放一份，避免被当作插件内容解析。
type installMarker struct {
	PluginID    string    `json:"plugin_id"`
	Slug        string    `json:"slug"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	InstalledBy string    `json:"installed_by"`
	InstalledAt time.Time `json:"installed_at"`
	Immutable   bool      `json:"immutable"`
}

// installCopy 把已发布产物复制到空间 plugins/<slug>/，覆盖旧安装副本。
// 若目录里是没有安装记录的同名开发插件，拒绝覆盖。
func (m *Manager) installCopy(ctx context.Context, spaceID string, p store.Plugin, v store.PluginVersion, userID string) error {
	dir := DraftKey(m.o.Storage, spaceID, p.Slug)
	if _, err := m.installOf(ctx, spaceID, p.Slug); err != nil {
		// 没有安装记录时，目录只能是本地开发插件；不允许安装覆盖。
		if _, statErr := m.o.Storage.Stat(ctx, dir+"/"+ManifestFile); statErr == nil {
			return fmt.Errorf("%w：本空间存在同名本地插件 %s，请先改名或归档", ErrConflict, p.Slug)
		}
	}
	src, err := LoadBundleRaw(ctx, m.o.Storage, v.ArtifactPath)
	if err != nil {
		return err
	}
	if err := RemoveDir(ctx, m.o.Storage, dir); err != nil {
		return err
	}
	if err := src.WriteTo(ctx, m.o.Storage, dir); err != nil {
		return err
	}
	marker, _ := json.MarshalIndent(installMarker{
		PluginID: p.ID, Slug: p.Slug, Version: v.Version, SHA256: src.Hash,
		InstalledBy: userID, InstalledAt: time.Now().UTC(), Immutable: true,
	}, "", "  ")
	if _, err := m.o.Storage.Put(ctx, dir+"/.install.json", bytes.NewReader(marker), int64(len(marker))); err != nil {
		return err
	}
	return nil
}
