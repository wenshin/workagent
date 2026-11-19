package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/cache"
)

func delta(s string) agent.Event {
	return agent.Event{Type: agent.EventMessageDelta, Delta: s}
}

// 刷新后从 after_offset 续上：只补发客户端没收到的帧，再跟着直播走。
func TestLiveRunReplayAfterOffset(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("r1", "u1", cancel, nil, nil)
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

// 长思考的碎片增量不能把开头挤掉：超帧数上限时先合并，文本一字不丢，
// 客户端 after_offset=0 重连仍能重建完整回答。
func TestLiveRunCompactsFramesWithoutLosingText(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("r1", "u1", cancel, nil, nil)
	var want strings.Builder
	for i := 0; i < maxLiveFrames+500; i++ {
		want.WriteString("想")
		l.broadcast(delta("想"))
	}

	if len(l.frames) > maxLiveFrames {
		t.Fatalf("帧数应被压回上限内，实际 %d", len(l.frames))
	}
	feed := l.subscribe(0)
	defer feed.Close()
	var got strings.Builder
	for _, f := range feed.Backlog {
		got.WriteString(f.Event.Delta)
	}
	if got.String() != want.String() {
		t.Fatalf("缓冲应保留完整文本：得到 %d 字符，期望 %d",
			utf8.RuneCountInString(got.String()), utf8.RuneCountInString(want.String()))
	}
}

// 文本真的超过上限才丢前缀，并记下丢到哪个 offset 供排查。
func TestLiveRunDropsPrefixOverByteCap(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("r1", "u1", cancel, nil, nil)
	chunk := strings.Repeat("x", 64<<10)
	for round := 0; round < 20; round++ { // 1.25MB > 1MB
		l.broadcast(delta(chunk))
	}
	if l.bytes > maxLiveBytes {
		t.Fatalf("缓冲文本应被裁到上限内，实际 %d 字节", l.bytes)
	}
	if l.dropped == 0 {
		t.Fatal("丢前缀时应记录 dropped offset")
	}
}

// 显式取消要记下原因并真的掐断 run，且只生效一次。
func TestLiveRunMarkCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := newLiveRun("r1", "u1", cancel, nil, nil)
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

// fakeSink 记录推给缓存的快照与帧，用来验证「正文真的写出去了」。
type fakeSink struct {
	mu     sync.Mutex
	states []cache.RunState
	frames []cache.Frame
}

func (f *fakeSink) SaveRun(_ context.Context, _ string, st cache.RunState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, st)
	return nil
}

func (f *fakeSink) AppendFrames(_ context.Context, _ string, frames []cache.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frames...)
	return nil
}

func (f *fakeSink) last() cache.RunState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.states) == 0 {
		return cache.RunState{}
	}
	return f.states[len(f.states)-1]
}

// 强推一次之后，缓存里就有完整正文与事件帧；结束时最后一份快照带 done。
func TestLiveRunPushesSnapshotToCache(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeSink{}
	l := newLiveRun("r1", "u1", cancel, sink, slog.Default())
	l.broadcast(agent.Event{Type: agent.EventMessageDelta, Reasoning: true, Delta: "想一想"})
	l.broadcast(delta("你好"))
	l.flush(true)

	st := sink.last()
	if st.Content != "你好" || st.Reasoning != "想一想" {
		t.Fatalf("快照正文不对: %+v", st)
	}
	// offset 是「思考 + 正文」的累计字符数：3 + 2
	if st.Offset != 5 {
		t.Fatalf("快照 offset = %d，期望 5", st.Offset)
	}
	if st.Done {
		t.Fatal("还在跑的时候不该标 done")
	}
	// 异步推可能还在飞：轮询等它落地，不去碰实现的调度细节
	deadline := time.Now().Add(time.Second)
	for {
		sink.mu.Lock()
		frames := len(sink.frames)
		sink.mu.Unlock()
		if frames >= 2 || time.Now().After(deadline) {
			if frames < 2 {
				t.Fatalf("思考与正文都应有帧，实际 %d", frames)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 结束：同步推最后一份，带 done，清扫靠它判断「这一轮写完没有」
	l.finish()
	if st := sink.last(); !st.Done {
		t.Fatalf("结束时最后一份快照应带 done: %+v", st)
	}
	if text, _ := l.texts(); text != "你好" {
		t.Fatalf("收尾兜底用的正文 = %q", text)
	}
}

// 本进程没有执行体时（换副本 / 刚重启）从缓存回放，并跟着远端把这一轮读完。
func TestSubscribeFromCacheReplays(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := cache.Open("", time.Minute)
	if err != nil {
		t.Fatalf("建缓存失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	payload, _ := json.Marshal(delta("你好"))
	if err := c.AppendFrames(ctx, "r1", []cache.Frame{{Offset: 2, Payload: payload}}); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
	if err := c.SaveRun(ctx, "r1", cache.RunState{Content: "你好", Offset: 2}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	svc := &Service{stream: c, runs: newLiveRuns(), log: slog.Default()}
	feed := svc.subscribeFromCache(ctx, "r1", 0)
	if feed == nil {
		t.Fatal("缓存里有帧时应返回订阅")
	}
	defer feed.Close()
	if len(feed.Backlog) != 1 || feed.Backlog[0].Event.Delta != "你好" {
		t.Fatalf("回放内容不对: %+v", feed.Backlog)
	}

	// 生产者写完 done：订阅应当结束，交给前端去库里取最终消息
	_ = c.SaveRun(ctx, "r1", cache.RunState{Content: "你好", Offset: 2, Done: true})
	select {
	case <-feed.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("done 之后订阅没有结束")
	}

	// 缓存里没有这一轮：返回 nil，走「已结束」的库路径
	if feed := svc.subscribeFromCache(ctx, "missing", 0); feed != nil {
		t.Fatal("没有帧时不该返回订阅")
	}
}
