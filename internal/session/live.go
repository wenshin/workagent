package session

import (
	"context"
	"sync"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
)

const (
	// maxLiveBytes 是单个 run 在内存里保留的增量字符上限。
	// 超出后丢弃最旧的帧：客户端拿不到前缀，但仍在跟随最新内容，
	// 完整正文在 run 结束时整段落库，不会真的丢东西。
	maxLiveBytes = 256 << 10
	// maxLiveFrames 是帧数上限，防止极短增量把内存撑爆。
	maxLiveFrames = 8192
)

// closedCh 是一个永远可读的已关闭 channel，用于「没有直播流」的场景。
var closedCh = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// RunFrame 是流缓冲里的一帧。Offset 是累计字符数（思考与正文共用同一个计数），
// 客户端把最后一帧的 Offset 作为 after_offset 回传即可续传。
type RunFrame struct {
	Offset int
	Event  agent.Event
}

// runFeed 是一次流订阅：先补发 Backlog，再从 Frames 收增量。
// Live 为 false 表示 run 已经不在本进程（换副本或已结束），调用方应改走
// 「拉 run_step + 最终消息」。
type runFeed struct {
	Backlog []RunFrame
	Frames  <-chan RunFrame
	Done    <-chan struct{}
	Live    bool

	stop func()
}

// Close 取消订阅；重复调用安全。
func (f *runFeed) Close() {
	if f != nil && f.stop != nil {
		f.stop()
	}
}

// liveRun 是一次正在执行的 run 的内存视图。
//
// 它只服务于「同一进程内的重连」：刷新页面后客户端从帧流里续上，
// 不必等 run 结束再整量重取。跨进程（换副本）由上层转发或降级，
// liveRun 不承担持久化职责。
type liveRun struct {
	mu sync.Mutex

	userID string
	cancel context.CancelFunc
	// reason 记录是「谁」掐断了这一轮：显式取消与墙钟到点要能区分
	reason string

	frames  []RunFrame
	bytes   int
	offset  int
	dropped int

	subs   map[int]*runSubscriber
	nextID int

	done   bool
	doneCh chan struct{}
}

type runSubscriber struct {
	ch   chan RunFrame
	stop chan struct{}
}

func newLiveRun(userID string, cancel context.CancelFunc) *liveRun {
	return &liveRun{
		userID: userID,
		cancel: cancel,
		subs:   map[int]*runSubscriber{},
		doneCh: make(chan struct{}),
	}
}

// broadcast 记录一帧并分发给订阅者。
//
// 事件先落内存再推客户端：客户端断开只是少一个订阅者，run 本身不受影响，
// 这是「刷新页面不中断任务」的全部秘密。
func (l *liveRun) broadcast(ev agent.Event) int {
	l.mu.Lock()
	if l.done {
		offset := l.offset
		l.mu.Unlock()
		return offset
	}
	if ev.Type == agent.EventMessageDelta {
		l.offset += utf8.RuneCountInString(ev.Delta)
	}
	frame := RunFrame{Offset: l.offset, Event: ev}
	l.frames = append(l.frames, frame)
	l.bytes += len(ev.Delta)
	for (l.bytes > maxLiveBytes || len(l.frames) > maxLiveFrames) && len(l.frames) > 1 {
		old := l.frames[0]
		l.frames = l.frames[1:]
		l.bytes -= len(old.Event.Delta)
		l.dropped = old.Offset
	}
	subs := make([]*runSubscriber, 0, len(l.subs))
	for _, s := range l.subs {
		subs = append(subs, s)
	}
	offset := l.offset
	l.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- frame:
		default:
			// 订阅者跟不上：丢这一帧，它下次按 offset 重连会补齐
		}
	}
	return offset
}

// subscribe 订阅 after 之后的帧；after 为已收到的最大 offset。
func (l *liveRun) subscribe(after int) *runFeed {
	l.mu.Lock()
	defer l.mu.Unlock()

	var backlog []RunFrame
	for _, f := range l.frames {
		if f.Offset > after {
			backlog = append(backlog, f)
		}
	}
	if l.done {
		return &runFeed{Backlog: backlog, Done: l.doneCh, Live: false}
	}

	id := l.nextID
	l.nextID++
	sub := &runSubscriber{ch: make(chan RunFrame, 64), stop: make(chan struct{})}
	l.subs[id] = sub

	return &runFeed{
		Backlog: backlog,
		Frames:  sub.ch,
		Done:    sub.stop,
		Live:    true,
		stop: func() {
			l.mu.Lock()
			delete(l.subs, id)
			l.mu.Unlock()
		},
	}
}

// markCancelled 标记「用户显式取消」并掐断 run；已结束返回 false。
func (l *liveRun) markCancelled(reason string) bool {
	l.mu.Lock()
	// 已经结束或已经取消过：重复点击停止不应产生第二个取消原因
	if l.done || l.reason != "" {
		l.mu.Unlock()
		return false
	}
	l.reason = reason
	l.mu.Unlock()
	l.cancel()
	return true
}

// cancelledReason 返回被掐断的原因，空表示不是外部取消。
func (l *liveRun) cancelledReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

// finish 关闭这一轮的内存视图；此后订阅者收到 Done，新的订阅只能拿到缓冲。
func (l *liveRun) finish() {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	l.done = true
	close(l.doneCh)
	subs := l.subs
	l.subs = map[int]*runSubscriber{}
	l.mu.Unlock()

	for _, s := range subs {
		close(s.stop)
	}
}

// liveRuns 是进程内正在执行的 run 表。键是 run_id。
type liveRuns struct {
	mu sync.Mutex
	m  map[string]*liveRun
}

func newLiveRuns() *liveRuns { return &liveRuns{m: map[string]*liveRun{}} }

func (r *liveRuns) add(runID, userID string, cancel context.CancelFunc) *liveRun {
	l := newLiveRun(userID, cancel)
	r.mu.Lock()
	r.m[runID] = l
	r.mu.Unlock()
	return l
}

func (r *liveRuns) get(runID string) *liveRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[runID]
}

// remove 摘掉一个已结束的 run；只有当前登记的还是同一个实例时才删，
// 避免把同名新 run 误删。
func (r *liveRuns) remove(runID string, l *liveRun) {
	r.mu.Lock()
	if cur, ok := r.m[runID]; ok && cur == l {
		delete(r.m, runID)
	}
	r.mu.Unlock()
}
