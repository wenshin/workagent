package cache

import (
	"context"
	"testing"
	"time"
)

// 登录态：写入即生效，登出立刻失效，按用户踢下线清掉全部会话。
func TestMemClientSessions(t *testing.T) {
	ctx := context.Background()
	c := newMemClient(time.Minute)

	if _, ok, _ := c.GetSession(ctx, "h1"); ok {
		t.Fatal("没写过就查到了会话")
	}
	if err := c.SetSession(ctx, "h1", "u1", time.Minute); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	sess, ok, err := c.GetSession(ctx, "h1")
	if err != nil || !ok || sess.UserID != "u1" {
		t.Fatalf("读回会话不对: ok=%v err=%v sess=%+v", ok, err, sess)
	}

	// 过期即视为没登录
	if err := c.SetSession(ctx, "h2", "u1", time.Millisecond); err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok, _ := c.GetSession(ctx, "h2"); ok {
		t.Fatal("过期会话应视为没登录")
	}

	if err := c.DeleteSession(ctx, "h1"); err != nil {
		t.Fatalf("登出失败: %v", err)
	}
	if _, ok, _ := c.GetSession(ctx, "h1"); ok {
		t.Fatal("登出后会话仍可读")
	}

	// 同一账号两个会话：踢下线要一起失效
	_ = c.SetSession(ctx, "a", "u2", time.Minute)
	_ = c.SetSession(ctx, "b", "u2", time.Minute)
	_ = c.SetSession(ctx, "c", "u3", time.Minute)
	if err := c.DeleteUserSessions(ctx, "u2"); err != nil {
		t.Fatalf("踢下线失败: %v", err)
	}
	for _, h := range []string{"a", "b"} {
		if _, ok, _ := c.GetSession(ctx, h); ok {
			t.Fatalf("会话 %s 应已失效", h)
		}
	}
	if _, ok, _ := c.GetSession(ctx, "c"); !ok {
		t.Fatal("不该误删其他用户的会话")
	}
}

// 流缓存：快照覆盖写、帧按 offset 回放、done 之后可读、drop 之后清空。
func TestMemClientRunStream(t *testing.T) {
	ctx := context.Background()
	c := newMemClient(time.Minute)
	const runID = "run-1"

	if err := c.AppendFrames(ctx, runID, []Frame{
		{Offset: 1, Payload: []byte(`{"type":"message.delta","delta":"a"}`)},
		{Offset: 2, Payload: []byte(`{"type":"message.delta","delta":"b"}`)},
	}); err != nil {
		t.Fatalf("追加帧失败: %v", err)
	}
	if err := c.SaveRun(ctx, runID, RunState{Content: "ab", Offset: 2}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	frames, err := c.ReplayFrames(ctx, runID, 1)
	if err != nil || len(frames) != 1 || frames[0].Offset != 2 {
		t.Fatalf("回放应只给 offset>1 的帧: %v %+v", err, frames)
	}
	st, ok, err := c.LoadRun(ctx, runID)
	if err != nil || !ok || st.Content != "ab" || st.Offset != 2 {
		t.Fatalf("快照不对: ok=%v err=%v %+v", ok, err, st)
	}

	// 覆盖写：后写的正文必须赢（旧快照晚到不能改结果）
	if err := c.SaveRun(ctx, runID, RunState{Content: "abc", Offset: 3, Done: true}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}
	st, _, _ = c.LoadRun(ctx, runID)
	if st.Content != "abc" || !st.Done {
		t.Fatalf("快照没覆盖成功: %+v", st)
	}

	if err := c.DropRun(ctx, runID); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, ok, _ := c.LoadRun(ctx, runID); ok {
		t.Fatal("drop 之后不应再读到快照")
	}
	if frames, _ := c.ReplayFrames(ctx, runID, 0); len(frames) != 0 {
		t.Fatal("drop 之后不应再读到帧")
	}
}

// 内存降级同样要裁剪帧：长思考不能把进程内存吃满。
func TestMemClientTrimsFrames(t *testing.T) {
	ctx := context.Background()
	c := newMemClient(time.Minute)
	frames := make([]Frame, 0, maxMemFrames+10)
	for i := 1; i <= maxMemFrames+10; i++ {
		frames = append(frames, Frame{Offset: i, Payload: []byte(`{}`)})
	}
	if err := c.AppendFrames(ctx, "r", frames); err != nil {
		t.Fatalf("追加帧失败: %v", err)
	}
	got, _ := c.ReplayFrames(ctx, "r", 0)
	if len(got) != maxMemFrames {
		t.Fatalf("应裁到 %d 帧，实际 %d", maxMemFrames, len(got))
	}
	if got[0].Offset != 11 {
		t.Fatalf("应保留最新的帧，实际首帧 offset=%d", got[0].Offset)
	}
}
