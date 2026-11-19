package cache

import (
	"context"
	"os"
	"testing"
	"time"
)

// 真 Redis 的集成测试：只有配了 REDIS_URL 才跑，普通单测不受环境限制。
func newTestRedis(t *testing.T) Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("未配置 REDIS_URL，跳过 Redis 集成测试")
	}
	c, err := Open(url, time.Minute)
	if err != nil {
		t.Fatalf("连接 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRedisSessions(t *testing.T) {
	ctx := context.Background()
	c := newTestRedis(t)

	hash := "test-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { _ = c.DeleteSession(ctx, hash) })

	if err := c.SetSession(ctx, hash, "u1", time.Minute); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	sess, ok, err := c.GetSession(ctx, hash)
	if err != nil || !ok || sess.UserID != "u1" {
		t.Fatalf("读回会话不对: ok=%v err=%v sess=%+v", ok, err, sess)
	}
	if err := c.DeleteSession(ctx, hash); err != nil {
		t.Fatalf("登出失败: %v", err)
	}
	if _, ok, _ := c.GetSession(ctx, hash); ok {
		t.Fatal("登出后会话仍可读")
	}

	// 反向索引：按用户删
	uid := "u-" + hash
	_ = c.SetSession(ctx, hash+"-a", uid, time.Minute)
	_ = c.SetSession(ctx, hash+"-b", uid, time.Minute)
	if err := c.DeleteUserSessions(ctx, uid); err != nil {
		t.Fatalf("踢下线失败: %v", err)
	}
	for _, h := range []string{hash + "-a", hash + "-b"} {
		if _, ok, _ := c.GetSession(ctx, h); ok {
			t.Fatalf("会话 %s 应已失效", h)
		}
	}
}

func TestRedisRunStream(t *testing.T) {
	ctx := context.Background()
	c := newTestRedis(t)
	runID := "test-run-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() { _ = c.DropRun(ctx, runID) })

	// 工具事件与增量混在一起：回放要按 offset 截断，且不能丢掉同 offset 的非增量帧
	if err := c.AppendFrames(ctx, runID, []Frame{
		{Offset: 2, Payload: []byte(`{"type":"message.delta","delta":"你好"}`)},
		{Offset: 2, Payload: []byte(`{"type":"tool.call","tool":{"id":"c1"}}`)},
	}); err != nil {
		t.Fatalf("追加帧失败: %v", err)
	}
	if err := c.SaveRun(ctx, runID, RunState{Content: "你好", Offset: 2, Done: true}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	all, err := c.ReplayFrames(ctx, runID, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("应回放 2 帧: %v %+v", err, all)
	}
	if all[1].Payload == nil || len(all[1].Payload) == 0 {
		t.Fatal("事件负载丢了")
	}

	st, ok, err := c.LoadRun(ctx, runID)
	if err != nil || !ok || st.Content != "你好" || !st.Done {
		t.Fatalf("快照不对: ok=%v err=%v %+v", ok, err, st)
	}

	if err := c.DropRun(ctx, runID); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, ok, _ := c.LoadRun(ctx, runID); ok {
		t.Fatal("drop 之后不应再读到快照")
	}
}
