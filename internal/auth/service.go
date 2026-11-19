package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/cache"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/identity"
	"github.com/workagent/auto-agent/internal/store"
)

// 身份相关错误直接复用 identity 包，保证 handler 能按 errors.Is 统一处理。
var (
	ErrNotConfigured   = identity.ErrNotConfigured
	ErrNotImplemented  = identity.ErrNotImplemented
	ErrInvalidRequest  = identity.ErrInvalidRequest
	ErrNoIdentity      = identity.ErrNoIdentity
	ErrInvalidState    = errors.New("登录 state 无效或已过期，请重新登录")
	ErrUnauthenticated = errors.New("未登录或登录已过期")
	ErrForbidden       = errors.New("账号已被禁用")
)

// Result 是一次成功登录的结果。
type Result struct {
	User      store.User
	Token     string
	ExpiresAt time.Time
	Spaces    []store.Space
	NewSpace  bool
}

// SystemSpaceEnsurer 保证系统管理员的「系统空间」就绪。
//
// 定义为接口而不是直接依赖 space 包，避免 auth 与 space 相互引用。
type SystemSpaceEnsurer interface {
	EnsureSystemSpace(ctx context.Context, user store.User) error
}

// Service 负责登录编排：state 校验、换取 token、落库、建空间、发会话。
type Service struct {
	store store.Store
	// sessions 是登录态的唯一存放处：Postgres 只存用户资料，不存会话（§5.1.1）。
	sessions cache.Client
	provider identity.Provider
	cfg      *config.Config
	spaces   SystemSpaceEnsurer
	log      *slog.Logger
	states   *stateStore
}

// NewService 构造登录服务。IdP 由调用方按配置注入，切换 IdP 不改这里。
func NewService(st store.Store, cfg *config.Config, idp identity.Provider, spaces SystemSpaceEnsurer, sessions cache.Client, log *slog.Logger) *Service {
	return &Service{
		store:    st,
		sessions: sessions,
		provider: idp,
		cfg:      cfg,
		spaces:   spaces,
		log:      log,
		states:   newStateStore(10 * time.Minute),
	}
}

// ProviderInfo 返回当前 IdP 信息，供启动日志与登录页展示。
func (s *Service) ProviderInfo() (name, display string, ready bool, missing []string) {
	missing = s.provider.Missing()
	if missing == nil {
		missing = []string{}
	}
	return s.provider.Name(), s.provider.DisplayName(), s.provider.Ready(), missing
}

// MissingConfig 返回当前 IdP 缺失的环境变量名。
func (s *Service) MissingConfig() []string {
	missing := s.provider.Missing()
	if missing == nil {
		return []string{}
	}
	return missing
}

// LoginURL 生成带 state 的授权地址。
//
// 未配置与未实现分别由 Provider 返回 ErrNotConfigured / ErrNotImplemented。
func (s *Service) LoginURL() (string, error) {
	state, err := s.states.Issue()
	if err != nil {
		return "", err
	}
	return s.provider.AuthorizeURL(state)
}

// CompleteLogin 用 code + state 完成登录。
func (s *Service) CompleteLogin(ctx context.Context, code, state string) (*Result, error) {
	if code == "" || state == "" {
		return nil, ErrInvalidRequest
	}
	if !s.states.Consume(state) {
		return nil, ErrInvalidState
	}

	cred, err := s.provider.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	profile, err := s.provider.Profile(ctx, cred)
	if err != nil {
		return nil, err
	}
	if profile.ProviderUID == "" {
		return nil, ErrNoIdentity
	}

	user, err := s.store.UpsertUserByIdentity(ctx, store.IdentityProfile{
		Provider:     profile.Provider,
		ProviderUID:  profile.ProviderUID,
		UnionID:      profile.UnionID,
		OpenID:       profile.OpenID,
		EmployeeID:   profile.EmployeeID,
		Name:         profile.Name,
		Avatar:       profile.Avatar,
		Mobile:       profile.Mobile,
		Email:        profile.Email,
		JobNumber:    profile.JobNumber,
		Title:        profile.Title,
		CorpName:     profile.CorpName,
		ContactError: profile.ContactError,
		Raw:          profile.Raw,
	})
	if err != nil {
		return nil, fmt.Errorf("写入用户失败: %w", err)
	}
	if user.Status != "active" {
		return nil, ErrForbidden
	}

	// 系统管理员：确保系统空间存在并加入成员。必须早于个人空间初始化，
	// 因为 EnsurePersonalSpace 末尾会重新列出可见空间。
	if s.spaces != nil {
		if err := s.spaces.EnsureSystemSpace(ctx, user); err != nil {
			s.log.Warn("初始化系统空间失败", "user_id", user.ID, "err", err)
		}
	}

	spaces, created, err := s.store.EnsurePersonalSpace(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("初始化个人空间失败: %w", err)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(s.cfg.SessionTTL)
	if err := s.sessions.SetSession(ctx, hashToken(token), user.ID, s.cfg.SessionTTL); err != nil {
		return nil, fmt.Errorf("创建登录会话失败: %w", err)
	}

	s.log.Info("login success",
		"user_id", user.ID,
		"name", user.Name,
		"space_initialized", created,
	)

	return &Result{User: user, Token: token, ExpiresAt: expiresAt, Spaces: spaces, NewSpace: created}, nil
}

// Authenticate 用 cookie 中的 token 还原用户。
//
// 会话只在 Redis 里：读不到就是没登录；读出错（Redis 挂了）原样往上抛，
// handler 据此返回 503 而不是 401——否则一次抖动会把所有人踢到登录页。
func (s *Service) Authenticate(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, ErrUnauthenticated
	}
	sess, ok, err := s.sessions.GetSession(ctx, hashToken(token))
	if err != nil {
		return store.User{}, err
	}
	if !ok {
		return store.User{}, ErrUnauthenticated
	}
	user, err := s.store.GetUser(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrUnauthenticated
		}
		return store.User{}, err
	}
	if user.Status != "active" {
		return store.User{}, ErrForbidden
	}
	return user, nil
}

// Logout 让会话立即失效。
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.sessions.DeleteSession(ctx, hashToken(token))
}

// KickUser 让某个账号的所有会话立即失效（禁用账号、强制下线）。
//
// 会话只在 Redis，没有「库里的会话行」需要同步；调用方在改完 app_user.status
// 之后调一次即可，不需要等 TTL 自然过期。
func (s *Service) KickUser(ctx context.Context, userID string) error {
	return s.sessions.DeleteUserSessions(ctx, userID)
}

// Spaces 返回用户可见空间。
func (s *Service) Spaces(ctx context.Context, userID string) ([]store.Space, error) {
	return s.store.ListSpaces(ctx, userID)
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// stateStore 在内存中保存待校验的 OAuth state（10 分钟有效）。
// 单实例部署足够；多副本需换成 Redis 或改为 HMAC 签名 state。
type stateStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func newStateStore(ttl time.Duration) *stateStore {
	return &stateStore{ttl: ttl, m: make(map[string]time.Time)}
}

func (s *stateStore) Issue() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	state := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[state] = now.Add(s.ttl)
	return state, nil
}

func (s *stateStore) Consume(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	exp, ok := s.m[state]
	if !ok {
		return false
	}
	delete(s.m, state)
	return time.Now().Before(exp)
}
