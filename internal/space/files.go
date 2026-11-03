package space

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/storage"
)

const (
	// maxFileBytes 是单个文件的读取上限：树里的文件是说明文档与配置，不承载大文件。
	maxFileBytes = 256 << 10
	// maxFilesPerSpace 是单个空间最多展示的文件数，避免树被海量文件拖垮。
	maxFilesPerSpace = 500
)

// previewExts 是可在树中展示并在右侧预览的文件类型。
// 系统空间内置的是技能说明（md）与工具/MCP 定义（json），其余类型不进树。
var previewExts = map[string]bool{
	".md":   true,
	".json": true,
	".txt":  true,
	".yaml": true,
	".yml":  true,
}

// File 是空间目录下的一个可预览文件。
type File struct {
	SpaceID string
	// Path 是相对空间 files/ 的路径，如 skills/空间管理/SKILL.md
	Path string
	// Dir 是所属目录路径，空表示直接位于空间根
	Dir     string
	Name    string
	Size    int64
	ModTime time.Time
}

// FileContent 是文件正文；超出上限时截断。
type FileContent struct {
	File
	Content   string
	Truncated bool
}

// ListFiles 列出用户可见空间下所有可预览文件，一次取全供左侧树使用。
func (s *Service) ListFiles(ctx context.Context, userID string) ([]File, error) {
	spaces, err := s.store.ListSpaces(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]File, 0, 16)
	for _, sp := range spaces {
		out = append(out, s.walkFiles(ctx, sp.ID, "", 1)...)
	}
	return out, nil
}

// walkFiles 深度优先收集文件：目录递归下钻，只收白名单扩展名。
func (s *Service) walkFiles(ctx context.Context, spaceID, parent string, depth int) []File {
	if depth > maxDirDepth {
		return nil
	}

	key := s.filesKey(spaceID)
	if parent != "" {
		key = path.Join(key, parent)
	}
	entries, err := s.storage.List(ctx, key)
	if err != nil {
		// files/ 还没建或被清理时不阻塞树
		if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("读取空间文件失败", "space_id", spaceID, "dir", parent, "err", err)
		}
		return nil
	}

	out := make([]File, 0, len(entries))
	for _, e := range entries {
		name := path.Base(e.Key)
		// 隐藏文件（如 skill 的 .meta.json）属于后端元数据，不进树也不可预览
		if strings.HasPrefix(name, ".") {
			continue
		}
		rel := name
		if parent != "" {
			rel = parent + "/" + name
		}
		if e.IsDir {
			out = append(out, s.walkFiles(ctx, spaceID, rel, depth+1)...)
			continue
		}
		if !previewExts[strings.ToLower(path.Ext(name))] {
			continue
		}
		if len(out) >= maxFilesPerSpace {
			break
		}
		out = append(out, File{
			SpaceID: spaceID,
			Path:    rel,
			Dir:     parent,
			Name:    name,
			Size:    e.Size,
			ModTime: e.ModTime,
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ReadFile 读取文件内容。路径必须落在该空间的 files/ 下且属于可预览类型。
func (s *Service) ReadFile(ctx context.Context, userID, spaceID, rawPath string) (FileContent, error) {
	// 走 Get 而不是直接查表：同时完成存在性与成员权限校验
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return FileContent{}, err
	}

	rel, err := normalizeFilePath(rawPath)
	if err != nil {
		return FileContent{}, err
	}

	key := path.Join(s.filesKey(sp.ID), rel)
	entry, err := s.storage.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return FileContent{}, ErrFileNotFound
		}
		return FileContent{}, err
	}
	if entry.IsDir {
		return FileContent{}, ErrFileNotFound
	}

	f, err := s.storage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return FileContent{}, ErrFileNotFound
		}
		return FileContent{}, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return FileContent{}, err
	}
	truncated := len(data) > maxFileBytes
	if truncated {
		data = data[:maxFileBytes]
	}

	return FileContent{
		File: File{
			SpaceID: sp.ID,
			Path:    rel,
			Dir:     path.Dir(rel),
			Name:    path.Base(rel),
			Size:    entry.Size,
			ModTime: entry.ModTime,
		},
		Content:   string(data),
		Truncated: truncated,
	}, nil
}

// normalizeFilePath 校验相对 files/ 的文件路径：禁止越界、隐藏文件与不支持的扩展名。
func normalizeFilePath(raw string) (string, error) {
	rel := strings.Trim(strings.TrimSpace(raw), "/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", ErrNameInvalid
	}

	segs := strings.Split(rel, "/")
	if len(segs) > maxDirDepth+1 {
		return "", ErrDirTooDeep
	}
	for _, seg := range segs {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return "", ErrNameInvalid
		}
	}

	if !previewExts[strings.ToLower(path.Ext(rel))] {
		return "", ErrFileUnsupported
	}
	return rel, nil
}
