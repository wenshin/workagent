package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/store"
)

// UserResolver 从请求中解析当前登录用户（与 session 保持一致）。
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
	// 插件开发执行体：在草稿目录里跑命令（装依赖、deno cache、试跑）。
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/dev/exec", h.devExec)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/publish", h.publish)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/install", h.install)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/uninstall", h.uninstall)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/enable", h.enable)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/disable", h.disable)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/jwt-key", h.jwtKey)
	// 会话里的「应用密钥」卡片：按应用名生成或取回，只回给创建者本人。
	mux.HandleFunc("POST /spaces/{id}/mcp-apps/{app}/key", h.mcpAppKey)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/baseline", h.baseline)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/baseline/reset", h.resetBaseline)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/baseline/dismiss", h.dismissUpdate)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/data", h.data)
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/ui", h.ui)
	// 智能体头像：读走空间 read（会话列表要渲染），写走空间 write（只能改草稿）
	mux.HandleFunc("GET /spaces/{id}/plugins/{slug}/icon", h.agentIcon)
	mux.HandleFunc("POST /spaces/{id}/plugins/{slug}/icon", h.uploadAgentIcon)
	mux.HandleFunc("DELETE /spaces/{id}/plugins/{slug}/icon", h.removeAgentIcon)
	// 插件市场：全部已发布插件，只读。
	mux.HandleFunc("GET /plugins", h.catalog)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "read")
	if !ok {
		return
	}
	items, err := h.mgr.List(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// is_admin：平台凭证只有系统管理员能写，前端据此决定卡片上给「配置密钥」
	// 还是「转给管理员」（密钥是平台级资源，不是空间内容）。
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"is_admin": h.spaces.IsAdmin(user),
	})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	// 待发布候选的内容只有系统管理员能看；其他用户仍按空间 read 权限。
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteAuthError(w, r, err)
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

// devExec 在插件草稿目录里执行命令。
//
// 权限按空间 write 校验：能改草稿的人才能在里面跑命令。
// 不做审批但记录执行结果，命令原文由调用方自己保存（前端展示用）。
func (h *Handler) devExec(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "write")
	if !ok {
		return
	}
	var body struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	res, err := h.mgr.DevExec(r.Context(), r.PathValue("id"), r.PathValue("slug"),
		user.ID, body.Command, time.Duration(body.TimeoutSeconds)*time.Second)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"exit_code":   res.ExitCode,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"truncated":   res.Truncated,
		"sandbox":     res.Sandbox,
		"duration_ms": res.Duration.Milliseconds(),
	})
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	// 发布权只在系统管理员；不要求是来源空间成员。
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteAuthError(w, r, err)
		return
	}
	if !h.spaces.IsAdmin(user) {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "只有系统管理员可以发布插件")
		return
	}
	var body struct {
		Version string `json:"version"`
		Notes   string `json:"notes"`
		// AppKey 只在「应用名不属于自己」时需要：填它等于确认拿到过创建者的授权
		AppKey string `json:"app_key"`
	}
	_ = platform.DecodeJSON(r, &body)
	v, err := h.mgr.Publish(r.Context(), r.PathValue("id"), r.PathValue("slug"), user.ID,
		body.Version, body.Notes, body.AppKey, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := map[string]any{"version": v.Version, "sha256": v.SHA256, "risk": v.Risk}
	// jwt 模式的 MCP 插件：发布直接回显应用名与密钥，省掉一次往返。
	// 非 jwt 插件会返回 ErrConflict（这条路径是常态，不记日志）。
	if app, key, err := h.mgr.MCPAppKey(r.Context(), r.PathValue("id"), r.PathValue("slug")); err == nil {
		out["jwt"] = map[string]any{"app": app, "key": key, "scope": mcp.MCPJWTScope}
	} else if !errors.Is(err, ErrConflict) {
		h.log.Warn("导出 MCP 应用密钥失败", "slug", r.PathValue("slug"), "err", err)
	}
	platform.WriteJSON(w, http.StatusOK, out)
}

// jwtKey 返回 MCP 应用的签名密钥，供部署者配置到 MCP Server 侧验签。
//
// 空间 manage 只是门槛，真正的判据是归属：应用名绑定了创建者时，只有创建者能取。
// 否则任何空间管理员写一个 auth.app 指过来的草稿就能读到别人的生产密钥，
// 按创建者收口的规则等于绕过去了。没有归属记录的老数据仍按 manage 放行，
// 作为管理员兜底；发布时会补上归属，之后就走归属判定。
func (h *Handler) jwtKey(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "manage")
	if !ok {
		return
	}
	app, key, err := h.mgr.MCPAppKey(r.Context(), r.PathValue("id"), r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if owner, err := h.mgr.MCPAppOwner(r.Context(), app); err == nil && owner != user.ID {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden",
			"应用名 "+app+" 由创建者持有，密钥只能由创建者取回")
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"app": app, "key": key, "scope": mcp.MCPJWTScope, "algorithm": "HS256",
		"header": "Authorization", "ttl_seconds": int(mcp.UserTokenTTL.Seconds()),
		"claims": []string{"sub", "user_id", "email", "job_number", "iss", "aud", "iat", "exp"},
	})
}

// mcpAppKey 是会话里「MCP 应用密钥」卡片的数据源：按应用名生成或取回签名密钥。
//
// 权限按「创建者」收口，不是空间权限：空间 write 权限的人可以随手把 auth.app
// 写成别人的应用名，按空间放行等于把生产密钥交出去。除创建者外只回创建者姓名，
// 明文只在创建者这一支上出现，且不进模型上下文（模型只看到工具回执里的状态）。
func (h *Handler) mcpAppKey(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.guard(w, r, "read")
	if !ok {
		return
	}
	res, err := h.mgr.MCPAppKeyFor(r.Context(), user.ID, r.PathValue("id"), r.PathValue("app"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// 明文密钥不进任何缓存：浏览器回退/代理缓存都不该留住它
	w.Header().Set("Cache-Control", "no-store")
	body := map[string]any{
		"app": res.App, "status": res.Status, "owner_name": res.OwnerName,
		"scope": mcp.MCPJWTScope, "algorithm": "HS256", "header": "Authorization",
		"ttl_seconds": int(mcp.UserTokenTTL.Seconds()),
		"claims":      []string{"sub", "user_id", "email", "job_number", "iss", "aud", "iat", "exp"},
	}
	if res.Key != "" {
		body["key"] = res.Key
	}
	platform.WriteJSON(w, http.StatusOK, body)
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
	// Permissions 是清单声明的权限，安装前给用户看「装完它会拿到什么」。
	Permissions []string `json:"permissions,omitempty"`
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
		platform.WriteAuthError(w, r, err)
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
		var permissions []string
		if v, err := h.store.GetPluginVersion(r.Context(), p.ID, p.LatestVersion); err == nil {
			risk = v.Risk
			if mm, perr := Parse(v.Manifest, true); perr == nil {
				permissions = mm.Permissions()
			}
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
			Permissions: permissions,
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

// agentIcon 返回智能体头像。
//
// 头像在会话列表里每个会话都要渲染，所以允许浏览器缓存；地址里的 v 是文件
// 修改时间，换头像即换地址，不会出现「改完还是旧图」。
func (h *Handler) agentIcon(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "read"); !ok {
		return
	}
	data, contentType, err := h.mgr.AgentIcon(r.Context(), r.PathValue("id"), r.PathValue("slug"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=300, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// uploadAgentIcon 收头像原图：裁剪与压缩都在服务端再做一次，
// 前端算错或直接打接口都不会把大图落进空间。
func (h *Handler) uploadAgentIcon(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "write"); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentIconSourceBytes+(1<<20))
	if err := r.ParseMultipartForm(maxAgentIconSourceBytes); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请以 multipart/form-data 上传，字段名 icon")
		return
	}
	file, _, err := r.FormFile("icon")
	if err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "missing_icon", "缺少图片字段 icon")
		return
	}
	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(io.LimitReader(file, maxAgentIconSourceBytes+1))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	iconURL, err := h.mgr.SetAgentIcon(r.Context(), r.PathValue("id"), r.PathValue("slug"), raw)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"icon_url": iconURL})
}

func (h *Handler) removeAgentIcon(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, "write"); !ok {
		return
	}
	if err := h.mgr.ClearAgentIcon(r.Context(), r.PathValue("id"), r.PathValue("slug")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guard 校验空间权限，返回用户与实际权限。
func (h *Handler) guard(w http.ResponseWriter, r *http.Request, need string) (store.User, string, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteAuthError(w, r, err)
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
		"缺少 plugin.json", "体积", "头像",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
