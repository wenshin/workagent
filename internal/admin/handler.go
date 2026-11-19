// Package admin 是平台级管理接口，目前只包含凭证维护。
//
// 为什么密钥不放在系统空间的文件树里：系统空间对管理员来说是「可下载的文件」，
// 明文密钥一旦进树，就同时进了备份、发布快照，以及任何拿到 read 权限的会话。
// 所以这里只提供「写入 + 指纹回显」，明文永远不出服务端。
package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/plugin"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
)

// maxSecretBytes 是口令 / 密钥的长度上限；搜索 key 一般 40 多字节，留足余量。
const maxSecretBytes = 4096

// SecretRef 描述平台某个能力引用的凭证：scope + name 用于寻址，label 是给管理员看的用途。
//
// 只有被 ref 引用的那条凭证会被平台真正使用，其余同 scope 下的条目只是历史遗留，
// 界面上用 in_use 标出来，避免管理员改了 A 却仍跑在 B 上。
type SecretRef struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	Label string `json:"label"`
	// Source 说明密钥从哪来：store（凭证存储，界面可管理）或 env（环境变量，
	// 由部署平台注入，这里只管显示，不需要也不能在界面上录入）。
	Source string `json:"source"`
}

// 凭证来源。
const (
	SourceStore = "store"
	SourceEnv   = "env"
)

// SecretItem 是一条已存凭证的元信息，不含明文。
type SecretItem struct {
	Scope       string    `json:"scope"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	UpdatedAt   time.Time `json:"updated_at"`
	InUse       bool      `json:"in_use"`
}

// Usage 返回「当前正在使用中」的凭证，键为 scope/name。
//
// 引用关系是运行期状态（装一个搜索源 / 停用一个工具就会变），所以不做静态表，
// 由调用方每次现算；传 nil 表示平台无法判断，此时不做使用中标记。
type Usage func(ctx context.Context) map[string]bool

// MCPApps 是「MCP 应用密钥」的管理员视图（plugin.Manager 实现）。
//
// 接口定义在这里、由装配层注入：admin 只负责鉴权与响应，密钥的生成、归属判定
// 与审计仍留在插件层，避免同一件事出现两套规则。
type MCPApps interface {
	// ListMCPApps 列出全部应用、归属与密钥指纹，不含明文。
	ListMCPApps(ctx context.Context) ([]plugin.MCPAppInfo, error)
	// RevealMCPAppKey 取回明文并写审计；只读不建，缺密钥时返回
	// plugin.ErrMCPAppKeyMissing。
	RevealMCPAppKey(ctx context.Context, actor, app string) (plugin.MCPAppInfo, string, error)
}

// Handler 提供 /admin 下的接口。
type Handler struct {
	spaces  *space.Service
	secrets secret.Store
	refs    []SecretRef
	usage   Usage
	mcpApps MCPApps
	resolve func(*http.Request) (store.User, error)
	log     *slog.Logger
}

// NewHandler 构造管理接口；secrets 允许为 nil（未配置 SECRET_KEK），
// refs 允许为空（凭证的引用关系由插件清单在运行期声明），usage 允许为 nil。
func NewHandler(spaces *space.Service, secrets secret.Store, refs []SecretRef, usage Usage,
	resolve func(*http.Request) (store.User, error), log *slog.Logger) *Handler {
	return &Handler{spaces: spaces, secrets: secrets, refs: refs, usage: usage, resolve: resolve, log: log}
}

// SetMCPApps 注入 MCP 应用密钥视图；不注入时相关接口返回 503。
func (h *Handler) SetMCPApps(view MCPApps) { h.mcpApps = view }

// Register 注册路由。全部要求系统管理员（按工号白名单）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/settings", h.settings)
	mux.HandleFunc("PUT /admin/secrets/{scope}/{name}", h.putSecret)
	mux.HandleFunc("DELETE /admin/secrets/{scope}/{name}", h.deleteSecret)
	// 应用密钥：列表只给指纹，明文按应用单独取，避免一次响应带走全部密钥。
	mux.HandleFunc("GET /admin/mcp-apps", h.listMCPApps)
	mux.HandleFunc("GET /admin/mcp-apps/{app}/key", h.revealMCPAppKey)
}

// settings 返回「平台引用了哪些凭证」以及每条凭证的指纹。
func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}

	refs := h.refs
	if refs == nil {
		refs = []SecretRef{}
	}
	enabled := h.secrets != nil && h.secrets.Enabled()
	items := make([]SecretItem, 0, len(refs))
	if enabled {
		usage := map[string]bool{}
		if h.usage != nil {
			usage = h.usage(r.Context())
		}
		// 列举的口径是「存储里有哪些 scope」，不是「refs 里有哪些 scope」：
		// 搜索源与插件声明的凭证都是运行期状态，只按 refs 列会让已经配好的密钥消失。
		for _, scope := range h.scopes(r.Context()) {
			list, err := h.secrets.Status(r.Context(), scope)
			if err != nil {
				h.log.Warn("读取凭证状态失败", "scope", scope, "err", err)
				continue
			}
			for _, it := range list {
				items = append(items, SecretItem{
					Scope:       scope,
					Name:        it.Name,
					Fingerprint: it.Fingerprint,
					UpdatedAt:   it.UpdatedAt,
					InUse:       h.inUse(refs, usage, scope, it.Name),
				})
			}
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"secrets_enabled": enabled,
		"refs":            refs,
		"items":           items,
	})
}

// scopes 汇总要列举的 scope：refs 声明的 + 存储里实际存在的（去重、稳定排序）。
func (h *Handler) scopes(ctx context.Context) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(h.refs)+1)
	add := func(scope string) {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			return
		}
		seen[scope] = true
		out = append(out, scope)
	}
	for _, ref := range h.refs {
		add(ref.Scope)
	}
	// 存储实现能列举 scope 时一并列出：否则「配过但没人引用」的条目不显示，
	// 管理员会以为密钥丢了（实际是 refs 传空了）。
	if lister, ok := h.secrets.(interface {
		Scopes() ([]string, error)
	}); ok {
		scopes, err := lister.Scopes()
		if err != nil {
			h.log.Warn("列举凭证 scope 失败", "err", err)
		}
		for _, scope := range scopes {
			add(scope)
		}
	}
	sort.Strings(out)
	return out
}

// inUse 判断某个 scope/name 是否正被使用：静态 refs 或运行期统计命中即可。
func (h *Handler) inUse(refs []SecretRef, usage map[string]bool, scope, name string) bool {
	if usage[scope+"/"+name] {
		return true
	}
	for _, ref := range refs {
		if ref.Scope == scope && ref.Name == name {
			return true
		}
	}
	return false
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

// listMCPApps 列出全部 MCP 应用及其密钥指纹。
//
// 系统空间页的「MCP 应用」入口用它：应用名是全局的（不随空间复制），所以这条
// 只能是平台级接口，不能挂在某个空间下面。
func (h *Handler) listMCPApps(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.mcpApps == nil {
		platform.WriteError(w, r, http.StatusServiceUnavailable, "not_ready", "MCP 应用视图未接入")
		return
	}
	items, err := h.mcpApps.ListMCPApps(r.Context())
	if err != nil {
		h.log.Error("列举 MCP 应用失败", "err", err)
		platform.WriteError(w, r, http.StatusInternalServerError, "internal", "读取 MCP 应用失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		// 凭证存储没启用时列表只有归属、没有密钥，界面按它给出提示
		"secrets_enabled": h.secrets != nil && h.secrets.Enabled(),
		"items":           items,
	})
}

// revealMCPAppKey 取回某个应用的签名密钥明文。
//
// 明文只回这一次响应，不写日志、不进审计（审计只记动作与对象）。
func (h *Handler) revealMCPAppKey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.mcpApps == nil {
		platform.WriteError(w, r, http.StatusServiceUnavailable, "not_ready", "MCP 应用视图未接入")
		return
	}
	info, key, err := h.mcpApps.RevealMCPAppKey(r.Context(), user.ID, r.PathValue("app"))
	switch {
	case errors.Is(err, plugin.ErrMCPAppName):
		platform.WriteError(w, r, http.StatusBadRequest, "bad_name", plugin.ErrMCPAppName.Error())
		return
	case errors.Is(err, plugin.ErrMCPAppKeyMissing):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, secret.ErrDisabled):
		platform.WriteError(w, r, http.StatusServiceUnavailable, "secrets_disabled", err.Error())
		return
	case err != nil:
		h.log.Error("读取 MCP 应用密钥失败", "app", r.PathValue("app"), "err", err)
		platform.WriteError(w, r, http.StatusInternalServerError, "internal", "读取密钥失败")
		return
	}

	h.log.Info("管理员查看 MCP 应用密钥", "app", info.App, "operator", user.ID)
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"app": info.App,
		"key": key,
	})
}

// requireAdmin 统一鉴权：未登录 401，非管理员 403。
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteAuthError(w, r, err)
		return store.User{}, false
	}
	if !h.spaces.IsAdmin(user) {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "只有系统管理员可以管理密钥")
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
