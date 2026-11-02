package session

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/autohome/auto-agent/internal/platform"
	"github.com/autohome/auto-agent/internal/space"
	"github.com/autohome/auto-agent/internal/store"
)

// UserResolver 从请求中解析当前登录用户。
type UserResolver func(r *http.Request) (store.User, error)

// Handler 提供会话 HTTP 端点。
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
	mux.HandleFunc("GET /spaces/{id}/sessions", h.list)
	mux.HandleFunc("POST /spaces/{id}/sessions", h.create)
}

type sessionDTO struct {
	ID        string `json:"id"`
	SpaceID   string `json:"space_id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func toDTO(s store.Session) sessionDTO {
	return sessionDTO{
		ID:        s.ID,
		SpaceID:   s.SpaceID,
		Title:     s.Title,
		Status:    s.Status,
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	out := make([]sessionDTO, 0, len(list))
	for _, s := range list {
		out = append(out, toDTO(s))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if r.ContentLength != 0 {
		if err := platform.DecodeJSON(r, &req); err != nil {
			platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
	}

	sess, err := h.svc.Create(r.Context(), user.ID, r.PathValue("id"), req.Title)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"session": toDTO(sess)})
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
	case errors.Is(err, space.ErrForbidden), errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "没有该空间的权限")
	case errors.Is(err, ErrNotSessionSpace), errors.Is(err, ErrTitleInvalid):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("会话接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
