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

	"github.com/autohome/auto-agent/internal/config"
	"github.com/autohome/auto-agent/internal/store"
)

var (
	ErrNotConfigured  = errors.New("钉钉登录未配置")
	ErrInvalidRequest = errors.New("缺少授权码或 state")
	ErrInvalidState   = errors.New("登录 state 无效或已过期，请重新登录")
	ErrNoIdentity     = errors.New("未能获取钉钉身份标识（unionId/openId 均为空）")
	ErrUnauthenticated = errors.New("未登录或登录已过期")
	ErrForbidden      = errors.New("账号已被禁用")
)

// Result 是一次成功登录的结果。
type Result struct {
	User      store.User
	Token     string
	ExpiresAt time.Time
	Spaces    []store.Space
	NewSpace  bool
}

// Service 负责登录编排：state 校验、换取 token、落库、建空间、发会话。
type Service struct {
	store  store.Store
	client *DingTalkClient
	cfg    *config.Config
	log    *slog.Logger
	states *stateStore
}

// NewService 构造登录服务。
func NewService(st store.Store, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{
		store:  st,
		client: NewDingTalkClient(cfg.DingTalkClientID, cfg.DingTalkClientSecret, cfg.DingTalkRedirectURI),
		cfg:    cfg,
		log:    log,
		states: newStateStore(10 * time.Minute),
	}
}

// LoginURL 生成带 state 的授权地址。
func (s *Service) LoginURL() (string, error) {
	if !s.cfg.DingTalkReady() {
		return "", ErrNotConfigured
	}
	state, err := s.states.Issue()
	if err != nil {
		return "", err
	}
	return s.client.AuthorizeURL(state), nil
}

// CompleteLogin 用 code + state 完成登录。
func (s *Service) CompleteLogin(ctx context.Context, code, state string) (*Result, error) {
	if !s.cfg.DingTalkReady() {
		return nil, ErrNotConfigured
	}
	if code == "" || state == "" {
		return nil, ErrInvalidRequest
	}
	if !s.states.Consume(state) {
		return nil, ErrInvalidState
	}

	tok, err := s.client.ExchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	profile, err := s.client.UserProfile(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}

	unionID := profile.UnionID
	if unionID == "" {
		unionID = profile.OpenID
	}
	if unionID == "" {
		return nil, ErrNoIdentity
	}

	user, err := s.store.UpsertUser(ctx, store.DingTalkProfile{
		UnionID: unionID,
		OpenID:  profile.OpenID,
		Name:    profile.Nick,
		Avatar:  profile.AvatarURL,
		Mobile:  profile.Mobile,
	})
	if err != nil {
		return nil, fmt.Errorf("写入用户失败: %w", err)
	}
	if user.Status != "active" {
		return nil, ErrForbidden
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
	if err := s.store.CreateAuthSession(ctx, hashToken(token), user.ID, expiresAt); err != nil {
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
func (s *Service) Authenticate(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, ErrUnauthenticated
	}
	sess, err := s.store.GetAuthSession(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrUnauthenticated
		}
		return store.User{}, err
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
	return s.store.DeleteAuthSession(ctx, hashToken(token))
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
