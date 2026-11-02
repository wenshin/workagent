package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("not found")

// User 是平台用户，与钉钉账号一一对应（以 union_id 为唯一键）。
type User struct {
	ID              string
	DingTalkUnionID string
	Name            string
	Avatar          string
	Mobile          string
	Status          string
	CreatedAt       time.Time
}

// DingTalkProfile 是钉钉返回的用户资料。
type DingTalkProfile struct {
	UnionID string
	OpenID  string
	Name    string
	Avatar  string
	Mobile  string
}

// Space 是空间（个人空间 / 共享空间），parent_id 为空表示根空间。
type Space struct {
	ID               string
	OwnerID          string
	Name             string
	Type             string // personal | shared
	ParentID         string
	Path             string
	Depth            int
	IsSystemReserved bool
	Status           string
	Perm             string // 当前用户在该空间的权限
	CreatedAt        time.Time
}

// SpaceInput 是创建空间的入参，ID 与 Path 由业务层生成。
type SpaceInput struct {
	ID       string
	OwnerID  string
	Name     string
	Type     string
	ParentID string
	Path     string
	Depth    int
}

// Session 是聊天会话，直接归属于某个空间。
type Session struct {
	ID            string
	SpaceID       string
	Title         string
	TitleAuto     bool // true 表示标题可被首条消息改写
	CreatedBy     string
	LastMessageAt *time.Time
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Message 是会话中的一条消息。
type Message struct {
	ID        string
	SessionID string
	Role      string // user | assistant | system
	Content   string
	CreatedAt time.Time
}

// AuthSession 是登录会话（注意：与聊天会话 session 表不是一回事）。
type AuthSession struct {
	TokenHash string
	UserID    string
	ExpiresAt time.Time
}

// Store 是持久化接口，业务层只依赖它。
type Store interface {
	Ping(ctx context.Context) error

	UpsertUser(ctx context.Context, p DingTalkProfile) (User, error)
	GetUser(ctx context.Context, id string) (User, error)

	CreateAuthSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error
	GetAuthSession(ctx context.Context, tokenHash string) (AuthSession, error)
	DeleteAuthSession(ctx context.Context, tokenHash string) error

	// EnsurePersonalSpace 保证用户拥有个人空间（含一个「新会话」），返回可见空间及是否本次创建。
	EnsurePersonalSpace(ctx context.Context, userID string) ([]Space, bool, error)
	ListSpaces(ctx context.Context, userID string) ([]Space, error)

	CreateSpace(ctx context.Context, in SpaceInput) (Space, error)
	GetSpace(ctx context.Context, id string) (Space, error)
	// SpacePerm 返回用户在某空间的权限，无权限返回空字符串。
	SpacePerm(ctx context.Context, spaceID, userID string) (string, error)
	RenameSpace(ctx context.Context, id, name string) error
	// DeleteSpaceTree 软删整棵子树，返回被删除的空间列表（业务层据此移入回收站）。
	DeleteSpaceTree(ctx context.Context, id string) ([]Space, error)

	CreateSession(ctx context.Context, spaceID, userID, title string) (Session, error)
	ListSessions(ctx context.Context, spaceID string) ([]Session, error)
	// ListUserSessions 返回用户在可见空间下的全部会话（左侧树一次性取数）。
	ListUserSessions(ctx context.Context, userID string) ([]Session, error)
	GetSession(ctx context.Context, id string) (Session, error)
	// RenameSession 修改标题；auto 为 true 表示仍允许被首条消息覆盖。
	RenameSession(ctx context.Context, id, title string, auto bool) error
	// DeleteSession 软删会话，历史消息保留。
	DeleteSession(ctx context.Context, id string) error
	// TouchSession 更新最近活动时间。
	TouchSession(ctx context.Context, id string) error

	CreateMessage(ctx context.Context, sessionID, role, content string) (Message, error)
	ListMessages(ctx context.Context, sessionID string) ([]Message, error)
	CountMessages(ctx context.Context, sessionID string) (int, error)
}
