package session

import (
	"context"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
)

func delta(s string) agent.Event {
	return agent.Event{Type: agent.EventMessageDelta, Delta: s}
}

// 刷新后从 after_offset 续上：只补发客户端没收到的帧，再跟着直播走。
func TestLiveRunReplayAfterOffset(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("u1", cancel)
	l.broadcast(delta("你好")) // offset 2
	l.broadcast(delta("世界")) // offset 4

	feed := l.subscribe(2)
	defer feed.Close()
	if !feed.Live {
		t.Fatal("run 仍在执行，订阅应当是直播")
	}
	if len(feed.Backlog) != 1 || feed.Backlog[0].Offset != 4 {
		t.Fatalf("应从 offset 2 之后补发，实际 %+v", feed.Backlog)
	}

	l.broadcast(delta("!")) // offset 5
	select {
	case f := <-feed.Frames:
		if f.Offset != 5 {
			t.Fatalf("直播帧 offset = %d，期望 5", f.Offset)
		}
	case <-time.After(time.Second):
		t.Fatal("订阅没有收到增量")
	}

	l.finish()
	select {
	case <-feed.Done:
	case <-time.After(time.Second):
		t.Fatal("run 结束后订阅应当关闭")
	}

	// 结束后再订阅：只能拿缓冲，且明确告知不在直播
	tail := l.subscribe(0)
	defer tail.Close()
	if tail.Live {
		t.Fatal("已结束的 run 不该再是直播")
	}
	if len(tail.Backlog) != 3 {
		t.Fatalf("缓冲应有 3 帧，实际 %d", len(tail.Backlog))
	}
}

// 显式取消要记下原因并真的掐断 run，且只生效一次。
func TestLiveRunMarkCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("u1", cancel)
	if !l.markCancelled(ReasonUserCancelled) {
		t.Fatal("第一次取消应当成功")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("取消没有作用到 run 的 ctx")
	}
	if got := l.cancelledReason(); got != ReasonUserCancelled {
		t.Fatalf("取消原因 = %q", got)
	}
	if l.markCancelled(ReasonUserCancelled) {
		t.Fatal("已取消的 run 不该被再次取消")
	}

	l.finish()
	if l.markCancelled(ReasonUserCancelled) {
		t.Fatal("已结束的 run 不该还能取消")
	}
}
