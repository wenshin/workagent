package space

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

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
	// 这两个端点只读，且只暴露 md/json 等文本类型。
	mux.HandleFunc("GET /spaces/files", h.listFiles)
	mux.HandleFunc("GET /spaces/{id}/file", h.readFile)
	// 上传只接受可预览的文本类型，落在空间 files/ 下（dir 可选，相对 files/）
	mux.HandleFunc("POST /spaces/{id}/files", h.uploadFiles)
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
}

// fileContentDTO 在文件元信息上追加正文，Truncated 表示超过读取上限被截断。
type fileContentDTO struct {
	fileDTO
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func toFileDTO(f File) fileDTO {
	dto := fileDTO{
		SpaceID: f.SpaceID,
		Path:    f.Path,
		Dir:     f.Dir,
		Name:    f.Name,
		Size:    f.Size,
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

// uploadFiles 接收 multipart/form-data：文件字段名 files，可选 dir 表示目标子目录。
//
// 一次最多 10 个文件，单个大小上限由配置注入（默认 4MB）；扩展名与同名策略由 Service.SaveFile 兜底。
func (h *Handler) uploadFiles(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	// 先限制整个请求体：multipart 解析前就要挡住超大请求，否则会先落盘再拒绝。
	// 多留 1MB 给表单边界与字段（Go 的 * 与 << 同级，括号不能省）
	limit := h.svc.UploadMaxBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit*maxUploadFiles+(1<<20))
	if err := r.ParseMultipartForm(limit); err != nil {
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
		f, err := header.Open()
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		item, err := h.svc.SaveFile(r.Context(), user.ID, r.PathValue("id"), dir, header.Filename, f)
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
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
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
	case errors.Is(err, ErrDirNotFound), errors.Is(err, ErrFileNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrFileUnsupported):
		platform.WriteError(w, r, http.StatusBadRequest, "unsupported_file", err.Error())
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
