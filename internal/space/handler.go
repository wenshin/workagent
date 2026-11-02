package space

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/autohome/auto-agent/internal/platform"
	"github.com/autohome/auto-agent/internal/store"
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

	var req struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sp, err := h.svc.Create(r.Context(), user.ID, CreateInput{Name: req.Name, ParentID: req.ParentID})
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

func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	rootID := r.PathValue("id")
	root, err := h.svc.Get(r.Context(), user.ID, rootID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	all, err := h.svc.List(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	var sub []store.Space
	for _, s := range all {
		if s.ID == root.ID || strings.HasPrefix(s.Path, root.Path+"/") {
			sub = append(sub, s)
		}
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"spaces": toDTOs(BuildTree(sub))})
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
	case errors.Is(err, ErrNameInvalid), errors.Is(err, ErrConfirmMismatch), errors.Is(err, ErrDepthExceeded):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("空间接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
