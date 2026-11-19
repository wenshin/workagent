package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	sessionPrefix = "wa:sess:"
	userPrefix    = "wa:usess:"
	runPrefix     = "wa:run:"
	stateSuffix   = ":state"
	frameSuffix   = ":frames"
	// maxRedisFrames 与 session 包的内存上限一致：一个 token 一帧的长思考
	// 只靠 MAXLEN 裁剪就够，不需要额外计数。
	maxRedisFrames = 8192
)

// openRedis 建 Redis 客户端。Ping 一次：连不上就在启动时暴露，
// 而不是等第一个用户登录才发现。
func openRedis(url string, runTTL time.Duration) (Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL 不合法: %w", err)
	}
	if runTTL <= 0 {
		runTTL = 30 * time.Minute
	}
	c := &redisClient{rdb: redis.NewClient(opts), runTTL: runTTL}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		_ = c.rdb.Close()
		return nil, fmt.Errorf("连接 Redis 失败（%s）: %w", opts.Addr, err)
	}
	return c, nil
}

type redisClient struct {
	rdb    *redis.Client
	runTTL time.Duration
}

func (c *redisClient) Close() error { return c.rdb.Close() }

// wrap 把驱动错误统一成 ErrUnavailable：上层只认这一个哨兵值，
// 不把 redis 的包细节漏到 handler。
func wrap(err error) error {
	if err == nil || errors.Is(err, redis.Nil) {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func (c *redisClient) SetSession(ctx context.Context, tokenHash, userID string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	data, err := json.Marshal(Session{UserID: userID, ExpiresAt: time.Now().Add(ttl)})
	if err != nil {
		return err
	}
	key := sessionPrefix + tokenHash
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, key, data, ttl)
	// 反向索引供「按用户踢下线」：成员过期由 TTL 自然清理，先删也无害。
	pipe.SAdd(ctx, userPrefix+userID, tokenHash)
	pipe.Expire(ctx, userPrefix+userID, ttl)
	_, err = pipe.Exec(ctx)
	return wrap(err)
}

func (c *redisClient) GetSession(ctx context.Context, tokenHash string) (Session, bool, error) {
	data, err := c.rdb.Get(ctx, sessionPrefix+tokenHash).Bytes()
	if errors.Is(err, redis.Nil) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, wrap(err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		// 值坏了等同于没登录：让用户重新登录，比报 500 好
		return Session{}, false, nil
	}
	if !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt) {
		return Session{}, false, nil
	}
	return s, true, nil
}

func (c *redisClient) DeleteSession(ctx context.Context, tokenHash string) error {
	key := sessionPrefix + tokenHash
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		var s Session
		if json.Unmarshal(data, &s) == nil && s.UserID != "" {
			_ = c.rdb.SRem(ctx, userPrefix+s.UserID, tokenHash).Err()
		}
	} else if !errors.Is(err, redis.Nil) {
		return wrap(err)
	}
	return wrap(c.rdb.Del(ctx, key).Err())
}

func (c *redisClient) DeleteUserSessions(ctx context.Context, userID string) error {
	idxKey := userPrefix + userID
	hashes, err := c.rdb.SMembers(ctx, idxKey).Result()
	if err != nil {
		return wrap(err)
	}
	if len(hashes) > 0 {
		keys := make([]string, 0, len(hashes)+1)
		for _, h := range hashes {
			keys = append(keys, sessionPrefix+h)
		}
		keys = append(keys, idxKey)
		if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
			return wrap(err)
		}
		return nil
	}
	return wrap(c.rdb.Del(ctx, idxKey).Err())
}

func (c *redisClient) SaveRun(ctx context.Context, runID string, st RunState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	key := runPrefix + runID + stateSuffix
	done := "0"
	if st.Done {
		done = "1"
	}
	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key, map[string]any{
		"content":    st.Content,
		"reasoning":  st.Reasoning,
		"offset":     st.Offset,
		"dropped":    st.Dropped,
		"done":       done,
		"updated_at": st.UpdatedAt.Unix(),
	})
	pipe.Expire(ctx, key, c.runTTL)
	_, err := pipe.Exec(ctx)
	return wrap(err)
}

func (c *redisClient) AppendFrames(ctx context.Context, runID string, frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	key := runPrefix + runID + frameSuffix
	pipe := c.rdb.Pipeline()
	for _, f := range frames {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: key,
			MaxLen: maxRedisFrames,
			Approx: true,
			Values: map[string]any{"off": f.Offset, "ev": f.Payload},
		})
	}
	pipe.Expire(ctx, key, c.runTTL)
	_, err := pipe.Exec(ctx)
	return wrap(err)
}

func (c *redisClient) ReplayFrames(ctx context.Context, runID string, after int) ([]Frame, error) {
	key := runPrefix + runID + frameSuffix
	msgs, err := c.rdb.XRange(ctx, key, "-", "+").Result()
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]Frame, 0, len(msgs))
	for _, m := range msgs {
		off, ok := parseInt(m.Values["off"])
		if !ok || off <= after {
			continue
		}
		payload, _ := m.Values["ev"].(string)
		out = append(out, Frame{Offset: off, Payload: []byte(payload)})
	}
	return out, nil
}

func (c *redisClient) LoadRun(ctx context.Context, runID string) (RunState, bool, error) {
	key := runPrefix + runID + stateSuffix
	m, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return RunState{}, false, wrap(err)
	}
	if len(m) == 0 {
		return RunState{}, false, nil
	}
	st := RunState{
		Content:   m["content"],
		Reasoning: m["reasoning"],
		Done:      m["done"] == "1",
	}
	st.Offset, _ = parseInt(m["offset"])
	st.Dropped, _ = parseInt(m["dropped"])
	if ts, ok := parseInt(m["updated_at"]); ok {
		st.UpdatedAt = time.Unix(int64(ts), 0)
	}
	return st, true, nil
}

func (c *redisClient) DropRun(ctx context.Context, runID string) error {
	keys := []string{runPrefix + runID + stateSuffix, runPrefix + runID + frameSuffix}
	return wrap(c.rdb.Del(ctx, keys...).Err())
}

// parseInt 兼容字符串与数值两种存法：XADD/HSet 会把 int 存成字符串，
// 但不同客户端版本拿回来的类型不一致，这里统一收敛。
func parseInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}
