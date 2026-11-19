package space

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// DefaultUploadMaxBytes 是单文件上传的默认上限（4MB）。
// 部署时可用 UPLOAD_MAX_FILE_MB 覆盖，见 Service.SetUploadMaxBytes。
const DefaultUploadMaxBytes int64 = 4 << 20

const (
	// maxFileBytes 是单个文件的读取上限：树里的文件是说明文档与配置，不承载大文件。
	maxFileBytes = 256 << 10
	// maxFilesPerSpace 是单个空间最多展示的文件数，避免树被海量文件拖垮。
	maxFilesPerSpace = 500
	// maxUploadFiles 是一次请求允许上传的文件数上限。
	maxUploadFiles = 10
)

// previewExts 是右侧面板能渲染、并且 Agent 能通过 file_read 读到正文的文件类型。
// 用户自己的隐藏文件（.env / .gitignore 等）没有常规后缀，也按文本处理（见 previewable）。
// 树里其余类型（图片、压缩包、脚本产物等）照常展示，但只提供「下载到本机」；
// 上传仍只接受文本类型：预览不了的文件收下来对模型没用。
var previewExts = map[string]bool{
	".md":   true,
	".json": true,
	".txt":  true,
	".csv":  true,
	".yaml": true,
	".yml":  true,
}

// File 是空间目录下的一个文件。
type File struct {
	SpaceID string
	// Path 是相对空间 files/ 的路径，如 skills/空间管理/SKILL.md
	Path string
	// Dir 是所属目录路径，空表示直接位于空间根
	Dir     string
	Name    string
	Size    int64
	ModTime time.Time
	// Previewable 表示能否在右侧面板渲染：白名单内的文本类型走预览，
	// 其余（图片、压缩包、可执行产物等）只能下载到本机
	Previewable bool
}

// FileContent 是文件正文；超出上限时截断。
type FileContent struct {
	File
	Content   string
	Truncated bool
}

// ListFiles 列出用户可见空间下的全部文件（平台保留的隐藏项除外），一次取全供左侧树使用。
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

// walkFiles 深度优先收集文件：目录递归下钻，平台保留的隐藏项不进树，
// 其余全部收集——用户自己的隐藏文件（.env 等）与预览不了的类型都留在树里，
// 后者前端据此给下载入口。
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
		// 平台保留的隐藏项（.meta.json / .install.json / .history）属于后端元数据，
		// 不进树也不可预览；用户自己的隐藏文件（.env 等）照常展示。
		if spacefs.Reserved(name) {
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
		if len(out) >= maxFilesPerSpace {
			break
		}
		out = append(out, File{
			SpaceID:     spaceID,
			Path:        rel,
			Dir:         parent,
			Name:        name,
			Size:        e.Size,
			ModTime:     e.ModTime,
			Previewable: previewable(name),
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
			SpaceID:     sp.ID,
			Path:        rel,
			Dir:         path.Dir(rel),
			Name:        path.Base(rel),
			Size:        entry.Size,
			ModTime:     entry.ModTime,
			Previewable: true,
		},
		Content:   string(data),
		Truncated: truncated,
	}, nil
}

// DownloadFile 打开文件原始字节，供「下载到本机」使用。
//
// 与 ReadFile 的两处差别：不看扩展名（图片、压缩包、脚本产物都要能取回本机），
// 也不按 256KB 截断。路径校验仍是同一套：越界、反斜杠、平台保留项、目录层级。
func (s *Service) DownloadFile(ctx context.Context, userID, spaceID, rawPath string) (File, io.ReadCloser, error) {
	// 与 ReadFile 一致：走 Get 完成存在性与成员权限校验
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return File{}, nil, err
	}

	rel, err := normalizeAnyPath(rawPath)
	if err != nil {
		return File{}, nil, err
	}

	key := path.Join(s.filesKey(sp.ID), rel)
	entry, err := s.storage.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return File{}, nil, ErrFileNotFound
		}
		return File{}, nil, err
	}
	if entry.IsDir {
		return File{}, nil, ErrFileNotFound
	}

	rc, err := s.storage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return File{}, nil, ErrFileNotFound
		}
		return File{}, nil, err
	}

	return File{
		SpaceID:     sp.ID,
		Path:        rel,
		Dir:         path.Dir(rel),
		Name:        path.Base(rel),
		Size:        entry.Size,
		ModTime:     entry.ModTime,
		Previewable: previewable(rel),
	}, rc, nil
}

// SaveFile 把上传内容写入空间的 files/ 目录，返回落盘后的文件元信息。
//
// 约束只有两条：
//   - 只接受可预览的文本类型（previewable）：二进制文件既预览不了，file_read 也读不到；
//   - 同名文件不覆盖：自动追加 -1、-2 后缀，避免上传把已有材料冲掉。
//
// 目录不设限（含 plugins/）：用户可以在空间里任何目录放材料。落在 plugins/ 下的
// 文件属于插件内容，写完要刷新能力索引并让插件运行时重载，否则工具列表还是旧的。
func (s *Service) SaveFile(ctx context.Context, userID, spaceID, rawDir, rawName string, r io.Reader) (File, error) {
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return File{}, err
	}
	if !canWrite(sp.Perm) {
		return File{}, ErrForbidden
	}

	dir, err := normalizeRelPath(rawDir)
	if err != nil {
		return File{}, err
	}

	name, err := normalizeName(path.Base(strings.ReplaceAll(rawName, "\\", "/")))
	if err != nil {
		return File{}, err
	}
	if !previewable(name) {
		return File{}, ErrFileUnsupported
	}

	limit := s.UploadMaxBytes()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return File{}, err
	}
	if int64(len(data)) > limit {
		return File{}, fmt.Errorf("%w（单个文件 ≤ %dMB）", ErrFileTooLarge, limit>>20)
	}

	base := s.filesKey(sp.ID)
	rel, err := s.uniqueFilePath(ctx, base, dir, name)
	if err != nil {
		return File{}, err
	}
	key := path.Join(base, rel)
	if _, err := s.storage.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
		return File{}, err
	}

	entry, err := s.storage.Stat(ctx, key)
	if err != nil {
		return File{}, err
	}

	// 插件目录里的文件就是插件内容：索引与运行时缓存都要跟着变
	if rel == DraftDirName || strings.HasPrefix(rel, DraftDirName+"/") {
		if s.pluginChanged != nil {
			if err := s.pluginChanged(ctx, sp.ID); err != nil {
				s.log.Warn("重载插件失败", "space_id", sp.ID, "path", rel, "err", err)
			}
		}
		s.refreshIndex(ctx, sp.ID)
	}

	return File{
		SpaceID:     sp.ID,
		Path:        rel,
		Dir:         dir,
		Name:        path.Base(rel),
		Size:        entry.Size,
		ModTime:     entry.ModTime,
		Previewable: true,
	}, nil
}

// DeleteFile 删除单个文件；目录走 DeleteDir。
//
// 只读边界与 DeleteDir 一致：系统空间内置目录整棵只读，已安装插件的副本也只能卸载，
// 允许逐文件删会让安装记录、发布快照与磁盘内容对不上（删掉 plugin.json 就是半个插件）。
// 本地草稿（plugins/<slug>/，没有安装记录）可以删，删完要重载插件层与索引，
// 否则运行时还在按旧清单提供工具。
func (s *Service) DeleteFile(ctx context.Context, userID, spaceID, rawPath string) error {
	rel, err := normalizeRelPath(rawPath)
	if err != nil || rel == "" {
		return ErrFileNotFound
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}
	// 系统空间的内置目录是平台资产，里面的文件同样不允许单独删
	if s.inBuiltinDir(sp, rel) {
		return ErrReserved
	}
	if s.isInstalledSubtree(ctx, sp.ID, rel) {
		return ErrPluginReadOnly
	}

	key := path.Join(s.filesKey(sp.ID), rel)
	entry, err := s.storage.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrFileNotFound
		}
		return err
	}
	if entry.IsDir {
		return errors.New("目标是目录，不能按文件删除")
	}
	if err := s.storage.Delete(ctx, key); err != nil {
		return err
	}

	// 插件区里的文件就是插件内容：索引与运行时缓存都要跟着变
	if rel == DraftDirName || strings.HasPrefix(rel, DraftDirName+"/") {
		// 删掉清单等于插件失效：候选记录留着会在市场里挂一条打不开的「待发布」
		if path.Base(rel) == "plugin.json" && path.Dir(path.Dir(rel)) == DraftDirName {
			slug := path.Base(path.Dir(rel))
			if err := s.store.DeletePluginCandidate(ctx, sp.ID, slug); err != nil {
				s.log.Warn("清理插件候选失败", "space_id", sp.ID, "slug", slug, "err", err)
			}
		}
		if s.pluginChanged != nil {
			if err := s.pluginChanged(ctx, sp.ID); err != nil {
				s.log.Warn("重载插件失败", "space_id", sp.ID, "path", rel, "err", err)
			}
		}
		s.refreshIndex(ctx, sp.ID)
	}
	s.log.Info("文件已删除", "space_id", sp.ID, "file", rel, "by", userID)
	return nil
}

// inBuiltinDir 判断 rel 是否落在系统空间的内置目录内（含目录自身）。
func (s *Service) inBuiltinDir(sp store.Space, rel string) bool {
	if sp.Type != SpaceTypeSystem {
		return false
	}
	return isSystemBuiltinDir(strings.SplitN(strings.Trim(rel, "/"), "/", 2)[0])
}

// uniqueFilePath 为上传文件找一个不冲突的相对路径：name.ext → name-1.ext → name-2.ext。
func (s *Service) uniqueFilePath(ctx context.Context, base, dir, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 100; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		rel := candidate
		if dir != "" {
			rel = dir + "/" + candidate
		}
		if _, err := s.storage.Stat(ctx, path.Join(base, rel)); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return rel, nil
			}
			return "", err
		}
	}
	return "", fmt.Errorf("%w：同名文件过多，请改名后重试", ErrNameInvalid)
}

// previewable 报告文件能否在右侧面板渲染（以及 file_read 能否读到）：
// 白名单内的文本类型，或用户自己的隐藏文件（.env / .gitignore 这类没有常规后缀的配置）。
func previewable(name string) bool {
	if spacefs.UserHidden(path.Base(name)) {
		return true
	}
	return previewExts[strings.ToLower(path.Ext(name))]
}

// normalizeFilePath 校验相对 files/ 的文件路径：禁止越界、平台保留项与不可预览的扩展名。
// 只有在「读正文」这条路上才限定扩展名——右侧预览与 file_read 都只吃文本。
func normalizeFilePath(raw string) (string, error) {
	rel, err := normalizeAnyPath(raw)
	if err != nil {
		return "", err
	}
	if !previewable(rel) {
		return "", ErrFileUnsupported
	}
	return rel, nil
}

// normalizeAnyPath 只校验路径本身，不看扩展名：
// 下载要能取到图片、压缩包这类预览不了的文件。
//
// 隐藏文件不在拒绝之列（用户可能就要看 .env）；只拦平台保留项。
func normalizeAnyPath(raw string) (string, error) {
	rel := strings.Trim(strings.TrimSpace(raw), "/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", ErrNameInvalid
	}

	segs := strings.Split(rel, "/")
	if len(segs) > maxDirDepth+1 {
		return "", ErrDirTooDeep
	}
	for _, seg := range segs {
		if seg == "." || seg == ".." || spacefs.Reserved(seg) {
			return "", ErrNameInvalid
		}
	}

	return rel, nil
}
