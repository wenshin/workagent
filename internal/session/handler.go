package session

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

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
	mux.HandleFunc("GET /spaces/{id}/sessions", h.listBySpace)
	mux.HandleFunc("POST /spaces/{id}/sessions", h.create)
	mux.HandleFunc("GET /sessions", h.listMine)
	mux.HandleFunc("PATCH /sessions/{id}", h.rename)
	mux.HandleFunc("DELETE /sessions/{id}", h.remove)
	mux.HandleFunc("GET /sessions/{id}/messages", h.messages)
	mux.HandleFunc("POST /sessions/{id}/messages", h.appendMessage)
}

type sessionDTO struct {
	ID            string  `json:"id"`
	SpaceID       string  `json:"space_id"`
	Title         string  `json:"title"`
	TitleAuto     bool    `json:"title_auto"`
	Status        string  `json:"status"`
	LastMessageAt *string `json:"last_message_at"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type messageDTO struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

const timeLayout = "2006-01-02T15:04:05Z07:00"

func toDTO(s store.Session) sessionDTO {
	dto := sessionDTO{
		ID:        s.ID,
		SpaceID:   s.SpaceID,
		Title:     s.Title,
		TitleAuto: s.TitleAuto,
		Status:    s.Status,
		CreatedAt: s.CreatedAt.Format(timeLayout),
		UpdatedAt: s.UpdatedAt.Format(timeLayout),
	}
	if s.LastMessageAt != nil {
		v := s.LastMessageAt.Format(timeLayout)
		dto.LastMessageAt = &v
	}
	return dto
}

func toMessageDTO(m store.Message) messageDTO {
	return messageDTO{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Content:   m.Content,
		CreatedAt: m.CreatedAt.In(time.Local).Format(timeLayout),
	}
}

func (h *Handler) listBySpace(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessionDTOs(list)})
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListMine(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessionDTOs(list)})
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

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sess, err := h.svc.Rename(r.Context(), user.ID, r.PathValue("id"), req.Title)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"session": toDTO(sess)})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), user.ID, r.PathValue("id")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Messages(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	out := make([]messageDTO, 0, len(list))
	for _, m := range list {
		out = append(out, toMessageDTO(m))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (h *Handler) appendMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	msg, sess, err := h.svc.AppendMessage(r.Context(), user.ID, r.PathValue("id"), req.Content)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{
		"message": toMessageDTO(msg),
		"session": toDTO(sess),
	})
}

func sessionDTOs(list []store.Session) []sessionDTO {
	out := make([]sessionDTO, 0, len(list))
	for _, s := range list {
		out = append(out, toDTO(s))
	}
	return out
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
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "会话或空间不存在")
	case errors.Is(err, space.ErrForbidden), errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "没有该空间的权限")
	case errors.Is(err, ErrLastSession):
		platform.WriteError(w, r, http.StatusConflict, "last_session", err.Error())
	case errors.Is(err, ErrTitleInvalid), errors.Is(err, ErrMessageEmpty), errors.Is(err, ErrMessageTooLong),
		errors.Is(err, space.ErrNameInvalid):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("会话接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
