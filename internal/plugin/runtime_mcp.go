package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// mcpRuntime 是一个插件化的 MCP 连接：按 manifest 的白名单暴露工具。
//
// 两种鉴权来源：
//   - static：初始化时取一次凭证存储里的值，之后每个请求都带同一个值；
//   - jwt：平台按用户身份现签，每次 tools/call 都换成一个 5 分钟的短令牌。
//
// 与 §5.3 的全局 MCP Gateway 不冲突：那是平台统一管理的连接，
// 这里是「某个空间自己装的一个 MCP 服务」，鉴权与出网域名都来自插件能力清单。
type mcpRuntime struct {
	client *mcp.Client
	tools  map[string]mcp.ToolDef // 模型可见名 → 定义
	remote map[string]string      // 模型可见名 → 远端工具名

	// 以下字段只在 jwt 模式下有值。
	app        string
	authHeader string
	signingKey string
	users      userReader
	// userTTL 来自清单 auth.token_ttl_seconds（默认 300s，上限 3600s）
	userTTL time.Duration

	// 服务令牌按需重签：实例长期驻留，签一次会在一小时后全线 401
	svcMu       sync.Mutex
	signService func() (string, error)
	svcSecret   string
	svcIssued   time.Time
}

// serviceSecret 返回当前可用的服务令牌，临近过期时重新签发。
// 重签失败时沿用旧令牌：让它自然过期被对端拒绝，也好过所有请求都带空鉴权。
func (r *mcpRuntime) serviceSecret() string {
	r.svcMu.Lock()
	defer r.svcMu.Unlock()

	if r.svcSecret != "" && time.Since(r.svcIssued) < mcp.ServiceTokenTTL-10*time.Minute {
		return r.svcSecret
	}
	token, err := r.signService()
	if err != nil {
		return r.svcSecret
	}
	r.svcSecret = "Bearer " + token
	r.svcIssued = time.Now()
	return r.svcSecret
}

// userReader 是签发用户令牌需要的最小用户目录。
type userReader interface {
	GetUser(ctx context.Context, id string) (store.User, error)
}

// secretWriter 是应用密钥的写入能力：Options.Secrets 只保证 Get。
type secretWriter interface {
	Enabled() bool
	Put(ctx context.Context, scope, name, value string) error
}

var (
	// errSecretsDisabled 明确指向部署原因：没有 KEK 就签不出可信令牌，
	// 此时宁可插件不可用，也不要退回无鉴权调用。
	errSecretsDisabled = errors.New("凭证存储未启用（未配置 SECRET_KEK），无法为 MCP 应用生成签名密钥")
	// appKeyMu 防止同一应用在多个空间并发装配时各自生成一把密钥。
	appKeyMu sync.Mutex
)

// newMCPRuntime 连接远端并同步工具清单。
//
// cachePath 是上次成功拉取的工具清单：远端抖动时沿用快照，至少让清单不消失
// （调用本身仍会失败，模型能看到工具暂时不可用，而不是整个插件凭空蒸发）。
func newMCPRuntime(ctx context.Context, meta Manifest, secrets Secrets,
	users userReader, log *slog.Logger, cachePath string) (*mcpRuntime, error) {
	if meta.Transport == nil {
		return nil, fmt.Errorf("MCP 插件缺少 transport")
	}

	rt := &mcpRuntime{
		tools:  map[string]mcp.ToolDef{},
		remote: map[string]string{},
		users:  users,
	}

	authHeader, secret := "", ""
	if meta.Auth != nil {
		authHeader = meta.Auth.HeaderName()
		if meta.Auth.JWT() {
			app := meta.Auth.AppName(meta.Slug)
			key, err := ensureAppKey(ctx, secrets, app)
			if err != nil {
				return nil, err
			}
			rt.app, rt.authHeader, rt.signingKey = app, authHeader, key
			rt.userTTL = meta.Auth.UserTokenTTL()

			// 握手与 tools/list 没有用户上下文：用应用自身的服务令牌。
			// 用户令牌只在 tools/call 时按调用者现签，两者共用一把密钥。
			//
			// 服务令牌只签一次不够：运行时实例是长期驻留的，1 小时后所有请求都会带
			// 过期令牌（§15.11 决定 6）。这里改成按需重签，提前 10 分钟换新。
			serviceToken, err := mcp.SignUserToken(key, app, mcp.UserClaims{
				Subject: "app:" + app,
				Act:     "platform",
			}, mcp.ServiceTokenTTL)
			if err != nil {
				return nil, err
			}
			secret = "Bearer " + serviceToken
			rt.signService = func() (string, error) {
				return mcp.SignUserToken(key, app, mcp.UserClaims{
					Subject: "app:" + app,
					Act:     "platform",
				}, mcp.ServiceTokenTTL)
			}
			rt.svcIssued = time.Now()
		} else if meta.Auth.Secret != "" && secrets != nil {
			v, err := secrets.Get(ctx, "plugin", meta.Auth.Secret)
			if err != nil {
				log.Warn("MCP 插件引用的密钥不存在", "slug", meta.Slug, "secret", meta.Auth.Secret, "err", err)
			} else {
				secret = v
			}
		}
	}

	client := mcp.NewClient(meta.Slug, meta.Transport.URL, authHeader, secret)
	if rt.signService != nil {
		// 静态 secret 只在首次握手时有效，之后每次请求都现签
		client.SetSecretProvider(rt.serviceSecret)
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := client.Initialize(callCtx); err != nil {
		return nil, err
	}
	defs, err := client.ListTools(callCtx)
	if err != nil {
		if cached, ok := readMCPCache(cachePath); ok {
			log.Warn("MCP 工具清单拉取失败，沿用上次快照", "slug", meta.Slug, "err", err)
			return buildMCPRuntime(client, meta, cached, rt)
		}
		return nil, err
	}
	writeMCPCache(cachePath, defs, log)
	return buildMCPRuntime(client, meta, defs, rt)
}

func buildMCPRuntime(client *mcp.Client, meta Manifest, defs []mcp.ToolDef, rt *mcpRuntime) (*mcpRuntime, error) {
	rt.client = client
	for _, def := range defs {
		// 白名单支持尾部 `*`：远端工具随时会加，逐个列全不现实；deny 仍然优先。
		if !meta.AllowsTool(def.Name) {
			continue
		}
		rt.tools[def.Name] = def
		rt.remote[def.Name] = def.Name
	}
	if len(rt.tools) == 0 {
		return nil, fmt.Errorf("MCP 服务没有暴露白名单内的工具（tools.allow 与实际清单不匹配）")
	}
	return rt, nil
}

// ensureAppKey 取得（必要时生成）一个应用的签名密钥。
//
// 幂等：已存在就复用，重新发布同一个应用不会让已经在用的 Server 突然验签失败。
func ensureAppKey(ctx context.Context, secrets Secrets, app string) (string, error) {
	if secrets == nil {
		return "", errSecretsDisabled
	}
	if v, err := secrets.Get(ctx, mcp.MCPJWTScope, app); err == nil && v != "" {
		return v, nil
	}

	appKeyMu.Lock()
	defer appKeyMu.Unlock()
	// 双检：并发的另一次装配可能已经写好了。
	if v, err := secrets.Get(ctx, mcp.MCPJWTScope, app); err == nil && v != "" {
		return v, nil
	}

	w, ok := secrets.(secretWriter)
	if !ok || !w.Enabled() {
		return "", errSecretsDisabled
	}
	key, err := mcp.NewSigningKey()
	if err != nil {
		return "", err
	}
	if err := w.Put(ctx, mcp.MCPJWTScope, app, key); err != nil {
		return "", fmt.Errorf("保存 MCP 应用签名密钥失败: %w", err)
	}
	return key, nil
}

func readMCPCache(cachePath string) ([]mcp.ToolDef, bool) {
	if cachePath == "" {
		return nil, false
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	var defs []mcp.ToolDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, false
	}
	return defs, len(defs) > 0
}

func writeMCPCache(cachePath string, defs []mcp.ToolDef, log *slog.Logger) {
	if cachePath == "" {
		return
	}
	raw, err := json.Marshal(defs)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		log.Warn("写入 MCP 工具快照失败", "err", err)
		return
	}
	if err := os.WriteFile(cachePath, raw, 0o600); err != nil {
		log.Warn("写入 MCP 工具快照失败", "err", err)
	}
}

func (r *mcpRuntime) names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// jwtMode 判断是否由平台签发用户令牌。
func (r *mcpRuntime) jwtMode() bool { return r.signingKey != "" }

// call 执行一次远端调用。jwt 模式下按当前调用者现签令牌。
func (r *mcpRuntime) call(ctx context.Context, remote string, args json.RawMessage, env tool.Env) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if !r.jwtMode() {
		return r.client.CallTool(callCtx, remote, args)
	}
	token, err := r.userToken(callCtx, env)
	if err != nil {
		return "", err
	}
	return r.client.CallToolWithAuth(callCtx, remote, r.authHeader, "Bearer "+token, args)
}

// userToken 为一次调用签出用户令牌。
//
// 中间不落库、不缓存：令牌 5 分钟过期，复用会让「谁在什么时候调了什么」
// 变得不可追溯，也会让撤销用户权限要等令牌自然过期才生效。
func (r *mcpRuntime) userToken(ctx context.Context, env tool.Env) (string, error) {
	if env.UserID == "" {
		return "", errors.New("缺少调用者身份：MCP 应用令牌必须绑定用户")
	}
	if r.users == nil {
		return "", errors.New("MCP 令牌签发缺少用户目录")
	}
	u, err := r.users.GetUser(ctx, env.UserID)
	if err != nil {
		return "", fmt.Errorf("取用户信息失败，无法签发 MCP 令牌: %w", err)
	}
	return mcp.SignUserToken(r.signingKey, r.app, mcp.UserClaims{
		Subject: u.ID,
		// 主体跟着 sub 走：sub 已经能唯一标识用户，重复填 user_id 只是冗余
		UserID:    u.ID,
		Email:     u.Email,
		JobNumber: u.JobNumber,
	}, r.userTTL)
}
