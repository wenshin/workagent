package plugin

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// Bootstrap 在服务启动时做三件事：发布内置技能与工具（并全局安装）、迁移存量插件、补默认安装集。
// 任何一步失败都只告警：插件体系不可用不应该让整个服务起不来。
func (m *Manager) Bootstrap(ctx context.Context) error {
	if err := m.SeedBuiltinPlugins(ctx); err != nil {
		m.o.Logger.Warn("发布内置插件失败", "err", err)
	}

	spaces, err := m.o.Store.ListAllSpaces(ctx)
	if err != nil {
		return err
	}
	for _, sp := range spaces {
		if err := m.migrateSpace(ctx, sp); err != nil {
			m.o.Logger.Warn("迁移存量插件失败", "space", sp.ID, "err", err)
		}
		// 已存在的空间不追溯默认安装集（改配置只影响之后新建的空间）：
		// 这里只打标记，避免升级部署后把新预置一股脑塞进历史空间。
		if _, err := m.o.Store.MarkPluginBootstrapped(ctx, sp.ID); err != nil {
			m.o.Logger.Warn("标记空间插件初始化状态失败", "space", sp.ID, "err", err)
		}
		m.markBootstrapped(sp.ID)
	}

	// 预热系统空间：全局搜索源（kind=source）是在 ensureLoaded 时注册的，
	// 不预热就要等某个空间被访问之后才生效。
	if sysID, sysErr := m.systemSpaceID(ctx); sysErr == nil && sysID != "" {
		if err := m.ensureLoaded(ctx, sysID); err != nil {
			m.o.Logger.Warn("预热系统空间失败", "err", err)
		}
	}
	return nil
}

// migrateSpace 一次性迁移：旧技能目录 → plugins/<slug>/，旧只读投影目录删除。
func (m *Manager) migrateSpace(ctx context.Context, sp store.Space) error {
	root := path.Join(m.o.Storage.SpaceKey(sp.ID), "files")
	entries, err := m.o.Storage.List(ctx, root)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		name := path.Base(e.Key)
		switch name {
		case DraftDir:
			continue
		case "skills", "tools", "mcps":
			// 系统空间里的这些目录是平台托管内容，内置件已改为发布态；
			// 其他空间出现同名目录说明是旧布局，整体迁走后删除。
			if err := m.migrateLegacyDir(ctx, sp, e.Key, name); err != nil {
				m.o.Logger.Warn("迁移旧目录失败", "space", sp.ID, "dir", name, "err", err)
			}
			continue
		case "agents":
			// 旧版智能体目录：迁移成 kind=agent 插件后移除，不再保留目录
			// （兼容读取会让「智能体只有一个来源」这件事变得模糊）。
			if err := m.migrateLegacyAgents(ctx, sp, e.Key); err != nil {
				m.o.Logger.Warn("迁移旧 agents 目录失败", "space", sp.ID, "err", err)
			}
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if err := m.migrateOneSkill(ctx, sp.ID, e.Key, name); err != nil {
			m.o.Logger.Warn("迁移技能失败", "space", sp.ID, "dir", name, "err", err)
		}
	}
	return nil
}

// migrateLegacyDir 处理 skills / tools / mcps：先备份，再迁技能，最后删除。
func (m *Manager) migrateLegacyDir(ctx context.Context, sp store.Space, dirKey, name string) error {
	root := path.Join(m.o.Storage.SpaceKey(sp.ID), "files")
	backup := path.Join(root, DraftDir, ".history", "legacy",
		strconv.FormatInt(time.Now().Unix(), 10), name)
	if err := CopyDir(ctx, m.o.Storage, dirKey, backup); err != nil {
		m.o.Logger.Warn("备份旧目录失败，跳过删除", "dir", name, "err", err)
		return nil
	}

	if name == "skills" {
		children, err := m.o.Storage.List(ctx, dirKey)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.IsDir {
				continue
			}
			// 系统空间的内置技能已在发布态重建，不再迁回空间文件。
			if sp.Type == space.SpaceTypeSystem {
				continue
			}
			_ = m.migrateOneSkill(ctx, sp.ID, child.Key, path.Base(child.Key))
		}
	}
	m.o.Logger.Info("旧能力目录已迁移并移除", "space", sp.ID, "dir", name, "backup", backup)
	return m.o.Storage.Delete(ctx, dirKey)
}

// migrateLegacyAgents 把系统空间 agents/ 迁移成 kind=agent 插件，然后删掉目录。
//
// 迁移是「发布 + 全局安装」：原 agents/<id>/ 变成 origin=system 的 agent 插件，
// 装进系统空间即对所有空间可选。目录先备份到 plugins/.history/legacy/<ts>/agents，
// 出问题时人工还能捞回原文。
func (m *Manager) migrateLegacyAgents(ctx context.Context, sp store.Space, dirKey string) error {
	root := path.Join(m.o.Storage.SpaceKey(sp.ID), "files")
	backup := path.Join(root, DraftDir, ".history", "legacy",
		strconv.FormatInt(time.Now().Unix(), 10), "agents")
	if err := CopyDir(ctx, m.o.Storage, dirKey, backup); err != nil {
		m.o.Logger.Warn("备份旧 agents 目录失败，跳过删除", "space", sp.ID, "err", err)
		return nil
	}

	if sp.Type == space.SpaceTypeSystem {
		children, err := m.o.Storage.List(ctx, dirKey)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.IsDir {
				continue
			}
			id := path.Base(child.Key)
			if err := m.migrateOneAgent(ctx, sp.ID, child.Key, id); err != nil {
				m.o.Logger.Warn("智能体迁移失败，备份里保留原文", "id", id, "err", err)
			}
		}
	}
	m.o.Logger.Info("旧 agents 目录已迁移并移除", "space", sp.ID, "backup", backup)
	return m.o.Storage.Delete(ctx, dirKey)
}

// legacyAgentSpec 是旧版 agents/<id>/agent.json 的结构，解析尽量宽松：
// 迁移要能容忍历史文件的字段偏差，不能因为一个多余字段整目录搬不走。
type legacyAgentSpec struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Extends     string `json:"extends"`
	Prompt      string `json:"prompt"`
	Enabled     *bool  `json:"enabled"`
}

func (m *Manager) migrateOneAgent(ctx context.Context, spaceID, dirKey, dirName string) error {
	var spec legacyAgentSpec
	if raw, err := ReadFile(ctx, m.o.Storage, dirKey, "agent.json"); err == nil {
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			return fmt.Errorf("%s/agent.json 非法: %w", dirName, err)
		}
	}
	if spec.Enabled != nil && !*spec.Enabled {
		// 旧语义里停用的智能体不出现在选择器；迁成插件会变成全局可选，跳过更保真。
		return fmt.Errorf("已停用，跳过迁移（原文在备份里）")
	}

	slug := strings.TrimSpace(spec.ID)
	if slug == "" {
		slug = dirName
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = slug
	}
	if !validSlug(slug) {
		slug = slugify(name)
		if !validSlug(slug) {
			return fmt.Errorf("无法从 %q 推导合法 slug", dirName)
		}
	}
	desc := strings.TrimSpace(spec.Description)
	if desc == "" {
		desc = "从旧版 agents/ 迁移，建议补充描述（选择器里靠它区分智能体）"
	}
	entry := strings.TrimSpace(spec.Prompt)
	if entry == "" {
		entry = "prompt.md"
	}
	body, err := ReadFile(ctx, m.o.Storage, dirKey, entry)
	if err != nil {
		return fmt.Errorf("%s 缺少提示词正文 %s: %w", dirName, entry, err)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%s 的提示词正文为空", dirName)
	}

	// 同名 slug 已被非内置插件占用时不抢占：跳过迁移，原文留在备份里。
	if p, err := m.o.Store.GetPluginBySlug(ctx, slug); err == nil && p.Origin != store.PluginOriginSystem {
		return fmt.Errorf("slug %s 已被其他插件占用，跳过迁移", slug)
	}

	meta := Manifest{
		Protocol:    Protocol,
		Kind:        KindAgent,
		Slug:        slug,
		Name:        name,
		Description: desc,
		Capability:  CapAgentUse,
		Agent:       &AgentSpec{Entry: AgentEntry, Extends: strings.TrimSpace(spec.Extends)},
		Extensible:  boolPtr(false),
	}
	if err := m.seedBuiltin(ctx, spaceID, spaceID, meta, map[string]string{AgentEntry: body}); err != nil {
		return err
	}
	m.o.Logger.Info("智能体已迁移为插件", "space", spaceID, "slug", slug, "name", name)
	return nil
}

// validSlug 与清单校验保持同一口径：小写字母开头，[a-z0-9_]，2-32 位。
func validSlug(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
		if i == 0 && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// migrateOneSkill 把一个带 .meta.json 的技能目录转换成插件目录。
func (m *Manager) migrateOneSkill(ctx context.Context, spaceID, dirKey, name string) error {
	raw, err := ReadFile(ctx, m.o.Storage, dirKey, ".meta.json")
	if err != nil {
		return nil // 不是技能目录
	}
	var meta struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return fmt.Errorf("%s 的 .meta.json 非法: %w", name, err)
	}
	if meta.Type != "" && meta.Type != "skill" {
		return nil
	}
	display := meta.Name
	if display == "" {
		display = name
	}
	desc := meta.Description
	if desc == "" {
		desc = "从旧版技能迁移，请补充描述（模型靠它判断何时使用）"
	}

	slug := slugify(display)
	target := DraftKey(m.o.Storage, spaceID, slug)
	if _, err := m.o.Storage.Stat(ctx, path.Join(target, ManifestFile)); err == nil {
		// 已经迁移过：删除旧目录，避免索引里出现两份
		return m.o.Storage.Delete(ctx, dirKey)
	}

	src, err := LoadBundleRaw(ctx, m.o.Storage, dirKey)
	if err != nil {
		return err
	}
	// 单一定义点：正文里不再保留 frontmatter
	if body, ok := src.Files["SKILL.md"]; ok {
		src.Files["SKILL.md"] = []byte(stripFrontmatter(string(body)))
	}
	if len(src.Files) == 0 {
		return fmt.Errorf("%s 没有可迁移的内容", name)
	}

	manifest := Manifest{
		Protocol:    Protocol,
		Kind:        KindSkill,
		Slug:        slug,
		Name:        display,
		Description: desc,
		Capability:  CapSkillUse,
		Skill:       &SkillSpec{Entry: "SKILL.md"},
	}
	body, _ := json.MarshalIndent(manifest, "", "  ")
	src.Files[ManifestFile] = body
	if err := src.WriteTo(ctx, m.o.Storage, target); err != nil {
		return err
	}
	if err := m.o.Storage.Delete(ctx, dirKey); err != nil {
		m.o.Logger.Warn("旧技能目录删除失败，请手工清理", "dir", name, "err", err)
	}
	m.o.Logger.Info("技能已迁移为插件", "space", spaceID, "slug", slug, "name", display)
	return nil
}

// slugify 把展示名转成合法 slug；中文名没有 ASCII 可转，退化成内容哈希。
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if len(s) < 2 {
		sum := sha1.Sum([]byte(name))
		s = "s_" + hex.EncodeToString(sum[:])[:8]
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// stripFrontmatter 去掉 Agent Skills 的 YAML frontmatter：元数据只认 plugin.json。
func stripFrontmatter(body string) string {
	if !strings.HasPrefix(body, "---\n") && !strings.HasPrefix(body, "---\r\n") {
		return body
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(body, "---\r\n"), "---\n")
	for _, sep := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return strings.TrimLeft(rest[i+len(sep):], "\r\n")
		}
	}
	return body
}
