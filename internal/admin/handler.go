// Package admin 是平台级管理接口，目前只包含凭证维护与平台配置回显。
//
// 为什么密钥不放在系统空间的文件树里：系统空间对管理员来说是「可下载的文件」，
// 明文密钥一旦进树，就同时进了备份、发布快照，以及任何拿到 read 权限的会话。
// 所以这里只提供「写入 + 指纹回显」，明文永远不出服务端。
package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
)

// maxSecretBytes 是口令 / 密钥的长度上限；搜索 key 一般 40 多字节，留足余量。
const maxSecretBytes = 4096

// SearchSettings 是搜索相关配置的只读快照（不含密钥）。
type SearchSettings struct {
	Provider string   `json:"provider"`
	Endpoint string   `json:"endpoint"`
	Allow    []string `json:"allow"`
	// SecretName 是 config.json 里 search.secret 指向的凭证名
	SecretName    string     `json:"secret_name"`
	SecretPresent bool       `json:"secret_present"`
	Fingerprint   string     `json:"fingerprint,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// Handler 提供 /admin 下的接口。
type Handler struct {
	spaces  *space.Service
	secrets secret.Store
	search  SearchSettings
	resolve func(*http.Request) (store.User, error)
	log     *slog.Logger
}

// NewHandler 构造管理接口；secrets 允许为 nil（未配置 SECRET_KEK）。
func NewHandler(spaces *space.Service, secrets secret.Store, searchCfg SearchSettings,
	resolve func(*http.Request) (store.User, error), log *slog.Logger) *Handler {
	return &Handler{spaces: spaces, secrets: secrets, search: searchCfg, resolve: resolve, log: log}
}

// Register 注册路由。全部要求系统管理员（按工号白名单）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/settings", h.settings)
	mux.HandleFunc("PUT /admin/secrets/{scope}/{name}", h.putSecret)
	mux.HandleFunc("DELETE /admin/secrets/{scope}/{name}", h.deleteSecret)
}

// settings 返回平台配置与凭证指纹，供管理页展示「配没配、是什么指纹」。
func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}

	enabled := h.secrets != nil && h.secrets.Enabled()
	searchCfg := h.search
	if searchCfg.Allow == nil {
		searchCfg.Allow = []string{}
	}
	if enabled {
		items, err := h.secrets.Status(r.Context(), search.SecretScope)
		if err != nil {
			h.log.Warn("读取凭证状态失败", "scope", search.SecretScope, "err", err)
		}
		for _, it := range items {
			if it.Name != searchCfg.SecretName {
				continue
			}
			searchCfg.SecretPresent = true
			searchCfg.Fingerprint = it.Fingerprint
			updated := it.UpdatedAt
			searchCfg.UpdatedAt = &updated
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, struct {
		SecretsEnabled bool           `json:"secrets_enabled"`
		Search         SearchSettings `json:"search"`
	}{SecretsEnabled: enabled, Search: searchCfg})
}

// putSecret 写入或覆盖凭证，只回显指纹。
//
// 密钥只出现在请求体里：不写日志、不回显、不落明文。
func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.secrets == nil || !h.secrets.Enabled() {
		platform.WriteError(w, r, http.StatusServiceUnavailable, "secrets_disabled",
			"凭证存储未启用：请先配置 SECRET_KEK 并重启")
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	value := strings.TrimSpace(body.Value)
	switch {
	case value == "":
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "value 不能为空；删除凭证请用 DELETE")
		return
	case len(value) > maxSecretBytes:
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "凭证过长")
		return
	case strings.ContainsAny(value, "\r\n"):
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "凭证不能包含换行，请检查是否粘贴了多余内容")
		return
	}

	scope, name := r.PathValue("scope"), r.PathValue("name")
	if err := h.secrets.Put(r.Context(), scope, name, value); err != nil {
		h.writeSecretErr(w, r, err)
		return
	}

	h.log.Info("凭证已更新", "scope", scope, "name", name, "operator", user.ID)
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]string{
		"scope": scope, "name": name, "fingerprint": secret.Fingerprint(value),
	})
}

// deleteSecret 删除凭证；不存在视为成功。
func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.secrets == nil || !h.secrets.Enabled() {
		platform.WriteError(w, r, http.StatusServiceUnavailable, "secrets_disabled",
			"凭证存储未启用：请先配置 SECRET_KEK 并重启")
		return
	}

	scope, name := r.PathValue("scope"), r.PathValue("name")
	if err := h.secrets.Delete(r.Context(), scope, name); err != nil {
		h.writeSecretErr(w, r, err)
		return
	}

	h.log.Info("凭证已删除", "scope", scope, "name", name, "operator", user.ID)
	// 前端统一按 JSON 解析响应，不返回 204，避免调用方拿到空 body
	platform.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireAdmin 统一鉴权：未登录 401，非管理员 403。
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
		return store.User{}, false
	}
	if !h.spaces.IsAdmin(user) {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "只有系统管理员可以修改平台配置")
		return store.User{}, false
	}
	return user, true
}

func (h *Handler) writeSecretErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, secret.ErrNameInvalid):
		platform.WriteError(w, r, http.StatusBadRequest, "bad_name", err.Error())
	case errors.Is(err, secret.ErrDisabled):
		platform.WriteError(w, r, http.StatusServiceUnavailable, "secrets_disabled", err.Error())
	default:
		h.log.Error("凭证操作失败", "err", err)
		platform.WriteError(w, r, http.StatusInternalServerError, "internal", "凭证操作失败")
	}
}
