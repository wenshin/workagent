package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

// UserResolver 从请求中解析当前登录用户（与 resource/session 保持一致）。
type UserResolver func(r *http.Request) (store.User, error)

// SpaceAccess 是插件接口需要的空间能力（space.Service 实现）。
type SpaceAccess interface {
	SpacePerm(ctx context.Context, spaceID, userID string) (string, error)
	IsAdmin(user store.User) bool
	// SystemSpaceID 用于把系统空间的发布件识别为「平台内置」。
	SystemSpaceID(ctx context.Context) (string, error)
}

// Handler 暴露插件的开发、发布、安装、运行接口。
type Handler struct {
	mgr     *Manager
	store   store.Store
	spaces  SpaceAccess
	resolve UserResolver
	log     *slog.Logger
}

// NewHandler 构造 HTTP 处理器。
func NewHandler(mgr *Manager, st store.Store, spaces SpaceAccess, resolve UserResolver, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{mgr: mgr, store: st, spaces: spaces, resolve: resolve, log: log}
}

// Register 注册路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /spaces/{id}/plugins", h.list)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}", h.detail)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/config", h.config)
	mux.HandleFunc("PUT /spaces/{id}/plugins/{slug}/config", h.updateConfig)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/dry-run", h.dryRun)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/publish", h.publish)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/install", h.install)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/uninstall", h.uninstall)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/enable", h.enable)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/disable", h.disable)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/baseline", h.baseline)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/baseline/reset", h.resetBaseline)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/baseline/dismiss", h.dismissUpdate)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/data", h.data)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/ui", h.ui)
	// 资源管理：全部已发布插件，只读。
	mux.HandleFunc("GET /plugins", h.catalog)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	items, err := h.mgr.List(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	// 待发布候选的内容只有系统管理员能看；其他用户仍按空间 read 权限。
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "未登录")
		return
	}
	if !h.spaces.IsAdmin(user) {
		if _, _, ok := h.guard(w, r, "read"); !ok {
			return
		}
	}
	item, manifest, err := h.mgr.Detail(r.Context(), r.PathValue("id"), r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"item": item, "manifest": manifest})
}

func (h *Handler) dryRun(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "write")
	if !ok {
		return
	}
	var body struct {
		Input json.RawMessage `json:"input"`
	}
	_ = platform.DecodeJSON(r, &body)
	if len(body.Input) == 0 {
		body.Input = json.RawMessage(`{}`)
	}
	res, err := h.mgr.DryRun(r.Context(), r.PathValue("id"), r.PathValue("slug"), user.ID, body.Input)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	// 发布权只在系统管理员；不要求是来源空间成员。
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "未登录")
		return
	}
	if !h.spaces.IsAdmin(user) {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "只有系统管理员可以发布插件")
		return
	}
	var body struct {
		Version string `json:"version"`
		Notes   string `json:"notes"`
	}
	_ = platform.DecodeJSON(r, &body)
	v, err := h.mgr.Publish(r.Context(), r.PathValue("id"), r.PathValue("slug"), user.ID,
		body.Version, body.Notes, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"version": v.Version, "sha256": v.SHA256, "risk": v.Risk,
	})
}

func (h *Handler) install(w http.ResponseWriter, r *http.Request) {
	user, perm, ok := h.guard(w, r, "manage")
	if !ok {
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	_ = platform.DecodeJSON(r, &body)
	in, err := h.mgr.Install(r.Context(), InstallInput{
		SpaceID: r.PathValue("id"), Slug: r.PathValue("slug"), Version: body.Version,
		UserID: user.ID, Perm: perm,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"slug": in.Slug, "version": in.Version, "risk": in.Risk, "status": in.Status,
	})
}

func (h *Handler) uninstall(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "manage")
	if !ok {
		return
	}
	if err := h.mgr.Uninstall(r.Context(), user.ID, r.PathValue("id"), r.PathValue("slug")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) enable(w http.ResponseWriter, r *http.Request)  { h.toggle(w, r, true) }
func (h *Handler) disable(w http.ResponseWriter, r *http.Request) { h.toggle(w, r, false) }

func (h *Handler) toggle(w http.ResponseWriter, r *http.Request, enabled bool) {
	user, _, ok := h.guard(w, r, "manage")
	if !ok {
		return
	}
	spaceID, slug := r.PathValue("id"), r.PathValue("slug")
	err := h.mgr.SetEnabled(r.Context(), user.ID, spaceID, slug, enabled)
	if errors.Is(err, store.ErrNotFound) {
		// 没有安装记录时按本地开发插件处理：启用会写候选并进入本空间运行池。
		if enabled {
			_, err = h.mgr.EnableLocal(r.Context(), spaceID, slug, user.ID, user.Name)
		} else {
			err = h.mgr.DisableLocal(r.Context(), spaceID, slug, user.ID)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) baseline(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	st, err := h.mgr.BaselineStatus(r.Context(), r.PathValue("id"), r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, st)
}

func (h *Handler) resetBaseline(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "write")
	if !ok {
		return
	}
	if err := h.mgr.ResetBaseline(r.Context(), user.ID, r.PathValue("id"), r.PathValue("slug")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) dismissUpdate(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	_ = platform.DecodeJSON(r, &body)
	if err := h.mgr.DismissUpdate(r.Context(), r.PathValue("id"), r.PathValue("slug"), body.Version); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) data(w http.ResponseWriter, r *http.Request) {
	user, perm, ok := h.guard(w, r, "read")
	if !ok {
		return
	}
	var body struct {
		Source  string          `json:"source"`
		Params  json.RawMessage `json:"params"`
		Gesture bool            `json:"gesture"`
	}
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	// 只读视角不允许声明「用户手势」，防止组件伪造写操作授权。
	if perm == "read" || perm == "view" {
		body.Gesture = false
	}
	out, err := h.mgr.Data(r.Context(), r.PathValue("id"), user.ID, perm, r.PathValue("slug"),
		body.Source, body.Params, body.Gesture)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// config 返回安装级配置字段与当前值（凭证只回显名字）。
func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	view, err := h.mgr.ConfigView(r.Context(), r.PathValue("id"), r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, view)
}

// updateConfig 保存安装级配置：需要 manage 权限，保存后立即重建运行时。
func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "manage")
	if !ok {
		return
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	view, err := h.mgr.UpdateConfig(r.Context(), user.ID, r.PathValue("id"), r.PathValue("slug"), body.Values)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, view)
}

// catalogItem 是插件市场的展示字段：只有清单快照，不返回产物路径与连接配置。
//
// 市场分两类来源（组织内 / 外部）和三种状态（待发布 / 已发布 / 平台内置）。
// 「待发布」只暴露元数据，文件内容仍只有系统管理员能读。
type catalogItem struct {
	Slug          string `json:"slug"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	LatestVersion string `json:"latest_version"`
	Visibility    string `json:"visibility"`
	Extensible    bool   `json:"extensible"`
	Risk          int    `json:"risk"`
	Origin        string `json:"origin"`   // internal / system / external
	Category      string `json:"category"` // internal / system / external
	Pending       bool   `json:"pending"`
	Installable   bool   `json:"installable"`
	CanPublish    bool   `json:"can_publish"`
	SourceSpaceID string `json:"source_space_id,omitempty"`
	SourceSpace   string `json:"source_space,omitempty"`
	Author        string `json:"author,omitempty"`
	Published     bool   `json:"published"`
	PublishedSlug string `json:"published_slug,omitempty"`
}

func catalogCategory(origin string) string {
	switch origin {
	case store.PluginOriginSystem:
		return "system"
	case store.PluginOriginExternal:
		return "external"
	default:
		return "internal"
	}
}

func (h *Handler) catalog(w http.ResponseWriter, r *http.Request) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "未登录")
		return
	}
	isAdmin := h.spaces.IsAdmin(user)
	all, err := h.store.ListPlugins(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]catalogItem, 0, len(all))
	seen := map[string]bool{}
	for _, p := range all {
		// 私有发布件只对来源空间成员可见：目录列表不能变成跨空间枚举名称的通道。
		if p.Visibility == store.PluginVisibilityPrivate && p.OwnerSpaceID != "" {
			if perm, err := h.spaces.SpacePerm(r.Context(), p.OwnerSpaceID, user.ID); err != nil || perm == "" {
				continue
			}
		}
		risk := 0
		if v, err := h.store.GetPluginVersion(r.Context(), p.ID, p.LatestVersion); err == nil {
			risk = v.Risk
		}
		sourceSpace := ""
		if p.OwnerSpaceID != "" {
			if sp, err := h.store.GetSpace(r.Context(), p.OwnerSpaceID); err == nil {
				sourceSpace = sp.Name
			}
		}
		seen[p.Slug] = true
		items = append(items, catalogItem{
			Slug: p.Slug, Kind: p.Kind, Name: p.Name, Description: p.Description,
			LatestVersion: p.LatestVersion, Visibility: p.Visibility,
			Extensible: p.Extensible, Risk: risk,
			Origin: p.Origin, Category: catalogCategory(p.Origin),
			// 平台内置件默认对所有空间可用，没有「安装」这个动作。
			Installable: p.Origin != store.PluginOriginSystem, Published: true,
			SourceSpaceID: p.OwnerSpaceID, SourceSpace: sourceSpace,
		})
	}

	// 「组织内 · 待发布」：空间已启用、尚未发布；已发布但内容有变化时也列出来。
	cands, err := h.mgr.ListCandidates(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, c := range cands {
		if c.Status != store.PluginCandidateEnabled {
			continue
		}
		pending := true // 进入这个列表就是待发布或待更新
		publishedVersion := ""
		if c.PluginID != "" {
			p, err := h.store.GetPluginBySlug(r.Context(), c.Slug)
			if err != nil {
				continue
			}
			publishedVersion = p.LatestVersion
			v, err := h.store.GetPluginVersion(r.Context(), p.ID, p.LatestVersion)
			if err == nil && v.SHA256 == c.ManifestHash {
				continue // 已发布且没有变化
			}
		}
		if seen[c.Slug] && c.PluginID == "" {
			// 同 slug 已被别的空间发布，候选无法发布；不展示重复条目。
			continue
		}
		items = append(items, catalogItem{
			Slug: c.Slug, Kind: c.Kind, Name: c.Name, Description: c.Description,
			LatestVersion: publishedVersion, Visibility: store.PluginVisibilityPublic,
			Risk: c.Risk, Origin: store.PluginOriginInternal, Category: "internal",
			Pending: pending, Installable: false, CanPublish: isAdmin,
			SourceSpaceID: c.SpaceID, SourceSpace: c.SpaceName, Author: c.AuthorName,
			Published: c.PluginID != "", PublishedSlug: c.Slug,
		})
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "is_admin": isAdmin})
}

// ui 返回渲染组件所需的清单 + DSL。只开放给「已安装该组件」的空间成员：
// 渲染意味着可以用当前身份取数，未安装却能渲染等于绕过安装审批。
func (h *Handler) ui(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	view, err := h.mgr.ComponentView(r.Context(), r.PathValue("id"), r.PathValue("slug"),
		r.URL.Query().Get("version"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, view)
}

// guard 校验空间权限，返回用户与实际权限。
func (h *Handler) guard(w http.ResponseWriter, r *http.Request, need string) (store.User, string, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "未登录")
		return store.User{}, "", false
	}
	perm, err := h.spaces.SpacePerm(r.Context(), r.PathValue("id"), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return store.User{}, "", false
	}
	if perm == "" {
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "空间不存在")
		return store.User{}, "", false
	}
	if !permAtLeast(perm, need) {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "没有权限")
		return store.User{}, "", false
	}
	return user, perm, true
}

func permAtLeast(got, need string) bool {
	rank := map[string]int{"": 0, "view": 1, "read": 2, "write": 3, "manage": 4}
	return rank[got] >= rank[need]
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "插件不存在")
	case errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, ErrConflict), errors.Is(err, ErrReferenced):
		platform.WriteError(w, r, http.StatusConflict, "conflict", err.Error())
	default:
		if isValidation(err) {
			platform.WriteError(w, r, http.StatusBadRequest, "invalid_manifest", err.Error())
			return
		}
		h.log.Warn("插件接口失败", "err", err)
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "插件操作失败")
	}
}

// isValidation 粗判清单校验错误：这类原因必须原样给作者，
// 否则改清单只能靠猜。
func isValidation(err error) bool {
	msg := err.Error()
	for _, kw := range []string{
		"清单", "slug", "必须", "不允许", "只能是", "超过", "schema",
		"protocol", "kind", "capability", "hosts", "tools.allow", "transport", "未知配置项",
		"缺少 plugin.json", "体积",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
