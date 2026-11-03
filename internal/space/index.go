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

const indexVersion = 1

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
	Skills []IndexEntry
	Tools  []IndexEntry
	MCPS   []IndexEntry
}

// spaceIndex 是 files/.meta.json 的文件结构。
type spaceIndex struct {
	Version   int          `json:"version"`
	UpdatedAt string       `json:"updated_at"`
	Skills    []IndexEntry `json:"skills"`
	Tools     []IndexEntry `json:"tools"`
	MCPS      []IndexEntry `json:"mcps"`
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
			return IndexBundle{Skills: idx.Skills, Tools: idx.Tools, MCPS: idx.MCPS}
		}
	}

	idx := s.buildIndex(ctx, spaceID)
	if data, err := json.MarshalIndent(idx, "", "  "); err == nil {
		if _, putErr := s.storage.Put(ctx, key, strings.NewReader(string(data)), int64(len(data))); putErr != nil {
			s.log.Warn("写空间索引失败", "space_id", spaceID, "err", putErr)
		}
	}
	return IndexBundle{Skills: idx.Skills, Tools: idx.Tools, MCPS: idx.MCPS}
}

// MergedSources 返回「当前空间 + 系统空间」合并后的资源索引。
//
// 去重键是 Name：同名时当前空间优先（用户在自建空间里定制的版本覆盖平台默认）。
// 系统空间未初始化、或当前空间就是系统空间时自动降级，不依赖系统空间一定存在。
func (s *Service) MergedSources(ctx context.Context, spaceID string) IndexBundle {
	cur := s.LoadIndex(ctx, spaceID)
	out := IndexBundle{
		Skills: marked(cur.Skills, SourceCurrent),
		Tools:  marked(cur.Tools, SourceCurrent),
		MCPS:   marked(cur.MCPS, SourceCurrent),
	}

	sys, err := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	if err != nil || sys.ID == spaceID {
		return out
	}
	sysIdx := s.LoadIndex(ctx, sys.ID)
	out.Skills = mergeByName(out.Skills, marked(sysIdx.Skills, SourceSystem))
	out.Tools = mergeByName(out.Tools, marked(sysIdx.Tools, SourceSystem))
	out.MCPS = mergeByName(out.MCPS, marked(sysIdx.MCPS, SourceSystem))
	return out
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
// 这是 global_source_read 与技能正文加载的统一入口，因此路径只做安全校验，
// 不限定在 skills / tools / mcps 目录下——普通空间的技能就在 files 根下。
func (s *Service) ReadSourceFile(ctx context.Context, spaceID, rawPath string, limit int64) (string, error) {
	rel, err := normalizeSourcePath(rawPath)
	if err != nil {
		return "", err
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
		Version:   indexVersion,
		Skills:    []IndexEntry{},
		Tools:     []IndexEntry{},
		MCPS:      []IndexEntry{},
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.collectSkills(ctx, spaceID, "", 1, &idx.Skills)
	idx.Tools = s.collectManifests(ctx, spaceID, systemToolsDir)
	idx.MCPS = s.collectManifests(ctx, spaceID, systemMCPsDir)
	sortEntries(idx.Skills)
	sortEntries(idx.Tools)
	sortEntries(idx.MCPS)
	return idx
}

// collectSkills 递归找出带 .meta.json（type=skill）的目录。
//
// 覆盖两种布局：系统空间的 files/skills/<name>/，以及普通空间根下的 files/<name>/。
func (s *Service) collectSkills(ctx context.Context, spaceID, parent string, depth int, out *[]IndexEntry) {
	if depth > maxDirDepth {
		return
	}
	key := s.filesKey(spaceID)
	if parent != "" {
		key = path.Join(key, parent)
	}
	entries, err := s.storage.List(ctx, key)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("扫描技能索引失败", "space_id", spaceID, "dir", parent, "err", err)
		}
		return
	}

	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		name := path.Base(e.Key)
		if strings.HasPrefix(name, ".") {
			continue
		}
		rel := name
		if parent != "" {
			rel = parent + "/" + name
		}
		if meta, ok := s.readSkillMeta(ctx, spaceID, rel); ok {
			display := meta.Name
			if display == "" {
				display = name
			}
			*out = append(*out, IndexEntry{
				Kind:        "skills",
				Name:        display,
				Description: meta.Description,
				Path:        path.Join(rel, "SKILL.md"),
			})
		}
		s.collectSkills(ctx, spaceID, rel, depth+1, out)
	}
}

// collectManifests 收集 tools/mcps 目录下的一层 JSON 清单。
func (s *Service) collectManifests(ctx context.Context, spaceID, dir string) []IndexEntry {
	key := path.Join(s.filesKey(spaceID), dir)
	entries, err := s.storage.List(ctx, key)
	if err != nil {
		return []IndexEntry{}
	}

	out := make([]IndexEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		name := path.Base(e.Key)
		if strings.HasPrefix(name, ".") || !strings.EqualFold(path.Ext(name), ".json") {
			continue
		}
		entry := IndexEntry{Name: strings.TrimSuffix(name, ".json"), Path: path.Join(dir, name)}
		if f, err := s.storage.Get(ctx, e.Key); err == nil {
			var meta struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			_ = json.NewDecoder(io.LimitReader(f, 32<<10)).Decode(&meta)
			_ = f.Close()
			if meta.Name != "" {
				entry.Name = meta.Name
			}
			entry.Description = meta.Description
		}
		out = append(out, entry)
	}
	return out
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
