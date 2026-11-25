package space

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

// maxUploadInMemory 是 multipart 解析时单个文件留在内存的上限，超出部分自动落到临时文件。
// 它不决定「能传多大」（那是 UPLOAD_MAX_FILE_MB），只决定解析时占多少内存：
// 100MB 的文件按整段读进内存，几个并发上传就能把进程撑爆。
const maxUploadInMemory = 16 << 20

// UserResolver 从请求中解析当前登录用户。
type UserResolver func(r *http.Request) (store.User, error)

// Handler 提供空间 HTTP 端点。
type Handler struct {
	svc     *Service
	resolve UserResolver
	log     *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(svc *Service, resolve UserResolver, log *slog.Logger) *Handler {
	return &Handler{svc: svc, resolve: resolve, log: log}
}

// Register 注册路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /spaces", h.list)
	mux.HandleFunc("POST /spaces", h.create)
	mux.HandleFunc("GET /spaces/{id}", h.get)
	mux.HandleFunc("PATCH /spaces/{id}", h.rename)
	mux.HandleFunc("DELETE /spaces/{id}", h.remove)
	mux.HandleFunc("GET /spaces/{id}/tree", h.tree)

	// 目录是文件系统里的真实目录，与 space 表无关（MVP 方案 §4.2）；
	// 支持多级路径，因此读写都用 body/query 里的 path，而不是路径参数。
	mux.HandleFunc("GET /spaces/dirs", h.listDirs)
	// 文件树的结构版本号：前端定时拉它，变了才重新取 dirs / files。
	// 响应只有几十字节，因此可以比清单勤得多。
	mux.HandleFunc("GET /spaces/tree/rev", h.treeRev)
	mux.HandleFunc("POST /spaces/{id}/dirs", h.createDir)
	mux.HandleFunc("PATCH /spaces/{id}/dirs", h.renameDir)
	mux.HandleFunc("DELETE /spaces/{id}/dirs", h.deleteDir)

	// 兼容旧客户端的一级目录路由
	mux.HandleFunc("PATCH /spaces/{id}/dirs/{name}", h.renameDirLegacy)
	mux.HandleFunc("DELETE /spaces/{id}/dirs/{name}", h.deleteDirLegacy)

	// skill 本质是带 .meta.json 的特殊目录
	mux.HandleFunc("POST /spaces/{id}/skills", h.createSkill)
	// 插件草稿：kind 决定骨架（技能/工具/MCP/组件/智能体/搜索源）
	mux.HandleFunc("POST /spaces/{id}/drafts", h.createDraft)
	// 当前空间 + 系统空间合并后的技能列表，供会话技能选择器使用
	mux.HandleFunc("GET /spaces/{id}/skills", h.listSkills)

	// 树里的文件节点：/spaces/files 一次取全，/spaces/{id}/file 读正文。
	// 正文端点只读且只暴露 md/json 等文本类型；浏览器能原生渲染的媒体与 PDF 走
	// file/raw 取原始字节（内联 MIME + Range）；其余类型走 download 落盘。
	mux.HandleFunc("GET /spaces/files", h.listFiles)
	mux.HandleFunc("GET /spaces/{id}/file", h.readFile)
	mux.HandleFunc("GET /spaces/{id}/file/raw", h.previewFile)
	mux.HandleFunc("GET /spaces/{id}/file/download", h.downloadFile)
	// 上传不限类型，落在空间 files/ 下（dir 可选，相对 files/）；能否读正文由读取侧判定
	mux.HandleFunc("POST /spaces/{id}/files", h.uploadFiles)
	// 删除单个文件；目录走 /dirs，已安装插件与系统空间内置目录只读
	mux.HandleFunc("DELETE /spaces/{id}/file", h.deleteFile)
}

type spaceDTO struct {
	ID               string `json:"id"`
	ParentID         string `json:"parent_id,omitempty"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Path             string `json:"path"`
	Depth            int    `json:"depth"`
	Perm             string `json:"perm"`
	IsSystemReserved bool   `json:"is_system_reserved"`
	Locked           bool   `json:"locked"`
	CreatedAt        string `json:"created_at"`
}

func toDTO(s store.Space) spaceDTO {
	return spaceDTO{
		ID:               s.ID,
		ParentID:         s.ParentID,
		Name:             s.Name,
		Type:             s.Type,
		Path:             s.Path,
		Depth:            s.Depth,
		Perm:             s.Perm,
		IsSystemReserved: s.IsSystemReserved,
		Locked:           isLocked(s),
		CreatedAt:        s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func toDTOs(list []store.Space) []spaceDTO {
	out := make([]spaceDTO, 0, len(list))
	for _, s := range list {
		out = append(out, toDTO(s))
	}
	return out
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"spaces": toDTOs(BuildTree(list))})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	// 空间只有一级：parent_id 字段已废弃，传了也不生效
	var req struct {
		Name string `json:"name"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sp, err := h.svc.Create(r.Context(), user.ID, CreateInput{Name: req.Name})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"space": toDTO(sp)})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	sp, err := h.svc.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"space": toDTO(sp)})
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sp, err := h.svc.Rename(r.Context(), user.ID, r.PathValue("id"), req.Name)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"space": toDTO(sp)})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	if r.ContentLength != 0 {
		if err := platform.DecodeJSON(r, &req); err != nil {
			platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
	}

	if err := h.svc.Delete(r.Context(), user.ID, r.PathValue("id"), req.ConfirmName); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// tree 仅返回该空间自身：空间不再有子空间，保留端点是为了兼容旧前端。
func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	root, err := h.svc.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"spaces": toDTOs([]store.Space{root})})
}

type dirDTO struct {
	SpaceID string `json:"space_id"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	IsSkill bool   `json:"is_skill"`
	// PluginKind 非空表示该目录是一个插件草稿，值即插件类型。
	PluginKind string `json:"plugin_kind,omitempty"`
	// Builtin 表示系统空间的内置目录（skills / tools / mcps），不可改名删除
	Builtin     bool   `json:"builtin"`
	Installed   bool   `json:"installed,omitempty"`
	Local       bool   `json:"local,omitempty"`
	Mutable     bool   `json:"mutable,omitempty"`
	Enabled     bool   `json:"enabled,omitempty"`
	Description string `json:"description,omitempty"`
}

func toDirDTO(d Dir) dirDTO {
	return dirDTO{
		SpaceID:     d.SpaceID,
		Path:        d.Path,
		Name:        d.Name,
		IsSkill:     d.IsSkill,
		PluginKind:  d.PluginKind,
		Builtin:     d.Builtin,
		Installed:   d.Installed,
		Local:       d.Local,
		Mutable:     d.Mutable,
		Enabled:     d.Enabled,
		Description: d.Description,
	}
}

// skillDTO 是技能选择器里的一条：source 为 current（当前空间）或 system（系统空间）。
type skillDTO struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	Source      string `json:"source"`
}

func (h *Handler) listSkills(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	entries, err := h.svc.Skills(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]skillDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, skillDTO{
			Name:        e.Name,
			Description: e.Description,
			Path:        e.Path,
			Source:      e.Source,
		})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// treeRev 返回整棵树的结构版本号。
//
// 左树要靠它轮询：树的内容是磁盘现状，没有事件可订阅（其他会话里的 Agent、
// 另一个标签页都在改同一批文件）。版本号变了前端才重新取清单。
func (h *Handler) treeRev(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	rev, err := h.svc.TreeRev(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	// 版本号只用于比较，不该被任何一层缓存下来，否则轮询永远读到旧值
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]string{"rev": rev})
}

func (h *Handler) listDirs(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	dirs, err := h.svc.ListDirs(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]dirDTO, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, toDirDTO(d))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"dirs": out})
}

func (h *Handler) createDir(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
		// Parent 是父目录路径，为空表示建在空间根
		Parent string `json:"parent"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	dir, err := h.svc.CreateDir(r.Context(), user.ID, r.PathValue("id"), req.Parent, req.Name)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"dir": toDirDTO(dir)})
}

func (h *Handler) renameDir(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	dir, err := h.svc.RenameDir(r.Context(), user.ID, r.PathValue("id"), req.Path, req.Name)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"dir": toDirDTO(dir)})
}

func (h *Handler) renameDirLegacy(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	dir, err := h.svc.RenameDir(r.Context(), user.ID, r.PathValue("id"), r.PathValue("name"), req.Name)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"dir": toDirDTO(dir)})
}

func (h *Handler) deleteDir(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("path"))
	if err := h.svc.DeleteDir(r.Context(), user.ID, r.PathValue("id"), dir); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	file := strings.TrimSpace(r.URL.Query().Get("path"))
	if err := h.svc.DeleteFile(r.Context(), user.ID, r.PathValue("id"), file); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteDirLegacy(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteDir(r.Context(), user.ID, r.PathValue("id"), r.PathValue("name")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) createSkill(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	dir, err := h.svc.CreateSkill(r.Context(), user.ID, r.PathValue("id"), req.Name, req.Description)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"dir": toDirDTO(dir)})
}

// createDraft 按类型建插件草稿：技能 / 工具 / MCP / 组件 / 智能体 / 搜索源共用一条路径，
// 差别只在骨架文件与清单字段，前端不需要为每类各开一个接口。
func (h *Handler) createDraft(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	dir, err := h.svc.CreateDraft(r.Context(), user.ID, r.PathValue("id"), req.Kind, req.Name, req.Description)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"dir": toDirDTO(dir)})
}

type fileDTO struct {
	SpaceID string `json:"space_id"`
	Path    string `json:"path"`
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time,omitempty"`
	// Previewable=false 的文件不能读正文（file_read 与引用候选都认这个开关）
	Previewable bool `json:"previewable"`
	// PreviewKind 是面板渲染方式：text / image / video / audio / pdf / none
	PreviewKind PreviewKind `json:"preview_kind"`
}

// fileContentDTO 在文件元信息上追加正文，Truncated 表示超过读取上限被截断。
type fileContentDTO struct {
	fileDTO
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func toFileDTO(f File) fileDTO {
	dto := fileDTO{
		SpaceID:     f.SpaceID,
		Path:        f.Path,
		Dir:         f.Dir,
		Name:        f.Name,
		Size:        f.Size,
		Previewable: f.Previewable,
		PreviewKind: f.PreviewKind,
	}
	if !f.ModTime.IsZero() {
		dto.ModTime = f.ModTime.Format("2006-01-02T15:04:05Z07:00")
	}
	return dto
}

func (h *Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	files, err := h.svc.ListFiles(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]fileDTO, 0, len(files))
	for _, f := range files {
		out = append(out, toFileDTO(f))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"files": out})
}

func (h *Handler) readFile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	fc, err := h.svc.ReadFile(r.Context(), user.ID, r.PathValue("id"), rel)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"file": fileContentDTO{
			fileDTO:   toFileDTO(fc.File),
			Content:   fc.Content,
			Truncated: fc.Truncated,
		},
	})
}

// previewFile 内联返回浏览器可渲染的文件原始字节（图片 / 音视频 / PDF）。
//
// 与 downloadFile 的差别：按固定白名单给出真实 Content-Type 并 inline，浏览器才能渲染；
// 可寻址的存储直接交给 http.ServeContent，视频拖动进度条只取目标区间而不是整段重下。
// 端点与平台同源，因此只放行不会执行脚本的容器；SVG 是唯一能带脚本的格式，
// 靠 CSP sandbox 让它即便被直接打开也拿不到平台 Cookie 与 localStorage。
func (h *Handler) previewFile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	file, contentType, body, err := h.svc.PreviewFile(r.Context(), user.ID, r.PathValue("id"), rel)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	disposition := mime.FormatMediaType("inline", map[string]string{"filename": file.Name})
	if disposition == "" {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if contentType == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'")
	}

	// 本地 FS 的 Get 返回 *os.File，能直接走 Range / If-Modified-Since / HEAD；
	// 换成不支持 Seek 的存储时退回整段返回，功能不受影响（只是不能拖进度条）。
	if rs, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, file.Name, file.ModTime, rs)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, body); err != nil {
		h.log.Warn("文件预览中断", "space_id", file.SpaceID, "path", file.Path, "err", err)
	}
}

// downloadFile 把文件原始字节回给浏览器（预览不了的类型只有这一条取回路径）。
//
// 与 readFile 的两点差别：不看扩展名，也不按 256KB 截断——用户拿到的是本机上的完整文件，
// 不是预览用的前 256KB。Content-Disposition 交给 mime.FormatMediaType 生成：
// 中文名必须走 RFC 5987 的 filename*，直接拼 filename="中文" 浏览器会按 latin-1 解成乱码。
func (h *Handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	file, body, err := h.svc.DownloadFile(r.Context(), user.ID, r.PathValue("id"), rel)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": file.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	// 一律 octet-stream：下载端不做任何内联渲染，.html / .svg 不会被浏览器当页面执行
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	if _, err := io.Copy(w, body); err != nil {
		h.log.Warn("文件下载中断", "space_id", file.SpaceID, "path", file.Path, "err", err)
	}
}

// uploadFiles 接收 multipart/form-data：文件字段名 files，可选 dir 表示目标子目录。
//
// 一次最多 10 个文件，单个大小上限由配置注入（默认 100MB）；不限类型，同名策略由 Service.SaveFile 兜底。
func (h *Handler) uploadFiles(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	// 先限制整个请求体：multipart 解析前就要挡住超大请求，否则会先落盘再拒绝。
	// 多留 1MB 给表单边界与字段（Go 的 * 与 << 同级，括号不能省）
	limit := h.svc.UploadMaxBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit*maxUploadFiles+(1<<20))
	if err := r.ParseMultipartForm(maxUploadInMemory); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body",
			fmt.Sprintf("请用 multipart/form-data 上传文件，且单次不超过 %d 个、每个不超过 %dMB", maxUploadFiles, limit>>20))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	parts := r.MultipartForm.File["files"]
	if len(parts) == 0 {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "没有收到文件（字段名应为 files）")
		return
	}
	if len(parts) > maxUploadFiles {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", "一次最多上传 10 个文件")
		return
	}

	dir := strings.TrimSpace(r.FormValue("dir"))
	saved := make([]fileDTO, 0, len(parts))
	for _, header := range parts {
		// 解析阶段已经拿到真实字节数：先挡一次，超限文件不必写进存储再回滚
		if header.Size > limit {
			platform.WriteError(w, r, http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("「%s」超过 %dMB 上限", header.Filename, limit>>20))
			return
		}
		f, err := header.Open()
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		item, err := h.svc.SaveFile(r.Context(), user.ID, r.PathValue("id"), dir, header.Filename, f, header.Size)
		_ = f.Close()
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		saved = append(saved, toFileDTO(item))
	}

	platform.WriteJSON(w, http.StatusCreated, map[string]any{"files": saved})
}

func (h *Handler) current(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteAuthError(w, r, err)
		return store.User{}, false
	}
	return user, true
}

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "空间不存在或已删除")
	case errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, ErrReserved):
		platform.WriteError(w, r, http.StatusConflict, "reserved_space", err.Error())
	case errors.Is(err, ErrPluginReadOnly), errors.Is(err, ErrPluginRoot):
		platform.WriteError(w, r, http.StatusConflict, "plugin_readonly", err.Error())
	case errors.Is(err, ErrDirNotFound), errors.Is(err, ErrFileNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrFileUnsupported):
		platform.WriteError(w, r, http.StatusBadRequest, "unsupported_file", err.Error())
	case errors.Is(err, ErrFileNotText):
		// 单独一个 code：前端据此把面板退回「下载」，而不是当成读取失败
		platform.WriteError(w, r, http.StatusBadRequest, "not_text_file", err.Error())
	case errors.Is(err, ErrFileTooLarge):
		platform.WriteError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", err.Error())
	case errors.Is(err, ErrDirExists):
		platform.WriteError(w, r, http.StatusConflict, "dir_exists", err.Error())
	case errors.Is(err, ErrNameInvalid), errors.Is(err, ErrConfirmMismatch),
		errors.Is(err, ErrDirTooDeep):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("空间接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
