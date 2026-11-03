package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

// Handler 提供登录相关 HTTP 端点。
type Handler struct {
	svc *Service
	cfg *config.Config
	log *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(svc *Service, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{svc: svc, cfg: cfg, log: log}
}

// webURL 拼接前端站点地址。
func (h *Handler) webURL(p string) string { return h.cfg.WebBaseURL + p }

// Register 注册路由。
//
// 登录端点与 IdP 无关（§5.1.1），历史 /auth/dingtalk/* 保留为别名。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login/url", h.AuthURL)
	mux.HandleFunc("GET /auth/login", h.Login)
	mux.HandleFunc("GET /auth/callback", h.CallbackRedirect)
	mux.HandleFunc("POST /auth/callback", h.CallbackAPI)
	mux.HandleFunc("GET /auth/provider", h.Provider)

	mux.HandleFunc("GET /auth/dingtalk/url", h.AuthURL)
	mux.HandleFunc("GET /auth/dingtalk/login", h.Login)
	mux.HandleFunc("GET /auth/dingtalk/callback", h.CallbackRedirect)
	mux.HandleFunc("POST /auth/dingtalk/callback", h.CallbackAPI)

	mux.HandleFunc("GET /auth/me", h.Me)
	mux.HandleFunc("POST /auth/logout", h.Logout)
}

type userDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	Email     string `json:"email"`
	JobNumber string `json:"job_number"`
	CorpName  string `json:"corp_name"`
	// ProfileWarning 为通讯录同步失败原因，成功时为空
	ProfileWarning string `json:"profile_warning"`
	Provider       string `json:"provider"`
}

type spaceDTO struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	ParentID         string `json:"parent_id,omitempty"`
	Path             string `json:"path"`
	Depth            int    `json:"depth"`
	IsSystemReserved bool   `json:"is_system_reserved"`
	Perm             string `json:"perm"`
}

func toUserDTO(u store.User) userDTO {
	return userDTO{
		ID: u.ID, Name: u.Name, Avatar: u.Avatar,
		Email: u.Email, JobNumber: u.JobNumber, CorpName: u.CorpName,
		ProfileWarning: u.ProfileWarning, Provider: u.Provider,
	}
}

func toSpaceDTOs(list []store.Space) []spaceDTO {
	out := make([]spaceDTO, 0, len(list))
	for _, s := range list {
		out = append(out, spaceDTO{
			ID: s.ID, Name: s.Name, Type: s.Type, ParentID: s.ParentID,
			Path: s.Path, Depth: s.Depth, IsSystemReserved: s.IsSystemReserved, Perm: s.Perm,
		})
	}
	return out
}

// AuthURL 返回授权地址（供前端自行跳转）。
func (h *Handler) AuthURL(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.LoginURL()
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"url": u})
}

// Provider 返回当前 IdP 信息，登录页据此渲染按钮文案与缺失提示。
func (h *Handler) Provider(w http.ResponseWriter, r *http.Request) {
	name, display, ready, missing := h.svc.ProviderInfo()
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"provider":     name,
		"display_name": display,
		"ready":        ready,
		"missing":      missing,
	})
}

// Login 直接 302 到 IdP 授权页。
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.LoginURL()
	if err != nil {
		h.log.Warn("生成登录地址失败", "err", err)
		http.Redirect(w, r, h.webURL("/login")+"?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// CallbackRedirect 处理钉钉回调，写入 Cookie 后跳回首页。
func (h *Handler) CallbackRedirect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if msg := q.Get("error"); msg != "" {
		_, display, _, _ := h.svc.ProviderInfo()
		http.Redirect(w, r, h.webURL("/login")+"?error="+url.QueryEscape(display+"授权失败: "+msg), http.StatusFound)
		return
	}

	res, err := h.svc.CompleteLogin(r.Context(), q.Get("code"), q.Get("state"))
	if err != nil {
		h.log.Warn("登录失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		http.Redirect(w, r, h.webURL("/login")+"?error="+url.QueryEscape(h.publicMessage(err)), http.StatusFound)
		return
	}

	h.setCookie(w, res.Token)
	http.Redirect(w, r, h.cfg.WebBaseURL+"/", http.StatusFound)
}

// CallbackAPI 以 JSON 方式完成登录（供 SPA 使用）。
func (h *Handler) CallbackAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON")
		return
	}

	res, err := h.svc.CompleteLogin(r.Context(), req.Code, req.State)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	h.setCookie(w, res.Token)
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"user":   toUserDTO(res.User),
		"spaces": toSpaceDTOs(res.Spaces),
	})
}

// Me 返回当前登录用户与空间列表。
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	token := h.tokenFromRequest(r)
	user, err := h.svc.Authenticate(r.Context(), token)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
		return
	}
	spaces, err := h.svc.Spaces(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"user":   toUserDTO(user),
		"spaces": toSpaceDTOs(spaces),
	})
}

// Logout 让当前会话立即失效。表单提交跳转登录页，API 调用返回 JSON。
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	token := h.tokenFromRequest(r)
	if err := h.svc.Logout(r.Context(), token); err != nil {
		h.writeErr(w, r, err)
		return
	}
	h.clearCookie(w)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		http.Redirect(w, r, h.webURL("/login"), http.StatusFound)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// CurrentUser 供页面层复用（未登录返回错误）。
func (h *Handler) CurrentUser(r *http.Request) (store.User, error) {
	return h.svc.Authenticate(r.Context(), h.tokenFromRequest(r))
}

func (h *Handler) tokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(h.cfg.SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (h *Handler) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, h.newCookie(token, int(h.cfg.SessionTTL.Seconds())))
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, h.newCookie("", -1))
}

// newCookie 生成会话 Cookie。前后端同源（经反向代理），使用 Lax 即可。
func (h *Handler) newCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     h.cfg.SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotImplemented):
		_, display, _, _ := h.svc.ProviderInfo()
		platform.WriteError(w, r, http.StatusNotImplemented, "not_implemented",
			"身份接入方「"+display+"」尚未实现，当前仅支持 dingtalk")
	case errors.Is(err, ErrNotConfigured):
		missing := strings.Join(h.svc.MissingConfig(), ", ")
		platform.WriteError(w, r, http.StatusServiceUnavailable, "not_configured",
			"登录未配置，请设置环境变量: "+missing)
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrInvalidState):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, ErrNoIdentity), errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", err.Error())
	default:
		h.log.Error("auth error", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "登录失败，请重试或联系管理员")
	}
}

func (h *Handler) publicMessage(err error) string {
	switch {
	case errors.Is(err, ErrNotImplemented):
		_, display, _, _ := h.svc.ProviderInfo()
		return "身份接入方「" + display + "」尚未实现，请联系管理员改用已支持的登录方式"
	case errors.Is(err, ErrNotConfigured):
		return "登录未配置，请联系管理员补全：" + strings.Join(h.svc.MissingConfig(), " / ")
	case errors.Is(err, ErrInvalidState):
		return "登录已超时，请重新点击「钉钉登录」"
	case errors.Is(err, ErrNoIdentity):
		return "未能获取你的钉钉身份，请确认应用权限配置"
	case errors.Is(err, ErrForbidden):
		return "账号已被禁用，请联系管理员"
	default:
		return "登录失败：" + err.Error()
	}
}
