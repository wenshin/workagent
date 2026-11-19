package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/cache"
)

const (
	// maxLiveBytes 是单个 run 在内存里保留的增量文本上限。
	//
	// 超出后只能丢弃最旧的帧：客户端拿不到前缀，但仍在跟随最新内容，
	// 完整正文还有 Redis 快照兜底。1MB 足够容纳长思考，不必为省这点内存
	// 让「切页回来只看到后半段」成为常态。
	maxLiveBytes = 1 << 20
	// maxLiveFrames 是帧数上限：防止「一个 token 一帧」把内存撑爆。
	// 超限时先合并同类增量（不改文本量），合并不过来才丢前缀。
	maxLiveFrames = 8192
	// streamFlushInterval 是把正文与帧推到 Redis 的最小间隔。
	//
	// 不逐 token 写（写放大、且 Redis 也没必要扛这个 QPS），也不只在结束时写
	// （进程一死、审批等人期间切页，这段正文就找不回来）。秒级一次，
	// 进程级故障最多丢最后 1s。
	streamFlushInterval = 1 * time.Second
	// pushTimeout 是单次推 Redis 的超时：缓存慢不能反过来卡住模型输出。
	pushTimeout = 3 * time.Second
)

// closedCh 是一个永远可读的已关闭 channel，用于「没有直播流」的场景。
var closedCh = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// StreamSink 是流缓冲的落点（Redis）。接口只收快照与帧，
// 不含任何 session 类型，避免缓存层反向依赖。
type StreamSink interface {
	SaveRun(ctx context.Context, runID string, st cache.RunState) error
	AppendFrames(ctx context.Context, runID string, frames []cache.Frame) error
}

// RunFrame 是流缓冲里的一帧。Offset 是累计字符数（思考与正文共用同一个计数），
// 客户端把最后一帧的 Offset 作为 after_offset 回传即可续传。
type RunFrame struct {
	Offset int
	Event  agent.Event
}

// runFeed 是一次流订阅：先补发 Backlog，再从 Frames 收增量。
// Live 为 false 表示 run 已经不在本进程（换副本或已结束），调用方应改走
// 「Redis 回放 / 拉 run_step + 最终消息」。
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
// 两件事：一是同进程内的低延迟扇出（刷新后立刻接上），二是每 1s 把正文快照
// 与事件帧推到 Redis——后者才是「进程被杀 / 换副本也能恢复」的依据。
type liveRun struct {
	mu sync.Mutex

	runID  string
	userID string
	cancel context.CancelFunc
	// reason 记录是「谁」掐断了这一轮：显式取消与墙钟到点要能区分
	reason string
	// sink 为空表示不落缓存（未配置 Redis 且走内存实现时也可能为空）
	sink StreamSink
	log  *slog.Logger

	// 正文与思考攒在这里：结束时 run.go 用它兜底，Begin/FinishMessage 只写一次库
	text      strings.Builder
	reasoning strings.Builder

	frames  []RunFrame
	pending []RunFrame
	bytes   int
	offset  int
	dropped int

	lastPush time.Time
	pushing  bool
	// writeMu 串行化落缓存：并发的「异步节流推」与「断点强推」不能交错写
	writeMu sync.Mutex

	subs   map[int]*runSubscriber
	nextID int

	done   bool
	doneCh chan struct{}
}

type runSubscriber struct {
	ch   chan RunFrame
	stop chan struct{}
}

func newLiveRun(runID, userID string, cancel context.CancelFunc, sink StreamSink, log *slog.Logger) *liveRun {
	return &liveRun{
		runID:  runID,
		userID: userID,
		cancel: cancel,
		sink:   sink,
		log:    log,
		subs:   map[int]*runSubscriber{},
		doneCh: make(chan struct{}),
	}
}

// broadcast 记录一帧、分发给订阅者，并按节流推到 Redis。
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
		if ev.Reasoning {
			l.reasoning.WriteString(ev.Delta)
		} else {
			l.text.WriteString(ev.Delta)
		}
	}
	frame := RunFrame{Offset: l.offset, Event: ev}
	l.frames = append(l.frames, frame)
	l.pending = append(l.pending, frame)
	l.bytes += len(ev.Delta)
	l.compact()
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
	l.flush(false)
	return offset
}

// texts 返回已产出的正文与思考（结束时用来兜底 Result 里的空内容）。
func (l *liveRun) texts() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String(), l.reasoning.String()
}

// flush 按节流推一次；force 用于时间线上的断点（工具调用、进入审批等待）：
// 这些地方一停就是几分钟，不落缓存的话切页就只剩工具卡。
func (l *liveRun) flush(force bool) {
	if l == nil || l.sink == nil {
		return
	}
	l.mu.Lock()
	recent := time.Since(l.lastPush) < streamFlushInterval
	if !force && recent {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	// 强制推是时间线上的断点（工具调用、等人），必须等落地：否则进程下一秒被杀，
	// 这段正文就只剩内存里那一份
	l.push(force)
}

// flushFinal 同步推最后一次（带 done），保证「进程崩溃后清扫」看到的正文是完整的。
func (l *liveRun) flushFinal() {
	if l == nil || l.sink == nil {
		return
	}
	l.push(true)
}

// push 把正文快照与待发帧写进 Redis。
//
// 异步 push 用 pushing 去重：缓存慢的时候宁可少推一帧，也不能把模型输出卡住。
// 同步 push（结束时）必须等结果，否则 done 标记可能丢。
func (l *liveRun) push(sync bool) {
	l.mu.Lock()
	// 异步推：已经有一次在飞就跳过，缓存慢不能把模型输出拖住。
	// 同步推（断点 / 收尾）必须等，所以不跳过，交给 writeMu 排队。
	if !sync && l.pushing {
		l.mu.Unlock()
		return
	}
	l.pushing = true
	runID := l.runID
	state := cache.RunState{
		Content:   l.text.String(),
		Reasoning: l.reasoning.String(),
		Offset:    l.offset,
		Dropped:   l.dropped,
		Done:      l.done,
		UpdatedAt: time.Now(),
	}
	pending := l.pending
	l.pending = nil
	l.lastPush = time.Now()
	l.mu.Unlock()

	write := func() {
		// 串行化：异步推与断点强推可能同时发生，后到的快照必须赢
		l.writeMu.Lock()
		defer l.writeMu.Unlock()
		defer func() {
			l.mu.Lock()
			l.pushing = false
			l.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		if err := l.sink.SaveRun(ctx, runID, state); err != nil {
			l.log.Warn("写入流快照失败", "run_id", runID, "err", err)
			return
		}
		if len(pending) == 0 {
			return
		}
		frames := make([]cache.Frame, 0, len(pending))
		for _, f := range pending {
			data, err := json.Marshal(f.Event)
			if err != nil {
				continue
			}
			frames = append(frames, cache.Frame{Offset: f.Offset, Payload: data})
		}
		if err := l.sink.AppendFrames(ctx, runID, frames); err != nil {
			l.log.Warn("写入事件流失败", "run_id", runID, "err", err)
		}
	}
	if sync {
		write()
		return
	}
	go write()
}

// compact 处理缓冲上限：先合并、后丢弃。
//
// 帧数上限防的是「一个 token 一帧」的长思考——几千帧加起来可能只有几十 KB 文本，
// 直接丢最旧的等于把回答的开头挤掉，客户端 after_offset=0 重连时只能看到后半段。
// 所以超帧数上限时先把开头连续的同类增量合成一帧（文本量不变），
// 只有文本本身超过 maxLiveBytes 才真的丢前缀。
func (l *liveRun) compact() {
	if len(l.frames) > maxLiveFrames {
		merged := make([]RunFrame, 0, len(l.frames))
		merged = append(merged, l.frames[0])
		for _, f := range l.frames[1:] {
			last := &merged[len(merged)-1]
			if f.Event.Type == agent.EventMessageDelta &&
				last.Event.Type == agent.EventMessageDelta &&
				f.Event.Reasoning == last.Event.Reasoning {
				last.Event.Delta += f.Event.Delta
				last.Offset = f.Offset
				continue
			}
			merged = append(merged, f)
		}
		l.frames = merged
	}
	// 文本超限：只能丢前缀，记下丢到哪儿供排查
	for len(l.frames) > 1 && l.bytes > maxLiveBytes {
		old := l.frames[0]
		l.frames = l.frames[1:]
		l.bytes -= len(old.Event.Delta)
		l.dropped = old.Offset
	}
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

// finish 关闭这一轮的内存视图：先同步把最终快照（done=true）推到 Redis，
// 此后订阅者收到 Done，新的订阅只能拿到缓冲。
func (l *liveRun) finish() {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	l.done = true
	l.mu.Unlock()

	// 同步推：清扫要靠 done 判断「这一轮到底写完没有」，异步可能永远不来
	l.flushFinal()

	l.mu.Lock()
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

func (r *liveRuns) add(runID, userID string, cancel context.CancelFunc, sink StreamSink, log *slog.Logger) *liveRun {
	l := newLiveRun(runID, userID, cancel, sink, log)
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
