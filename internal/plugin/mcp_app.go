package plugin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/store"
)

// MCP 应用密钥的取数结论。
//
// 前端按 status 分支渲染，模型只拿到状态；key 只在「创建者本人」这一支上有值，
// 且只出现在针对浏览器的响应里，不经过模型上下文。
const (
	// MCPAppCreated 应用名此前不存在：本次生成密钥并绑定到当前用户。
	MCPAppCreated = "created"
	// MCPAppOwned 应用名存在且是当前用户创建的：直接取回同一把密钥。
	MCPAppOwned = "owned"
	// MCPAppTaken 应用名被别人创建：只回创建者姓名，引导线下要。
	MCPAppTaken = "taken"
	// MCPAppUnowned 密钥已存在但没有归属记录（历史数据）：找管理员，不认领。
	MCPAppUnowned = "unowned"
	// MCPAppDisabled 凭证存储未启用（没配 SECRET_KEK）。
	MCPAppDisabled = "disabled"
)

// MCPAppKeyResult 是「应用密钥卡片」的取数结果。
type MCPAppKeyResult struct {
	App       string `json:"app"`
	Status    string `json:"status"`
	Key       string `json:"key,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
}

// MCPAppKeyFor 按应用名生成或取回 MCP 的 JWT 签名密钥。
//
// 应用名全局唯一、只对应一把密钥，所以「谁能看到明文」必须按创建者判定，
// 而不是按空间权限：空间 write 权限可以随时把 auth.app 改成别人的应用名，
// 按空间放行等于把生产密钥交出去。
//
// 只有三种情况会返回 key：
//   - 应用名此前不存在（created）：这次生成，创建者就是调用者；
//   - 应用名由调用者创建（owned）；
//   - 应用名的密钥不存在但归属记录在（owned）：按归属重建。
//
// 其余情况（taken / unowned）一律不带 key，只说明该找谁。
func (m *Manager) MCPAppKeyFor(ctx context.Context, userID, spaceID, app string) (MCPAppKeyResult, error) {
	app = strings.TrimSpace(app)
	if !appNameRe.MatchString(app) {
		return MCPAppKeyResult{}, fmt.Errorf("应用名只能是字母、数字、下划线与短横线（1-64 位）：%q", app)
	}
	out := MCPAppKeyResult{App: app}
	if m.secretsUnavailable() {
		// 存储没开就什么也签不出来：明确告知，不要伪造一把不落库的密钥
		out.Status = MCPAppDisabled
		return out, nil
	}

	rec, err := m.o.Store.GetMCPApp(ctx, app)
	switch {
	case err == nil:
		if rec.OwnerUserID != userID {
			out.Status = MCPAppTaken
			out.OwnerName = m.userDisplayName(ctx, rec.OwnerUserID)
			return out, nil
		}
		out.Status = MCPAppOwned

	case errors.Is(err, store.ErrNotFound):
		// 有密钥却没有归属记录：说明它是这条流程之前生成的（发布时自动生成的那批）。
		// 这时不能认领——认领等于把一把已被别人使用的密钥交到当前用户名下。
		if v, kerr := m.o.Secrets.Get(ctx, mcp.MCPJWTScope, app); kerr == nil && v != "" {
			out.Status = MCPAppUnowned
			return out, nil
		}
		claimed, created, cerr := m.o.Store.ClaimMCPApp(ctx, app, userID, spaceID)
		if cerr != nil {
			return MCPAppKeyResult{}, cerr
		}
		// 并发抢占：别人在我们生成密钥之前占住了这个名字，按「已存在」处理，
		// 不能把自己那把新密钥返回去，否则两边各拿一把、Server 侧必然验签失败。
		if claimed.OwnerUserID != userID {
			out.Status = MCPAppTaken
			out.OwnerName = m.userDisplayName(ctx, claimed.OwnerUserID)
			return out, nil
		}
		if created {
			out.Status = MCPAppCreated
		} else {
			out.Status = MCPAppOwned
		}

	default:
		return MCPAppKeyResult{}, err
	}

	key, err := ensureAppKey(ctx, m.o.Secrets, app)
	if err != nil {
		return MCPAppKeyResult{}, err
	}
	out.Key = key

	// 审计只记「谁在什么时候取了哪个应用的密钥」，值不落审计。
	action := "mcp_app.key.view"
	if out.Status == MCPAppCreated {
		action = "mcp_app.create"
	}
	m.recordAudit(ctx, userID, action, app, out.Status, map[string]any{"space": spaceID})
	return out, nil
}

// MCPAppOwner 返回应用名的创建者 ID；没有归属记录（老数据）时返回空串。
func (m *Manager) MCPAppOwner(ctx context.Context, app string) (string, error) {
	rec, err := m.o.Store.GetMCPApp(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rec.OwnerUserID, nil
}

// confirmAppKey 确认发布者确实持有这个应用名的签名密钥。
//
// 归属记录是首选证据：应用名由当前用户创建就不必再输一遍（他已经在卡片上拿过）。
// 应用属于别人、或属于没有归属记录的老数据时，必须出示密钥——发布是「把这个插件
// 绑死到某个应用名」的动作，拿得出密钥说明得到了创建者的配合；拿不出来就拒绝，
// 否则发布响应会直接把别人的生产密钥交出去，归属也就形同虚设。
func (m *Manager) confirmAppKey(ctx context.Context, userID, spaceID, app, input string) error {
	if m.secretsUnavailable() {
		return errSecretsDisabled
	}
	existing, kerr := m.o.Secrets.Get(ctx, mcp.MCPJWTScope, app)

	rec, rerr := m.o.Store.GetMCPApp(ctx, app)
	switch {
	case rerr == nil && rec.OwnerUserID == userID:
		return nil

	case rerr == nil:
		if !sameKey(existing, input) {
			owner := m.userDisplayName(ctx, rec.OwnerUserID)
			if owner == "" {
				owner = "其他成员"
			}
			return fmt.Errorf("应用名 %s 由 %s 创建，发布前需要在「发布」里填写它的签名密钥以确认归属：%w", app, owner, ErrForbidden)
		}
		return nil

	case errors.Is(rerr, store.ErrNotFound):
		// 老数据：密钥已经在用但没有创建人记录，出示密钥即可接管并补上归属
		if kerr == nil && strings.TrimSpace(existing) != "" {
			if !sameKey(existing, input) {
				return fmt.Errorf("应用名 %s 已在使用，发布前需要填写它的签名密钥确认归属（找原来的创建者或系统管理员）：%w", app, ErrForbidden)
			}
			if _, _, err := m.o.Store.ClaimMCPApp(ctx, app, userID, spaceID); err != nil {
				return err
			}
			return nil
		}
		// 全新应用名：发布时生成，没有可比对的密钥，也就无需确认
		return nil

	default:
		return rerr
	}
}

// sameKey 用常量时间比较：密钥比对不该给出「差几位」这类可测的时序差异。
func sameKey(existing, input string) bool {
	input = strings.TrimSpace(input)
	if existing == "" || input == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(existing), []byte(input)) == 1
}

// userDisplayName 取创建者姓名，取不到就留空（前端会退化成「其他成员」）。
func (m *Manager) userDisplayName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	u, err := m.o.Store.GetUser(ctx, userID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Name)
}

// ErrMCPAppKeyMissing 应用名有归属记录，但凭证存储里没有对应密钥。
// 与「应用名不存在」区分开：管理员点开列表不该顺手生成一把新密钥。
var ErrMCPAppKeyMissing = errors.New("该应用还没有签名密钥")

// ErrMCPAppName 应用名不合规（它会直接作为凭证存储里的键）。
var ErrMCPAppName = errors.New("应用名只能包含字母、数字、下划线与短横线，且不超过 64 个字符")

// MCPAppInfo 是管理员视角的一条应用密钥，默认不含明文。
//
// 列表页一次拉全部应用：把密钥一起下发等于把生产密钥写进浏览器缓存、代理日志
// 与 HAR 文件里。明文只在用户点「查看」时单独取一次（RevealMCPAppKey）。
type MCPAppInfo struct {
	App        string    `json:"app"`
	OwnerID    string    `json:"owner_id,omitempty"`
	OwnerName  string    `json:"owner_name,omitempty"`
	OwnerSpace string    `json:"owner_space_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Fingerprint 与 UpdatedAt 来自凭证存储；HasKey=false 时为空。
	Fingerprint string    `json:"fingerprint,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	HasKey      bool      `json:"has_key"`
	// Legacy 表示有密钥但没有归属记录（早于 mcp_app 流程生成的），
	// 只能由系统管理员兜底，界面上要标出来。
	Legacy bool `json:"legacy"`
}

// secretStatusLister 是凭证存储的「列指纹」侧能力（secret.FileStore 实现）。
// 只需要元信息，不解密：列表刷新不该把全部密钥解一遍。
type secretStatusLister interface {
	Status(ctx context.Context, scope string) ([]secret.Item, error)
}

// ListMCPApps 列出全部 MCP 应用及其密钥指纹（管理员入口，不含明文）。
//
// 数据必须合并两处：归属表 mcp_app 回答「谁创建的」，凭证存储 mcp_jwt 回答
// 「有没有密钥、指纹是什么」。只看其中一处都会漏——历史密钥没有归属记录，
// 归属记录也可能因为生成失败而没有密钥。
func (m *Manager) ListMCPApps(ctx context.Context) ([]MCPAppInfo, error) {
	recs, err := m.o.Store.ListMCPApps(ctx)
	if err != nil {
		return nil, err
	}
	byApp := make(map[string]MCPAppInfo, len(recs))
	for _, rec := range recs {
		byApp[rec.App] = MCPAppInfo{
			App:        rec.App,
			OwnerID:    rec.OwnerUserID,
			OwnerName:  m.userDisplayName(ctx, rec.OwnerUserID),
			OwnerSpace: rec.OwnerSpaceID,
			CreatedAt:  rec.CreatedAt,
		}
	}

	// 指纹与更新时间以凭证存储为准；存储没启用时列表只剩归属信息。
	if m.o.Secrets != nil && !m.secretsUnavailable() {
		if lister, ok := m.o.Secrets.(secretStatusLister); ok {
			items, serr := lister.Status(ctx, mcp.MCPJWTScope)
			if serr != nil {
				if m.o.Logger != nil {
					m.o.Logger.Warn("列举 MCP 应用密钥失败", "err", serr)
				}
			} else {
				for _, it := range items {
					info, seen := byApp[it.Name]
					if !seen {
						info = MCPAppInfo{App: it.Name, Legacy: true}
					}
					info.HasKey = true
					info.Fingerprint = it.Fingerprint
					info.UpdatedAt = it.UpdatedAt
					byApp[it.Name] = info
				}
			}
		}
	}

	out := make([]MCPAppInfo, 0, len(byApp))
	for _, info := range byApp {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

// RevealMCPAppKey 取回任意应用的签名密钥明文（仅系统管理员入口）。
//
// 与 MCPAppKeyFor 的差别只在「谁能看」：那边按创建者收口，这条是平台兜底通道
// （创建人离职、老数据没有归属时只能靠它）。所以这里**只读不建**：缺密钥就报错，
// 管理员点开列表不该改变任何应用的密钥，更不该把「看过」变成「换过」。
func (m *Manager) RevealMCPAppKey(ctx context.Context, actor, app string) (MCPAppInfo, string, error) {
	app = strings.TrimSpace(app)
	if !appNameRe.MatchString(app) {
		return MCPAppInfo{}, "", fmt.Errorf("%w：%q", ErrMCPAppName, app)
	}
	if m.secretsUnavailable() {
		return MCPAppInfo{}, "", errSecretsDisabled
	}

	key, err := m.o.Secrets.Get(ctx, mcp.MCPJWTScope, app)
	if errors.Is(err, secret.ErrNotFound) || (err == nil && key == "") {
		return MCPAppInfo{}, "", ErrMCPAppKeyMissing
	}
	if err != nil {
		return MCPAppInfo{}, "", err
	}

	info := MCPAppInfo{App: app, HasKey: true, Fingerprint: secret.Fingerprint(key)}
	if rec, rerr := m.o.Store.GetMCPApp(ctx, app); rerr == nil {
		info.OwnerID = rec.OwnerUserID
		info.OwnerName = m.userDisplayName(ctx, rec.OwnerUserID)
		info.OwnerSpace = rec.OwnerSpaceID
		info.CreatedAt = rec.CreatedAt
	} else {
		info.Legacy = true
	}

	// 审计只记「谁在什么时候看了哪个应用的密钥」，值不落审计。
	m.recordAudit(ctx, actor, "mcp_app.key.admin_view", app, "ok", nil)
	return info, key, nil
}
