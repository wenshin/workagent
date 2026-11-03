// Package identity 是身份接入层（技术方案 §5.1.1 IdentityProvider）。
//
// 登录与用户资料获取统一收口在这里，切换 IdP（钉钉 / 企业微信 / 飞书）只改
// IDENTITY_PROVIDER，业务层不感知协议差异。
package identity

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/workagent/auto-agent/internal/config"
)

var (
	// ErrNotConfigured 表示当前 IdP 的凭证未配置齐。
	ErrNotConfigured = errors.New("身份接入未配置")
	// ErrNotImplemented 表示该 IdP 只有占位实现。
	ErrNotImplemented = errors.New("该身份接入方尚未实现")
	// ErrInvalidRequest 表示回调缺少授权码或 state。
	ErrInvalidRequest = errors.New("缺少授权码或 state")
	// ErrNoIdentity 表示未能拿到可用的唯一身份标识。
	ErrNoIdentity = errors.New("未能获取身份唯一标识")
)

// Credential 是授权码换到的访问凭证，字段按协议族留通用位。
type Credential struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	CorpID       string
}

// Profile 是跨 IdP 的统一用户资料，落库见 store.IdentityProfile。
type Profile struct {
	Provider    string // dingtalk | wecom | feishu
	ProviderUID string // 唯一键之一，钉钉为 unionId
	UnionID     string
	OpenID      string
	EmployeeID  string // 钉钉 userid / 企微 userid / 飞书 user_id
	Name        string
	Avatar      string
	Mobile      string
	Email       string
	JobNumber   string // 工号
	Title       string
	DeptIDs     []string
	CorpName    string // 企业名称，用于顶栏展示
	// ContactError 是通讯录同步失败的用户可读原因（成功时为空），
	// 会随 /auth/me 的 profile_warning 返回，避免权限缺失时静默为空。
	ContactError string
	Raw          []byte // 通讯录原始返回，便于排查
}

// Provider 是 IdP 适配接口，新增 IdP 只需实现它并在 New 中注册。
type Provider interface {
	// Name 是配置值，例如 dingtalk。
	Name() string
	// DisplayName 用于登录页与错误提示，例如 钉钉。
	DisplayName() string
	// Ready 表示凭证是否配置齐。
	Ready() bool
	// Missing 返回缺失的环境变量名，用于启动告警与登录页提示。
	Missing() []string
	// AuthorizeURL 生成授权地址；未配置返回 ErrNotConfigured，未实现返回 ErrNotImplemented。
	AuthorizeURL(state string) (string, error)
	// Exchange 用授权码换取访问凭证。
	Exchange(ctx context.Context, code string) (Credential, error)
	// Profile 拉取用户资料；通讯录类字段允许为空（权限缺失时降级）。
	Profile(ctx context.Context, cred Credential) (Profile, error)
}

// New 按 IDENTITY_PROVIDER 选择实现。未实现的 IdP 返回占位实现：
// 服务照常启动，登录接口返回 ErrNotImplemented，便于后续平滑接入。
func New(cfg *config.Config, log *slog.Logger) Provider {
	switch strings.ToLower(strings.TrimSpace(cfg.IdentityProvider)) {
	case "", "dingtalk":
		return newDingTalk(cfg, log)
	case "wecom":
		return &unimplemented{name: "wecom", display: "企业微信"}
	case "feishu":
		return &unimplemented{name: "feishu", display: "飞书"}
	default:
		name := strings.TrimSpace(cfg.IdentityProvider)
		return &unimplemented{name: name, display: name}
	}
}

// unimplemented 是企微/飞书的占位实现：接口齐全但不做任何外部调用。
type unimplemented struct {
	name    string
	display string
}

func (u *unimplemented) Name() string        { return u.name }
func (u *unimplemented) DisplayName() string { return u.display }
func (u *unimplemented) Ready() bool         { return false }
func (u *unimplemented) Missing() []string   { return nil }

func (u *unimplemented) AuthorizeURL(string) (string, error) {
	return "", ErrNotImplemented
}

func (u *unimplemented) Exchange(context.Context, string) (Credential, error) {
	return Credential{}, ErrNotImplemented
}

func (u *unimplemented) Profile(context.Context, Credential) (Profile, error) {
	return Profile{}, ErrNotImplemented
}
