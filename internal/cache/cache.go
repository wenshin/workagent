// Package cache 是运行时状态（登录态、正在跑的会话流）的缓存层。
//
// 这里只放「短命、可重建」的东西：登录会话丢了让用户重新登录，run 的增量帧
// 与正文快照在轮次结束时会整段落库并留到 TTL 到期。权威事实仍在 Postgres，
// 缓存层不做第二份长期数据，也不做跨库事务。
package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrUnavailable 表示缓存服务不可用。
//
// 调用方必须区分「没登录」与「缓存挂了」：前者 401 跳登录页，后者 503 让客户端
// 重试。混成一个，Redis 抖一下就会把所有人踢到登录页，而那里同样登不进去。
var ErrUnavailable = errors.New("缓存服务不可用")

// Session 是一条登录会话。
type Session struct {
	UserID    string    `json:"uid"`
	ExpiresAt time.Time `json:"exp"`
}

// Frame 是流缓冲里的一帧。Payload 是 agent.Event 的 JSON：
// 缓存层不解释事件内容，只负责按 offset 存取，避免和 agent 包互相依赖。
type Frame struct {
	Offset  int
	Payload []byte
}

// RunState 是一次执行在缓存里的正文快照。
type RunState struct {
	Content   string    `json:"content"`
	Reasoning string    `json:"reasoning"`
	Offset    int       `json:"offset"`
	Dropped   int       `json:"dropped"`
	Done      bool      `json:"done"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Client 是缓存层的全部能力。两种实现：redisClient（生产）与 memClient
// （未配置 REDIS_URL 时的降级，以及测试）。
type Client interface {
	// SetSession 写入登录会话，ttl 为剩余有效期。
	SetSession(ctx context.Context, tokenHash, userID string, ttl time.Duration) error
	// GetSession 读取会话；ok 为 false 表示没登录或已过期。
	GetSession(ctx context.Context, tokenHash string) (Session, bool, error)
	// DeleteSession 让一条会话立即失效（登出）。
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteUserSessions 删除某个用户的全部会话（禁用账号 / 踢下线）。
	DeleteUserSessions(ctx context.Context, userID string) error

	// SaveRun 覆盖一次执行的正文快照。
	SaveRun(ctx context.Context, runID string, st RunState) error
	// AppendFrames 追加事件帧，超出上限的旧帧由实现裁剪。
	AppendFrames(ctx context.Context, runID string, frames []Frame) error
	// ReplayFrames 返回 offset 之后的帧，按 offset 升序。
	ReplayFrames(ctx context.Context, runID string, after int) ([]Frame, error)
	// LoadRun 读取正文快照；ok 为 false 表示这条 run 不在缓存里。
	LoadRun(ctx context.Context, runID string) (RunState, bool, error)
	// DropRun 在轮次落库后清掉缓存。
	DropRun(ctx context.Context, runID string) error

	// Close 释放连接（内存实现为空操作）。
	Close() error
}

// Open 按地址建缓存：空地址退回进程内存。
//
// 内存实现只够单实例开发用——重启就掉登录、丢当前轮正文；生产必须配 REDIS_URL。
func Open(url string, runTTL time.Duration) (Client, error) {
	if url == "" {
		return newMemClient(runTTL), nil
	}
	return openRedis(url, runTTL)
}

// maxMemFrames 与 session 包的帧上限保持一致：内存降级时同样不该无限增长。
const maxMemFrames = 8192

// memClient 是进程内存实现，等待测试与「没配 Redis」的本地开发。
type memClient struct {
	mu     sync.Mutex
	runTTL time.Duration

	sessions map[string]memSession
	byUser   map[string]map[string]struct{}
	runs     map[string]*memRun
}

type memSession struct {
	sess      Session
	expiresAt time.Time
}

type memRun struct {
	state     RunState
	frames    []Frame
	expiresAt time.Time
}

func newMemClient(runTTL time.Duration) *memClient {
	if runTTL <= 0 {
		runTTL = 30 * time.Minute
	}
	return &memClient{
		runTTL:   runTTL,
		sessions: map[string]memSession{},
		byUser:   map[string]map[string]struct{}{},
		runs:     map[string]*memRun{},
	}
}

func (c *memClient) SetSession(_ context.Context, tokenHash, userID string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := time.Now().Add(ttl)
	c.sessions[tokenHash] = memSession{sess: Session{UserID: userID, ExpiresAt: exp}, expiresAt: exp}
	if c.byUser[userID] == nil {
		c.byUser[userID] = map[string]struct{}{}
	}
	c.byUser[userID][tokenHash] = struct{}{}
	return nil
}

func (c *memClient) GetSession(_ context.Context, tokenHash string) (Session, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[tokenHash]
	if !ok {
		return Session{}, false, nil
	}
	if time.Now().After(s.expiresAt) {
		delete(c.sessions, tokenHash)
		return Session{}, false, nil
	}
	return s.sess, true, nil
}

func (c *memClient) DeleteSession(_ context.Context, tokenHash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[tokenHash]
	if !ok {
		return nil
	}
	delete(c.sessions, tokenHash)
	if set := c.byUser[s.sess.UserID]; set != nil {
		delete(set, tokenHash)
		if len(set) == 0 {
			delete(c.byUser, s.sess.UserID)
		}
	}
	return nil
}

func (c *memClient) DeleteUserSessions(_ context.Context, userID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for hash := range c.byUser[userID] {
		delete(c.sessions, hash)
	}
	delete(c.byUser, userID)
	return nil
}

func (c *memClient) SaveRun(_ context.Context, runID string, st RunState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	if r == nil {
		r = &memRun{}
		c.runs[runID] = r
	}
	r.state = st
	r.expiresAt = time.Now().Add(c.runTTL)
	return nil
}

func (c *memClient) AppendFrames(_ context.Context, runID string, frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	if r == nil {
		r = &memRun{}
		c.runs[runID] = r
	}
	r.frames = append(r.frames, frames...)
	if len(r.frames) > maxMemFrames {
		r.frames = r.frames[len(r.frames)-maxMemFrames:]
	}
	r.expiresAt = time.Now().Add(c.runTTL)
	return nil
}

func (c *memClient) ReplayFrames(_ context.Context, runID string, after int) ([]Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	if r == nil || time.Now().After(r.expiresAt) {
		return nil, nil
	}
	out := make([]Frame, 0, len(r.frames))
	for _, f := range r.frames {
		if f.Offset > after {
			out = append(out, f)
		}
	}
	return out, nil
}

func (c *memClient) LoadRun(_ context.Context, runID string) (RunState, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	if r == nil || time.Now().After(r.expiresAt) {
		return RunState{}, false, nil
	}
	return r.state, true, nil
}

func (c *memClient) DropRun(_ context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.runs, runID)
	return nil
}

func (c *memClient) Close() error { return nil }
