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

// DefaultUploadMaxBytes 是单文件上传的默认上限（100MB）。
// 部署时可用 UPLOAD_MAX_FILE_MB 覆盖，见 Service.SetUploadMaxBytes。
const DefaultUploadMaxBytes int64 = 100 << 20

const (
	// maxFileBytes 是单个文件的读取上限：树里的文件是说明文档与配置，不承载大文件。
	maxFileBytes = 256 << 10
	// maxFilesPerSpace 是单个空间最多展示的文件数，避免树被海量文件拖垮。
	maxFilesPerSpace = 500
	// maxUploadFiles 是一次请求允许上传的文件数上限。
	maxUploadFiles = 10
)

// PreviewKind 是文件在右侧面板的渲染方式。
//
// 文本类型走 `GET /spaces/{id}/file` 取正文（Agent 的 file_read 也走这条）；
// 浏览器能原生渲染的媒体与 PDF 走 `GET /spaces/{id}/file/raw` 取原始字节；
// 其余（压缩包、可执行产物等）只能下载到本机。
type PreviewKind string

const (
	PreviewKindNone  PreviewKind = "none"
	PreviewKindText  PreviewKind = "text"
	PreviewKindImage PreviewKind = "image"
	PreviewKindVideo PreviewKind = "video"
	PreviewKindAudio PreviewKind = "audio"
	PreviewKindPDF   PreviewKind = "pdf"
)

// binaryExts 是明确不是文本的类型：压缩包、可执行产物、字体、办公文档、数据库文件。
//
// 判定走排除法——不在 binaryExts 与 mediaPreviewExts 里的扩展名一律按文本处理，
// 所以 .ts / .py / .sh / Makefile / Dockerfile 这类代码与配置文件天然可预览，
// 新增语言不需要改这里的名单。名单只负责「一眼看出是二进制」的类型，
// 没有扩展名或扩展名不认识的文件由 ReadFile 的 looksBinary 兜底判定。
var binaryExts = map[string]bool{
	// 压缩包
	".zip": true, ".tar": true, ".gz": true, ".tgz": true, ".bz2": true, ".tbz2": true,
	".xz": true, ".txz": true, ".zst": true, ".7z": true, ".rar": true, ".jar": true,
	".war": true, ".ear": true, ".apk": true, ".ipa": true, ".dmg": true, ".iso": true,
	// 可执行产物与字节码
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".o": true,
	".obj": true, ".a": true, ".lib": true, ".class": true, ".pyc": true, ".pyo": true,
	".wasm": true, ".node": true, ".msi": true, ".deb": true, ".rpm": true, ".pkg": true,
	// 字体
	".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".eot": true,
	// 办公文档（二进制容器，正文要交给本地应用）
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true, ".rtf": true, ".pages": true, ".key": true,
	".numbers": true,
	// 数据库与设计稿
	".sqlite": true, ".sqlite3": true, ".db": true, ".mdb": true, ".psd": true,
	".ai": true, ".sketch": true, ".fig": true, ".xd": true,
	// 音视频容器里 raw 端点不处理的补充格式
	".flv": true, ".wmv": true, ".avi": true, ".mov": true, ".mkv": true, ".m4b": true,
}

// mediaPreviewExts 是浏览器能原生渲染的类型，值为预览类别与 MIME。
//
// MIME 必须是固定白名单，不能用 mime.TypeByExtension：后者依赖系统
// /etc/mime.types，不同机器上对 .webp / .avif 这类新格式可能返回空串。
// 这里只收「不会执行脚本」的容器：HTML / XML / JS 不在白名单里——raw 端点与平台
// 同源，返回可执行文档等于把登录态交给文件内容。它们走文本预览按源码展示
// （转义后渲染，不执行），只有「按文档内联返回」这条路被堵死。SVG 允许
// （<img> 里不执行脚本），直接打开时由响应头 CSP sandbox 阻断脚本。
var mediaPreviewExts = map[string]struct {
	kind PreviewKind
	mime string
}{
	".png":  {PreviewKindImage, "image/png"},
	".jpg":  {PreviewKindImage, "image/jpeg"},
	".jpeg": {PreviewKindImage, "image/jpeg"},
	".gif":  {PreviewKindImage, "image/gif"},
	".webp": {PreviewKindImage, "image/webp"},
	".avif": {PreviewKindImage, "image/avif"},
	".bmp":  {PreviewKindImage, "image/bmp"},
	".ico":  {PreviewKindImage, "image/x-icon"},
	".svg":  {PreviewKindImage, "image/svg+xml"},

	".mp4":  {PreviewKindVideo, "video/mp4"},
	".m4v":  {PreviewKindVideo, "video/mp4"},
	".webm": {PreviewKindVideo, "video/webm"},
	".ogv":  {PreviewKindVideo, "video/ogg"},

	".mp3":  {PreviewKindAudio, "audio/mpeg"},
	".wav":  {PreviewKindAudio, "audio/wav"},
	".ogg":  {PreviewKindAudio, "audio/ogg"},
	".oga":  {PreviewKindAudio, "audio/ogg"},
	".m4a":  {PreviewKindAudio, "audio/mp4"},
	".aac":  {PreviewKindAudio, "audio/aac"},
	".flac": {PreviewKindAudio, "audio/flac"},
	".weba": {PreviewKindAudio, "audio/webm"},

	".pdf": {PreviewKindPDF, "application/pdf"},
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
	// Previewable 表示能否读取正文（文本类型）：file_read 与引用候选都认这个开关。
	// 预览面板的实际渲染方式看 PreviewKind——图片/音视频/PDF 读不了正文，但能展示。
	Previewable bool
	// PreviewKind 是面板渲染方式：text / image / video / audio / pdf / none
	PreviewKind PreviewKind
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
		out = append(out, newFile(spaceID, rel, e))
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

	// 扩展名不认识的文件在清单里先按文本候选展示（这样新语言不用改名单），
	// 真读进来才发现是二进制：如实报错，让面板退回下载，而不是把乱码塞进正文
	// 或交给模型当成文本。
	if looksBinary(data) {
		return FileContent{}, ErrFileNotText
	}

	return FileContent{
		File:      newFile(sp.ID, rel, entry),
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

	return newFile(sp.ID, rel, entry), rc, nil
}

// PreviewFile 打开浏览器能原生渲染的文件（图片 / 音视频 / PDF），同时返回 MIME。
//
// 只认 mediaPreviewExts 白名单：raw 端点与平台同源，返回可执行文档等于把登录态交给
// 文件内容；HTML / XML / JS 因此一律 400，SVG 允许但响应头要带 CSP sandbox。
func (s *Service) PreviewFile(ctx context.Context, userID, spaceID, rawPath string) (File, string, io.ReadCloser, error) {
	// 与 ReadFile 一致：走 Get 完成存在性与成员权限校验
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return File{}, "", nil, err
	}

	rel, err := normalizeAnyPath(rawPath)
	if err != nil {
		return File{}, "", nil, err
	}
	mt, ok := mediaPreviewExts[strings.ToLower(path.Ext(rel))]
	if !ok {
		return File{}, "", nil, ErrFileUnsupported
	}

	key := path.Join(s.filesKey(sp.ID), rel)
	entry, err := s.storage.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return File{}, "", nil, ErrFileNotFound
		}
		return File{}, "", nil, err
	}
	if entry.IsDir {
		return File{}, "", nil, ErrFileNotFound
	}

	rc, err := s.storage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return File{}, "", nil, ErrFileNotFound
		}
		return File{}, "", nil, err
	}

	return newFile(sp.ID, rel, entry), mt.mime, rc, nil
}

// fileTooLarge 统一超限错误：文案里的上限跟着配置走，接口报错与前端提示才不会说错数字。
func fileTooLarge(limit int64) error {
	return fmt.Errorf("%w（单个文件 ≤ %dMB）", ErrFileTooLarge, limit>>20)
}

// SaveFile 把上传内容写入空间的 files/ 目录，返回落盘后的文件元信息。
//
// 约束只有两条：
//   - 接受任意文件类型：面板按 preview_kind 决定内联预览还是给下载入口；单文件大小由
//     UPLOAD_MAX_FILE_MB 控制（默认 100MB）。二进制不会被 file_read 读到、也进不了引用候选，
//     这条边界在读取侧（previewable）而不是上传侧——用户要存附件、留存截图都得能传上来；
//   - 同名文件不覆盖：自动追加 -1、-2 后缀，避免上传把已有材料冲掉。
//
// size 是调用方已知的字节数（multipart 解析后的真实长度），未知传 -1：已知时先挡超限，
// 未知时只能靠写入过程中的计数兜底。内容一律流式写入存储——100MB 的上限意味着
// io.ReadAll 会把整段塞进内存，几个并发上传就能把进程撑爆。
//
// 目录不设限（含 plugins/）：用户可以在空间里任何目录放材料。落在 plugins/ 下的
// 文件属于插件内容，写完要刷新能力索引并让插件运行时重载，否则工具列表还是旧的。
func (s *Service) SaveFile(ctx context.Context, userID, spaceID, rawDir, rawName string, r io.Reader, size int64) (File, error) {
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

	limit := s.UploadMaxBytes()
	if size > limit {
		return File{}, fileTooLarge(limit)
	}

	base := s.filesKey(sp.ID)
	rel, err := s.uniqueFilePath(ctx, base, dir, name)
	if err != nil {
		return File{}, err
	}
	key := path.Join(base, rel)

	// 多读 1 字节用来判「实际内容超过上限」：size 未知时这是唯一防线。
	lr := &io.LimitedReader{R: r, N: limit + 1}
	if _, err := s.storage.Put(ctx, key, lr, size); err != nil {
		return File{}, err
	}
	if lr.N <= 0 {
		// 超限内容已经被写进去了：删掉，不能让空间里留下一个「传了一半算成功」的文件
		if err := s.storage.Delete(ctx, key); err != nil {
			s.log.Warn("清理超限文件失败", "space_id", sp.ID, "path", rel, "err", err)
		}
		return File{}, fileTooLarge(limit)
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

	return newFile(sp.ID, rel, entry), nil
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

// newFile 由存储条目构造文件元信息。
// Previewable 与 PreviewKind 必须同时给出，否则前端会拿到「不可预览但能渲染」
// 或「能渲染却不可预览」的矛盾状态。Dir 对根目录文件返回空串而不是 "."。
func newFile(spaceID, rel string, entry storage.Entry) File {
	return File{
		SpaceID:     spaceID,
		Path:        rel,
		Dir:         dirOf(rel),
		Name:        path.Base(rel),
		Size:        entry.Size,
		ModTime:     entry.ModTime,
		Previewable: Readable(rel),
		PreviewKind: previewKind(rel),
	}
}

// dirOf 返回文件所在目录；根目录文件返回空串（path.Dir 会给 "."）。
func dirOf(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

// Readable 报告文件能否读取正文（file_read 与引用清单的「可读」判定都认它）：
// 除媒体与已知二进制之外的一切文件——文本、代码、配置都没有固定白名单，
// 用户自己的隐藏文件（.env / .gitignore 这类没有常规后缀的配置）也按文本处理。
// 它不限制上传与引用：二进制能传、能作为附件引用、能预览下载，只是读不出正文。
func Readable(name string) bool {
	base := path.Base(name)
	if spacefs.UserHidden(base) {
		return true
	}
	ext := strings.ToLower(path.Ext(base))
	if _, ok := mediaPreviewExts[ext]; ok {
		return false
	}
	return !binaryExts[ext]
}

// previewKind 判定文件的渲染方式：媒体与 PDF 优先（raw 端点出原始字节），
// 已知二进制只能下载，其余一律按文本预览。
func previewKind(name string) PreviewKind {
	ext := strings.ToLower(path.Ext(name))
	if mt, ok := mediaPreviewExts[ext]; ok {
		return mt.kind
	}
	if binaryExts[ext] {
		return PreviewKindNone
	}
	return PreviewKindText
}

// maxSniffBytes 是二进制判定的取样窗口，与 net/http 的 DetectContentType 同口径。
const maxSniffBytes = 8000

// looksBinary 用 NUL 字节判定内容是不是二进制。
//
// 只看 NUL 不看 UTF-8 合法性：GBK 这类非 UTF-8 编码的文本仍然是文本，
// 按非法字节拒绝会把企业里常见的旧编码文档误判成二进制，反而不给预览。
func looksBinary(data []byte) bool {
	if len(data) > maxSniffBytes {
		data = data[:maxSniffBytes]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// normalizeFilePath 校验相对 files/ 的文件路径：禁止越界、平台保留项与不可预览的扩展名。
// 只有在「读正文」这条路上才限定扩展名——右侧预览与 file_read 都只吃文本。
func normalizeFilePath(raw string) (string, error) {
	rel, err := normalizeAnyPath(raw)
	if err != nil {
		return "", err
	}
	if !Readable(rel) {
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
