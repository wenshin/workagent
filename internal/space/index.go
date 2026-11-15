package space

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/storage"
)

// 空间根目录下的能力索引文件：汇总该空间的 skills / tools / mcps，
// 供 Agent 与会话技能选择器使用。隐藏文件，不进左侧树。
const spaceIndexFile = ".meta.json"

const indexVersion = 2

// IndexEntry 是索引中的一条资源。
//
// Source 只在合并系统空间时标记，落盘不写。
type IndexEntry struct {
	Kind        string `json:"-"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path 相对空间 files/，可直接用于读取正文
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
}

// IndexBundle 是一次读取的三类资源。
type IndexBundle struct {
	Skills     []IndexEntry
	Tools      []IndexEntry
	MCPS       []IndexEntry
	Components []IndexEntry
}

// spaceIndex 是 files/.meta.json 的文件结构。
type spaceIndex struct {
	Version    int          `json:"version"`
	UpdatedAt  string       `json:"updated_at"`
	Skills     []IndexEntry `json:"skills"`
	Tools      []IndexEntry `json:"tools"`
	MCPS       []IndexEntry `json:"mcps"`
	Components []IndexEntry `json:"components"`
}

// 资源来源：同名时 current 优先于 system。
const (
	SourceCurrent = "current"
	SourceSystem  = "system"
)

// RebuildIndex 重新扫描空间文件区并覆盖写 files/.meta.json。
//
// 所有会改变资源集合的写操作都应调用它；读取侧另有兜底（LoadIndex 发现缺失会重建）。
func (s *Service) RebuildIndex(ctx context.Context, spaceID string) error {
	idx := s.buildIndex(ctx, spaceID)
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.storage.Put(ctx, path.Join(s.filesKey(spaceID), spaceIndexFile),
		strings.NewReader(string(data)), int64(len(data)))
	return err
}

// LoadIndex 读取空间索引；文件缺失、损坏或版本不符时按磁盘现状重建。
func (s *Service) LoadIndex(ctx context.Context, spaceID string) IndexBundle {
	key := path.Join(s.filesKey(spaceID), spaceIndexFile)
	if f, err := s.storage.Get(ctx, key); err == nil {
		var idx spaceIndex
		decodeErr := json.NewDecoder(io.LimitReader(f, 256<<10)).Decode(&idx)
		_ = f.Close()
		if decodeErr == nil && idx.Version == indexVersion {
			return IndexBundle{Skills: idx.Skills, Tools: idx.Tools, MCPS: idx.MCPS, Components: idx.Components}
		}
	}

	idx := s.buildIndex(ctx, spaceID)
	if data, err := json.MarshalIndent(idx, "", "  "); err == nil {
		if _, putErr := s.storage.Put(ctx, key, strings.NewReader(string(data)), int64(len(data))); putErr != nil {
			s.log.Warn("写空间索引失败", "space_id", spaceID, "err", putErr)
		}
	}
	return IndexBundle{Skills: idx.Skills, Tools: idx.Tools, MCPS: idx.MCPS, Components: idx.Components}
}

// MergedSources 返回「当前空间 + 系统空间」合并后的资源索引。
//
// 去重键是 Name：同名时当前空间优先（用户在自建空间里定制的版本覆盖平台默认）。
// 系统空间未初始化、或当前空间就是系统空间时自动降级，不依赖系统空间一定存在。
func (s *Service) MergedSources(ctx context.Context, spaceID string) IndexBundle {
	cur := s.LoadIndex(ctx, spaceID)
	out := IndexBundle{
		Skills:     marked(cur.Skills, SourceCurrent),
		Tools:      marked(cur.Tools, SourceCurrent),
		MCPS:       marked(cur.MCPS, SourceCurrent),
		Components: marked(cur.Components, SourceCurrent),
	}

	// 已安装插件先入池：未落副本的预置件（extensible=false）不在文件树里，
	// 但安装后必须能被检索到，否则「装了却用不了」。
	if s.installed != nil {
		items, err := s.installed(ctx, spaceID)
		if err != nil {
			s.log.Warn("读取已安装插件失败", "space_id", spaceID, "err", err)
		}
		var ins IndexBundle
		for _, e := range items {
			switch e.Kind {
			case "skills":
				ins.Skills = append(ins.Skills, e)
			case "mcps":
				ins.MCPS = append(ins.MCPS, e)
			case "components":
				ins.Components = append(ins.Components, e)
			default:
				ins.Tools = append(ins.Tools, e)
			}
		}
		// 已安装优先，草稿同名让路（装好的版本是运行态的唯一事实）
		out.Skills = mergeByName(marked(ins.Skills, SourceCurrent), out.Skills)
		out.Tools = mergeByName(marked(ins.Tools, SourceCurrent), out.Tools)
		out.MCPS = mergeByName(marked(ins.MCPS, SourceCurrent), out.MCPS)
		out.Components = mergeByName(marked(ins.Components, SourceCurrent), out.Components)
	}

	sys, err := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	if err != nil || sys.ID == spaceID {
		return s.withoutDisabled(ctx, spaceID, out)
	}
	sysIdx := s.LoadIndex(ctx, sys.ID)
	out.Skills = mergeByName(out.Skills, marked(sysIdx.Skills, SourceSystem))
	out.Tools = mergeByName(out.Tools, marked(sysIdx.Tools, SourceSystem))
	out.MCPS = mergeByName(out.MCPS, marked(sysIdx.MCPS, SourceSystem))
	out.Components = mergeByName(out.Components, marked(sysIdx.Components, SourceSystem))
	return s.withoutDisabled(ctx, spaceID, out)
}

// withoutDisabled 摘掉已停用插件的条目。
//
// 停用是运行期状态，磁盘上的 plugins/<slug>/ 还在，索引（无论是文件现状还是
// 已安装清单）照样会把它算进来。不在这里筛一遍，「停用」对靠索引进检索池的
// 技能/组件就等于没生效。
func (s *Service) withoutDisabled(ctx context.Context, spaceID string, b IndexBundle) IndexBundle {
	if s.disabledPlugins == nil {
		return b
	}
	off, err := s.disabledPlugins(ctx, spaceID)
	if err != nil {
		s.log.Warn("读取停用插件失败", "space_id", spaceID, "err", err)
		return b
	}
	if len(off) == 0 {
		return b
	}
	keep := func(list []IndexEntry) []IndexEntry {
		if len(list) == 0 {
			return list
		}
		out := make([]IndexEntry, 0, len(list))
		for _, e := range list {
			if off[slugOfSourcePath(e.Path)] {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	b.Skills = keep(b.Skills)
	b.Tools = keep(b.Tools)
	b.MCPS = keep(b.MCPS)
	b.Components = keep(b.Components)
	return b
}

// slugOfSourcePath 从索引路径反解插件 slug：
// 空间副本是 plugins/<slug>/...，发布件是 published/<kind>/<slug>/<version>/...
func slugOfSourcePath(rel string) string {
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) >= 2 && parts[0] == DraftDirName:
		return parts[1]
	case len(parts) >= 3 && parts[0] == "published":
		return parts[2]
	}
	return ""
}

// isSystemCapabilityPath 判断相对路径是否落在可跨空间读取的能力目录内。
func isSystemCapabilityPath(rel string) bool {
	for _, dir := range []string{DraftDirName} {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// SystemSpaceID 返回系统空间 ID；系统空间尚未初始化时返回 store.ErrNotFound。
//
// 平台配置（agents / config / net_whitelist）必须显式用这个 ID 读取，
// 不能走 ReadSourceFile 的回落逻辑，否则用户空间里的同名文件会顶替平台配置。
func (s *Service) SystemSpaceID(ctx context.Context) (string, error) {
	sp, err := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	if err != nil {
		return "", err
	}
	return sp.ID, nil
}

// Skills 返回当前空间 + 系统空间合并后的技能（同名当前空间优先），供会话技能选择器使用。
func (s *Service) Skills(ctx context.Context, userID, spaceID string) ([]IndexEntry, error) {
	if _, err := s.Get(ctx, userID, spaceID); err != nil {
		return nil, err
	}
	return s.MergedSources(ctx, spaceID).Skills, nil
}

// ReadSourceFile 读取资源文件：先当前空间，再系统空间。
//
// 这是 resource_read 与技能正文加载的统一入口，因此路径只做安全校验，
// 不限定在 skills / tools / mcps 目录下——普通空间的技能就在 files 根下。
func (s *Service) ReadSourceFile(ctx context.Context, spaceID, rawPath string, limit int64) (string, error) {
	rel, err := normalizeSourcePath(rawPath)
	if err != nil {
		return "", err
	}
	// 已发布件不在空间文件区：published/{kind}/{slug}/{version}/...
	if strings.HasPrefix(rel, "published/") {
		if s.published == nil {
			return "", ErrFileNotFound
		}
		return s.published(ctx, spaceID, rel)
	}

	text, err := s.readAt(ctx, spaceID, rel, limit)
	if err == nil {
		return text, nil
	}
	if !errors.Is(err, ErrFileNotFound) {
		return "", err
	}

	sys, sysErr := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	if sysErr != nil || sys.ID == spaceID {
		return "", ErrFileNotFound
	}
	// 只允许回落到能力目录。agents/ 是平台配置，
	// 不能经这条通道被普通空间读到（否则等于公开了白名单与接入点）。
	if !isSystemCapabilityPath(rel) {
		return "", ErrFileNotFound
	}
	return s.readAt(ctx, sys.ID, rel, limit)
}

// SkillDoc 读取技能的 SKILL.md 正文，返回正文与来源。
//
// entry.Path 已经指向 SKILL.md；找不到时依次尝试同名技能的另一个来源。
func (s *Service) SkillDoc(ctx context.Context, spaceID, rel string, limit int64) (string, error) {
	return s.ReadSourceFile(ctx, spaceID, rel, limit)
}

func (s *Service) readAt(ctx context.Context, spaceID, rel string, limit int64) (string, error) {
	f, err := s.storage.Get(ctx, path.Join(s.filesKey(spaceID), rel))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", ErrFileNotFound
		}
		return "", err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Service) buildIndex(ctx context.Context, spaceID string) spaceIndex {
	idx := spaceIndex{
		Version:    indexVersion,
		Skills:     []IndexEntry{},
		Tools:      []IndexEntry{},
		MCPS:       []IndexEntry{},
		Components: []IndexEntry{},
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	// 只扫 plugins/：目录里有什么就执行什么，不再有 tools/mcps 的只读投影。
	// 本地插件必须启用才进索引；安装副本由 plugin.InstalledSources 负责。
	enabled := map[string]bool{}
	if s.localPlugins != nil {
		if m, err := s.localPlugins(ctx, spaceID); err == nil {
			enabled = m
		} else {
			s.log.Warn("读取本地插件启用状态失败", "space_id", spaceID, "err", err)
		}
	}
	plugins := s.collectPlugins(ctx, spaceID)
	for _, e := range plugins {
		if s.localPlugins != nil {
			slug := strings.SplitN(strings.TrimPrefix(e.Path, DraftDirName+"/"), "/", 2)[0]
			if !enabled[slug] {
				continue
			}
		}
		switch e.Kind {
		case "skills":
			idx.Skills = append(idx.Skills, e)
		case "mcps":
			idx.MCPS = append(idx.MCPS, e)
		case "components":
			idx.Components = append(idx.Components, e)
		default:
			idx.Tools = append(idx.Tools, e)
		}
	}
	sortEntries(idx.Skills)
	sortEntries(idx.Tools)
	sortEntries(idx.MCPS)
	sortEntries(idx.Components)
	return idx
}

// refreshIndex 刷新索引；失败只告警，不影响已经完成的写操作。
func (s *Service) refreshIndex(ctx context.Context, spaceID string) {
	if err := s.RebuildIndex(ctx, spaceID); err != nil {
		s.log.Warn("刷新空间索引失败", "space_id", spaceID, "err", err)
	}
}

// normalizeSourcePath 校验资源文件相对路径：禁止越界、反斜杠与隐藏文件，限定可读类型。
func normalizeSourcePath(raw string) (string, error) {
	rel := strings.Trim(strings.TrimSpace(raw), "/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", ErrNameInvalid
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return "", ErrNameInvalid
		}
	}
	if !previewExts[strings.ToLower(path.Ext(rel))] {
		return "", ErrFileUnsupported
	}
	return rel, nil
}

func marked(entries []IndexEntry, source string) []IndexEntry {
	out := make([]IndexEntry, len(entries))
	for i, e := range entries {
		e.Source = source
		out[i] = e
	}
	return out
}

// mergeByName 合并两批资源：base 优先，同名跳过。
func mergeByName(base, extra []IndexEntry) []IndexEntry {
	seen := make(map[string]struct{}, len(base))
	for _, e := range base {
		seen[e.Name] = struct{}{}
	}
	for _, e := range extra {
		if _, ok := seen[e.Name]; ok {
			continue
		}
		seen[e.Name] = struct{}{}
		base = append(base, e)
	}
	sortEntries(base)
	return base
}

func sortEntries(entries []IndexEntry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// DraftDirName 是空间文件区下存放插件的目录。
const DraftDirName = "plugins"

// collectPlugins 扫描 plugins/ 下的插件清单，产出检索条目。
//
// 这里只解析展示需要的字段，不引 plugin 包：space 是被 plugin 依赖的一方，
// 反向导入会成环。校验与执行仍由 plugin.Manager 负责。
func (s *Service) collectPlugins(ctx context.Context, spaceID string) []IndexEntry {
	root := path.Join(s.filesKey(spaceID), DraftDirName)
	entries, err := s.storage.List(ctx, root)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("扫描插件索引失败", "space_id", spaceID, "err", err)
		}
		return nil
	}

	out := make([]IndexEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		slug := path.Base(e.Key)
		if strings.HasPrefix(slug, ".") {
			continue
		}
		raw, err := s.readAt(ctx, spaceID, path.Join(DraftDirName, slug, "plugin.json"), 8<<10)
		if err != nil {
			continue
		}
		var m struct {
			Kind        string `json:"kind"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Skill       struct {
				Entry string `json:"entry"`
			} `json:"skill"`
			Runtime struct {
				Entry string `json:"entry"`
			} `json:"runtime"`
			UI struct {
				Entry string `json:"entry"`
			} `json:"ui"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		entry := m.Skill.Entry
		switch m.Kind {
		case "tool":
			entry = m.Runtime.Entry
		case "component":
			entry = m.UI.Entry
		}
		if entry == "" {
			entry = "plugin.json"
		}
		out = append(out, IndexEntry{
			Kind:        pluralKind(m.Kind),
			Name:        m.Name,
			Description: m.Description,
			Path:        path.Join(DraftDirName, slug, entry),
		})
	}
	return out
}

func pluralKind(kind string) string {
	switch kind {
	case "skill":
		return "skills"
	case "mcp":
		return "mcps"
	case "component":
		return "components"
	}
	return "tools"
}
