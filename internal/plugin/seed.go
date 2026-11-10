package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"

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

	for _, sk := range space.BuiltinSkills() {
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
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindAgent,
			Slug:        ag.Slug,
			Name:        ag.Name,
			Description: ag.Description,
			Capability:  CapAgentUse,
			Agent:       &AgentSpec{Entry: AgentEntry, Extends: ag.Extends},
			Extensible:  boolPtr(false),
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, map[string]string{AgentEntry: ag.Doc}); err != nil {
			return fmt.Errorf("发布内置智能体 %s 失败: %w", ag.Slug, err)
		}
	}

	for _, bm := range tool.BuiltinManifests() {
		meta := Manifest{
			Protocol:     Protocol,
			Kind:         KindTool,
			Slug:         builtinToolSlug(bm.Name),
			Name:         builtinToolLabel(bm.Name),
			Description:  bm.Description,
			Capability:   builtinToolCapability(bm.Name),
			Extensible:   boolPtr(false),
			Builtin:      true,
			Capabilities: builtinToolRisk(bm.Name),
		}
		if err := m.seedBuiltin(ctx, ownerID, sysID, meta, nil); err != nil {
			return fmt.Errorf("发布内置工具 %s 失败: %w", bm.Name, err)
		}
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
	h := fnv.New32a()
	_, _ = h.Write([]byte(meta.Slug + builtinSeedRevision))
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
	if in, err := m.o.Store.GetPluginInstall(ctx, sysID, slug); err == nil && in.Version == version {
		return nil
	}
	if _, err := m.Install(ctx, InstallInput{
		SpaceID: sysID, Slug: slug, Version: version, Perm: "manage", IsAdmin: true,
	}); err != nil {
		return err
	}
	return nil
}

// builtinToolSlug 给平台原生工具一个稳定的插件 slug（工具名 -> tool_<name>）。
func builtinToolSlug(name string) string {
	return "tool_" + name
}

// builtinToolLabel 是市场里展示的名称：工具名对业务用户没有意义。
var builtinToolLabels = map[string]string{
	"time_now":        "当前时间",
	"resource_search": "检索资源",
	"resource_read":   "读取资源",
	"session_list":    "会话列表",
	"file_list":       "列目录",
	"file_read":       "读文件",
	"file_write":      "写文件",
	"skill_create":    "创建技能",
	"web_search":      "联网搜索",
	"shell_exec":      "执行命令",
	"secret_request":  "录入凭证",
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
	case "file_write", "skill_create":
		return CapFSWrite
	case "shell_exec":
		return CapShellExec
	case "web_search":
		return CapSearchQuery
	}
	return CapPluginInvok
}

// builtinToolRisk 用能力声明表达真实副作用，风险由 Derive 推导，不在清单里写死。
func builtinToolRisk(name string) []Capability {
	switch name {
	case "file_write", "skill_create":
		return []Capability{{Method: MethodFSWr, Paths: []string{"plugins/"}}}
	case "shell_exec":
		return []Capability{{Method: MethodShell}}
	case "web_search":
		return []Capability{{Method: MethodSearch}}
	}
	return nil
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
