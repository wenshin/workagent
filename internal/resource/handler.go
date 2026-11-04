package resource

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

// UserResolver 从请求中解析当前登录用户。
type UserResolver func(r *http.Request) (store.User, error)

// Handler 提供「资源管理」HTTP 端点。
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
	mux.HandleFunc("GET /resources", h.list)
	mux.HandleFunc("POST /resources", h.publish)
	mux.HandleFunc("GET /resources/{id}", h.detail)
	mux.HandleFunc("PATCH /resources/{id}", h.setStatus)
	mux.HandleFunc("DELETE /resources/{id}", h.remove)
	mux.HandleFunc("DELETE /resources/{id}/versions/{version}", h.removeVersion)
}

// resourceDTO 是资源管理板块展示的最小信息：只有名称、版本、描述等，
// 不含发布目录、原始 MCP 配置、tool 文件与 DB 连接串。
type resourceDTO struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Visibility      string `json:"visibility"`
	Status          string `json:"status"`
	OwnerID         string `json:"owner_id"`
	OwnerName       string `json:"owner_name"`
	SourceSpaceID   string `json:"source_space_id,omitempty"`
	SourceSpaceName string `json:"source_space_name,omitempty"`
	LatestVersion   string `json:"latest_version"`
	VersionCount    int    `json:"version_count"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type versionDTO struct {
	Version    string `json:"version"`
	Notes      string `json:"notes"`
	FileCount  int    `json:"file_count"`
	TotalBytes int64  `json:"total_bytes"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  string `json:"created_at"`
}

func toDTO(r store.Resource) resourceDTO {
	return resourceDTO{
		ID:              r.ID,
		Kind:            r.Kind,
		Name:            r.Name,
		Description:     r.Description,
		Visibility:      r.Visibility,
		Status:          r.Status,
		OwnerID:         r.OwnerID,
		OwnerName:       r.OwnerName,
		SourceSpaceID:   r.SourceSpaceID,
		SourceSpaceName: r.SourceSpaceName,
		LatestVersion:   r.LatestVersion,
		VersionCount:    r.VersionCount,
		CreatedAt:       r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:       r.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func toVersionDTO(v store.ResourceVersion) versionDTO {
	return versionDTO{
		Version:    v.Version,
		Notes:      v.Notes,
		FileCount:  v.FileCount,
		TotalBytes: v.TotalBytes,
		CreatedBy:  v.CreatedBy,
		CreatedAt:  v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (h *Handler) current(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return store.User{}, false
	}
	return user, true
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
	out := make([]resourceDTO, 0, len(list))
	for _, item := range list {
		out = append(out, toDTO(item))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"resources": out})
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	var req struct {
		SpaceID    string `json:"space_id"`
		Path       string `json:"path"`
		Kind       string `json:"kind"`
		Version    string `json:"version"`
		Notes      string `json:"notes"`
		Visibility string `json:"visibility"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	res, err := h.svc.Publish(r.Context(), user, PublishInput{
		SpaceID:    req.SpaceID,
		Path:       req.Path,
		Kind:       req.Kind,
		Version:    req.Version,
		Notes:      req.Notes,
		Visibility: req.Visibility,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"resource": toDTO(res)})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	res, versions, err := h.svc.Detail(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]versionDTO, 0, len(versions))
	for _, v := range versions {
		out = append(out, toVersionDTO(v))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"resource": toDTO(res), "versions": out})
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	if err := h.svc.SetStatus(r.Context(), user, r.PathValue("id"), req.Status); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), user, r.PathValue("id")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) removeVersion(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteVersion(r.Context(), user, r.PathValue("id"), r.PathValue("version")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "资源或版本不存在")
	case errors.Is(err, ErrNoPermission):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, ErrVersionExists):
		platform.WriteError(w, r, http.StatusConflict, "version_exists", err.Error())
	case errors.Is(err, ErrBadVersion), errors.Is(err, ErrUnsupportedKind), errors.Is(err, ErrNotSkill),
		errors.Is(err, ErrNoDoc), errors.Is(err, ErrLastVersion), errors.Is(err, ErrTooLarge):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("资源接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
