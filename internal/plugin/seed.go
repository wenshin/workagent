package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"

	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// builtinSeedRevision 参与内置件版本指纹，用来强制换代。
const builtinSeedRevision = "/rev3"

// SeedBuiltinPlugins 发布平台内置技能与工具，并把它们安装到系统空间。
//
// 「安装到系统空间」就是「全局默认安装」：系统空间 `plugins/<slug>/` 是全局安装目录，
// 其他空间在 ensureLoaded 时把系统空间的安装记录一并加载。这样内置能力既在文件树里可见，
// 又不需要每个空间各装一遍，也不会出现「平台上没有装」的中间态。
func (m *Manager) SeedBuiltinPlugins(ctx context.Context) error {
	ownerID := ""
	sysID := ""
	if sys, err := m.o.Store.GetSpaceByType(ctx, space.SpaceTypeSystem); err == nil {
		ownerID, sysID = sys.ID, sys.ID
	}

	keep := map[string]bool{}
	for _, sk := range space.BuiltinSkills() {
		keep[sk.Slug] = true
		files := map[string]string{"SKILL.md": sk.Doc}
		for name, content := range sk.Files {
			files[name] = content
		}
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindSkill,
			Slug:        sk.Slug,
			Name:        sk.Name,
			Description: sk.Description,
			Capability:  CapSkillUse,
			Skill:       &SkillSpec{Entry: "SKILL.md"},
			Extensible:  boolPtr(false),
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, files); err != nil {
			return fmt.Errorf("发布内置技能 %s 失败: %w", sk.Slug, err)
		}
	}

	for _, ag := range builtinAgents() {
		keep[ag.Slug] = true
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindAgent,
			Slug:        ag.Slug,
			Name:        ag.Name,
			Description: ag.Description,
			Capability:  CapAgentUse,
			Agent:       &AgentSpec{Entry: AgentEntry},
			Extensible:  boolPtr(false),
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, map[string]string{AgentEntry: ag.Doc}); err != nil {
			return fmt.Errorf("发布内置智能体 %s 失败: %w", ag.Slug, err)
		}
	}

	for _, bm := range tool.BuiltinManifests() {
		keep[builtinToolSlug(bm.Name)] = true
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindTool,
			Slug:        builtinToolSlug(bm.Name),
			Name:        builtinToolLabel(bm.Name),
			Description: bm.Description,
			Capability:  builtinToolCapability(bm.Name),
			Extensible:  boolPtr(false),
			Builtin:     true,
			// 内置工具没有清单可推导副作用：风险直接取 Go 侧实现的那份声明。
			DeriveRisk:    bm.Risk,
			DeriveEffects: bm.SideEffects,
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, nil); err != nil {
			return fmt.Errorf("发布内置工具 %s 失败: %w", bm.Name, err)
		}
	}

	// 预置搜索源放在最后：它实现的是 search.query，不依赖其它内置件。
	// 预置的意义是「新部署装完就有搜索能力」，而不是给平台留一条绕开插件体系的旁路——
	// 它在市场里可见、可被用户自己的源按 priority 顶掉，也可以在系统空间的插件页停用
	// （停用对全部空间生效，见 Manager.SetEnabled）。
	for _, src := range builtinSources() {
		keep[src.Slug] = true
		spec := src.Source
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindSource,
			Slug:        src.Slug,
			Name:        src.Name,
			Description: src.Description,
			Capability:  CapSearchQuery,
			Source:      &spec,
			Extensible:  boolPtr(false),
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, src.Files); err != nil {
			return fmt.Errorf("发布内置搜索源 %s 失败: %w", src.Slug, err)
		}
	}

	// 最后清理已经从代码里下线的内置件（例如 builtin_ui 合并进 builtin_plugin_dev）。
	// 失败只告警：多留一条市场记录不影响服务启动，下次启动还会再试一次。
	if err := m.pruneBuiltins(ctx, sysID, keep); err != nil {
		m.o.Logger.Warn("清理下线的内置件失败", "err", err)
	}
	return nil
}

// pruneBuiltins 删掉再也没有发布源的内置件。
//
// seed 只做「发布 + 全局安装」，从不删除：一个内置技能被合并进另一个之后，
// plugin 记录、安装记录、空间目录、发布产物都还在，市场里会多出一条点不开的
// 「平台内置」。这里按 origin=system 且不在当前保留集合里来清理——
// 用户自己发布的插件 origin 不是 system，不会被误删。
func (m *Manager) pruneBuiltins(ctx context.Context, sysID string, keep map[string]bool) error {
	list, err := m.o.Store.ListPlugins(ctx)
	if err != nil {
		return err
	}
	for _, p := range list {
		if p.Origin != store.PluginOriginSystem || keep[p.Slug] {
			continue
		}
		// 先清它注册的能力与工具绑定，再删 plugin（版本、安装、扫描记录级联删）。
		if err := m.o.Store.DeletePluginToolRecords(ctx, p.Slug); err != nil {
			m.o.Logger.Warn("清理下线内置件的能力记录失败", "slug", p.Slug, "err", err)
			continue
		}
		if err := m.o.Store.DeletePlugin(ctx, p.ID); err != nil {
			m.o.Logger.Warn("删除下线内置件失败", "slug", p.Slug, "err", err)
			continue
		}
		if sysID != "" {
			dir := path.Join(m.o.Storage.SpaceKey(sysID), "files", DraftDir, p.Slug)
			if err := m.o.Storage.Delete(ctx, dir); err != nil {
				m.o.Logger.Warn("删除下线内置件的空间目录失败", "slug", p.Slug, "err", err)
			}
		}
		// 发布产物按 plugin_id 分目录，跟着记录一起清掉。
		if err := m.o.Storage.Delete(ctx, path.Join(publishedRoot, p.ID)); err != nil {
			m.o.Logger.Warn("删除下线内置件的发布产物失败", "slug", p.Slug, "err", err)
		}
		m.o.Logger.Info("已下线内置件", "slug", p.Slug)
	}
	return nil
}

// seedBuiltin 发布一个内置版本并确保它已安装到系统空间（全局安装）。
func (m *Manager) seedBuiltin(ctx context.Context, ownerID, sysID string, meta Manifest, extra map[string]string) error {
	files := map[string]string{}
	for name, content := range extra {
		files[name] = content
	}
	// 版本由内容指纹推导：内容没变版本不变，改了内容就发新版，旧版本继续可用。
	//
	// 清单本身也要进指纹：内置件的清单是代码生成的，字段增删（例如
	// capabilities[].paths 下线）不产生新版本的话，库里会一直留着那份旧清单，
	// 加载时按严格解析直接失败（unknown field），表现为「内置工具莫名其妙没了」。
	rawManifest, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(meta.Slug + builtinSeedRevision))
	_, _ = h.Write(rawManifest)
	for _, name := range sortedKeys(files) {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte(files[name]))
	}
	version := fmt.Sprintf("1.0.%d", int(h.Sum32())%10000)
	meta.Version = version

	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	files[ManifestFile] = string(raw)

	p, err := m.o.Store.GetPluginBySlug(ctx, meta.Slug)
	switch {
	case err == nil && p.LatestVersion == version:
		// 内容没变，直接确保全局安装到位（首次升级后可能还没装）
		return m.ensureGlobalInstall(ctx, sysID, meta.Slug, version)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	latest := ""
	if err == nil {
		latest = p.LatestVersion
	}
	p, err = m.o.Store.UpsertPlugin(ctx, store.Plugin{
		Slug: meta.Slug, Kind: string(meta.Kind), Name: meta.Name,
		Description: meta.Description, Visibility: store.PluginVisibilityPublic,
		Status: store.PluginStatusEnabled, LatestVersion: latest,
		Extensible: meta.IsExtensible(), OwnerSpaceID: ownerID, Origin: store.PluginOriginSystem,
	})
	if err != nil {
		return err
	}

	bundle := map[string][]byte{}
	for name, content := range files {
		bundle[name] = []byte(content)
	}
	b := &Bundle{Files: bundle, Hash: HashDir(bundle)}
	target := PublishedKey(p.ID, version)
	if err := b.WriteTo(ctx, m.o.Storage, target); err != nil {
		return err
	}
	risk, _ := Derive(meta)
	caps, _ := json.Marshal(meta.Capabilities)
	if _, err := m.o.Store.AddPluginVersion(ctx, store.PluginVersion{
		PluginID: p.ID, Version: version, Manifest: raw, ArtifactPath: target,
		SHA256: b.Hash, Capabilities: caps, Risk: int(risk), Notes: "平台预置",
	}, version); err != nil {
		return err
	}
	m.o.Logger.Info("内置插件已发布", "slug", meta.Slug, "kind", meta.Kind, "version", version)
	return m.ensureGlobalInstall(ctx, sysID, meta.Slug, version)
}

// ensureGlobalInstall 把内置件装进系统空间；系统空间还没建好时跳过，
// 运行期会退化为「按发布件全局可用」，下次启动补齐。
func (m *Manager) ensureGlobalInstall(ctx context.Context, sysID, slug, version string) error {
	if sysID == "" {
		return nil
	}
	prev, prevErr := m.o.Store.GetPluginInstall(ctx, sysID, slug)
	if prevErr == nil && prev.Version == version {
		return nil
	}
	if _, err := m.Install(ctx, InstallInput{
		SpaceID: sysID, Slug: slug, Version: version, Perm: "manage", IsAdmin: true,
	}); err != nil {
		return err
	}
	// 升级不改变开关状态：内置搜索源的「停用」是系统管理员的一次显式决定，
	// 平台换版只该换代码。Install 一律写成 enabled，这里按旧记录翻回去，
	// 并把刚建好的运行时摘掉（停用态本来就不该被加载）。
	if prevErr == nil && prev.Status == store.PluginInstallDisabled {
		prev.Version, prev.Status = version, store.PluginInstallDisabled
		if _, err := m.o.Store.UpsertPluginInstall(ctx, prev); err != nil {
			return err
		}
		m.unregisterSource(sysID, slug, true)
		m.closeSlugProcs(sysID, slug)
		m.mu.Lock()
		delete(m.inst[sysID], slug)
		delete(m.mcpCs, sysID+"/"+slug)
		m.mu.Unlock()
	}
	return nil
}

// builtinToolSlug 给平台原生工具一个稳定的插件 slug（工具名 -> tool_<name>）。
func builtinToolSlug(name string) string {
	return "tool_" + name
}

// builtinToolLabel 是市场里展示的名称：工具名对业务用户没有意义。
var builtinToolLabels = map[string]string{
	"time_now":       "当前时间",
	"space_list":     "空间列表",
	"plugin_search":  "检索插件",
	"plugin_read":    "读取插件",
	"session_list":   "会话列表",
	"file_list":      "列目录",
	"file_read":      "读文件",
	"file_write":     "写文件",
	"web_search":     "联网搜索",
	"shell_exec":     "执行命令",
	"secret_request": "录入凭证",
	"mcp_app_key":    "MCP 应用密钥",
	"wiki_list":      "列知识页",
	"wiki_read":      "读知识页",
	"wiki_write":     "写知识页",
	"wiki_search":    "搜知识库",
}

func builtinToolLabel(name string) string {
	if label, ok := builtinToolLabels[name]; ok {
		return label
	}
	return name
}

// builtinToolCapability 是插件层的单一能力标签（决定默认权限口径）。
func builtinToolCapability(name string) string {
	switch name {
	case "file_write", "wiki_write":
		return CapFSWrite
	case "wiki_list", "wiki_read", "wiki_search":
		// wiki 是跨空间共享区，但读写仍是文件语义：按 fs 能力口径归类，
		// 免得在市场里被当成「可调外部插件」而抬高风险。
		return CapFSRead
	case "shell_exec":
		return CapShellExec
	case "web_search":
		return CapSearchQuery
	}
	return CapPluginInvok
}

func boolPtr(b bool) *bool { return &b }

func fnv32(s string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32())
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 小切片，插入排序足够
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
