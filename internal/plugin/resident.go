package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// 常驻进程的默认策略。
//
// 空闲回收是必须的：插件不是常驻服务，没人用的时候没必要占着一个 deno 进程
// （每个进程大约 40–80MB）。10 分钟覆盖「连续对话里的多次调用」这个真实场景，
// 又不至于把机器撑爆。
const (
	DefaultProcIdle = 10 * time.Minute
	DefaultProcMax  = 32
	// 单次 invoke 的墙钟兜底：manifest 的 limits.timeout_ms 才是正式上限，
	// 这里只在清单没写时提供一个安全值。
	defaultInvokeTimeout = 30 * time.Second
)

// residentPool 是「每个插件一个常驻进程」的池子。
//
// 键是 空间|slug|目录：目录变化（重新安装、换版本、草稿被改）就是另一个进程，
// 不复用旧代码。同一个键永远只有一个进程，并发调用在进程内串行——
// 这与插件侧 SDK 的 invoke 队列语义一致，不会因为常驻而改变可见行为。
type residentPool struct {
	mu     sync.Mutex
	procs  map[string]*residentProc
	idle   time.Duration
	max    int
	logger *slog.Logger
	stop   chan struct{}
	once   sync.Once
}

func newResidentPool(idle time.Duration, max int, log *slog.Logger) *residentPool {
	if idle <= 0 {
		idle = DefaultProcIdle
	}
	if max <= 0 {
		max = DefaultProcMax
	}
	if log == nil {
		log = slog.Default()
	}
	p := &residentPool{
		procs:  map[string]*residentProc{},
		idle:   idle,
		max:    max,
		logger: log,
		stop:   make(chan struct{}),
	}
	go p.reap()
	return p
}

// acquire 取一个可用的常驻进程；没有就现起一个。
//
// 起进程放在锁外：deno 冷启动约 100ms，占着锁会让别的插件的调用一起排队。
// 代价是两个并发首次调用可能各起一个进程，后到的那个立刻关掉，不影响正确性。
func (p *residentPool) acquire(key string, rt *toolRuntime, start func() (*sandbox.Proc, error)) (*residentProc, error) {
	p.mu.Lock()
	if rp, ok := p.procs[key]; ok && !rp.dead() {
		rp.touch()
		p.mu.Unlock()
		return rp, nil
	}
	// 满了就先回收最久没用的（只回收空闲的，正在跑的不动）
	var victim *residentProc
	if len(p.procs) >= p.max {
		var victimKey string
		var oldest time.Time
		for k, rp := range p.procs {
			if rp.busy() {
				continue
			}
			seen := rp.lastSeen()
			if victim == nil || seen.Before(oldest) {
				victim, victimKey, oldest = rp, k, seen
			}
		}
		if victim != nil {
			delete(p.procs, victimKey)
		}
	}
	p.mu.Unlock()
	if victim != nil {
		victim.close()
	}

	proc, err := start()
	if err != nil {
		return nil, err
	}
	rp := newresidentProc(key, proc, rt, p.logger)

	p.mu.Lock()
	if existing, ok := p.procs[key]; ok && !existing.dead() {
		p.mu.Unlock()
		rp.close()
		return existing, nil
	}
	p.procs[key] = rp
	p.mu.Unlock()
	return rp, nil
}

func (p *residentPool) drop(key string, rp *residentProc) {
	p.mu.Lock()
	if cur, ok := p.procs[key]; ok && cur == rp {
		delete(p.procs, key)
	}
	p.mu.Unlock()
}

// closePrefix 关闭键前缀匹配的进程（空间级失效）。
func (p *residentPool) closePrefix(prefix string) int {
	p.mu.Lock()
	var list []*residentProc
	for k, rp := range p.procs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			list = append(list, rp)
			delete(p.procs, k)
		}
	}
	p.mu.Unlock()
	for _, rp := range list {
		rp.close()
	}
	return len(list)
}

// CloseAll 关闭全部进程并停止回收协程（服务退出）。
func (p *residentPool) CloseAll() {
	p.once.Do(func() { close(p.stop) })
	p.mu.Lock()
	list := make([]*residentProc, 0, len(p.procs))
	for k, rp := range p.procs {
		list = append(list, rp)
		delete(p.procs, k)
	}
	p.mu.Unlock()
	for _, rp := range list {
		rp.close()
	}
}

func (p *residentPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.procs)
}

func (p *residentPool) reap() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			now := time.Now()
			p.mu.Lock()
			var list []*residentProc
			for k, rp := range p.procs {
				if rp.busy() {
					continue
				}
				if now.Sub(rp.lastSeen()) >= p.idle {
					list = append(list, rp)
					delete(p.procs, k)
				}
			}
			p.mu.Unlock()
			for _, rp := range list {
				rp.close()
				p.logger.Info("插件常驻进程空闲回收", "key", rp.key)
			}
		}
	}
}

// residentProc 是一个常驻的插件进程。
//
// 它只做协议泵：invoke 帧进、return 帧出、中间的 call 帧转给宿主。
// 不做业务判断——能力校验仍然在 hostCall 里，和一次性执行时完全一样。
type residentProc struct {
	key  string
	proc *sandbox.Proc
	rt   *toolRuntime
	log  *slog.Logger

	wmu sync.Mutex // 串行化对 stdin 的写
	imu sync.Mutex // 串行化 invoke：一个进程同时只处理一个请求

	mu       sync.Mutex
	cur      *invokeInput
	curCtx   context.Context
	rpc      int
	overflow string
	lastUsed time.Time
	inflight int

	replies chan rpcFrame
	done    chan struct{}
	err     error
	once    sync.Once
}

func newresidentProc(key string, proc *sandbox.Proc, rt *toolRuntime, log *slog.Logger) *residentProc {
	p := &residentProc{
		key:      key,
		proc:     proc,
		rt:       rt,
		log:      log,
		replies:  make(chan rpcFrame, 1),
		done:     make(chan struct{}),
		lastUsed: time.Now(),
	}
	go p.loop()
	return p
}

func (p *residentProc) lastSeen() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastUsed
}

func (p *residentProc) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight > 0
}

func (p *residentProc) touch() {
	p.mu.Lock()
	p.lastUsed = time.Now()
	p.mu.Unlock()
}

func (p *residentProc) dead() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *residentProc) exitErr() error {
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	msg := clipText(p.proc.Stderr(), 500)
	switch {
	case err != nil && msg != "":
		return fmt.Errorf("插件进程已退出（%s）", msg)
	case err != nil:
		return fmt.Errorf("插件进程已退出: %w", err)
	case msg != "":
		return fmt.Errorf("插件进程已退出（%s）", msg)
	default:
		return errors.New("插件进程已退出")
	}
}

// loop 是唯一的读循环：一次启动，活到进程结束。
//
// 与一次性执行的差别只有一处：收到 return 之后不退出，继续读下一帧。
// 这也是常驻化必须先把读循环拆出来的原因——原来的写法把「读完就关」写死在流程里了。
func (p *residentProc) loop() {
	r := bufio.NewReaderSize(p.proc.Stdout, 64<<10)
	for {
		line, err := readLine(r, maxFrameBytes)
		if err != nil {
			p.fail(fmt.Errorf("读取插件输出失败: %w", err))
			return
		}
		var frame rpcFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			p.fail(fmt.Errorf("插件输出了非法帧: %s", clipText(string(line), 200)))
			return
		}
		switch frame.Op {
		case "return", "error":
			select {
			case p.replies <- frame:
			default:
				// 没有等待者：可能是超时后迟到的帧，丢弃即可。
			}
		case "call":
			p.handleCall(frame)
		default:
			p.fail(fmt.Errorf("插件输出了未知帧 %q", frame.Op))
			return
		}
	}
}

func (p *residentProc) handleCall(frame rpcFrame) {
	p.mu.Lock()
	in := p.cur
	ctx := p.curCtx
	limit := 0
	if in != nil {
		limit = in.Limits.MaxRPC
	}
	p.rpc++
	over := limit > 0 && p.rpc > limit
	if over && p.overflow == "" {
		p.overflow = fmt.Sprintf("插件单次调用超过宿主 RPC 上限（%d 次）", limit)
	}
	p.mu.Unlock()

	if over || in == nil || ctx == nil {
		msg := "宿主当前没有正在执行的调用"
		if over {
			msg = p.overflowText()
		}
		_ = p.write(rpcReply{Op: "result", ID: frame.ID, OK: false, Error: msg})
		return
	}

	data, callErr := p.rt.hostCall(ctx, *in, frame)
	reply := rpcReply{Op: "result", ID: frame.ID, OK: callErr == nil, Data: data}
	if callErr != nil {
		reply.Error = callErr.Error()
	}
	if err := p.write(reply); err != nil {
		p.fail(fmt.Errorf("回写宿主结果失败: %w", err))
	}
}

func (p *residentProc) overflowText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overflow
}

// invoke 发一个 invoke 帧并等它的结果。
//
// 超时即杀进程并从池里摘掉：协议可能停在半截（插件还没回 return），
// 留着复用会得到「上一个请求的返回值」这种最难查的错。
func (p *residentProc) invoke(ctx context.Context, in invokeInput, input json.RawMessage) (string, error) {
	p.imu.Lock()
	defer p.imu.Unlock()

	if p.dead() {
		return "", p.exitErr()
	}

	timeout := time.Duration(in.Limits.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultInvokeTimeout
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p.mu.Lock()
	p.cur = &in
	p.curCtx = ictx
	p.rpc = 0
	p.overflow = ""
	p.inflight++
	p.lastUsed = time.Now()
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.cur = nil
		p.curCtx = nil
		p.inflight--
		p.lastUsed = time.Now()
		p.mu.Unlock()
	}()

	// 清掉可能残留的迟到帧，避免把上一次的结果当成这一次的。
	select {
	case <-p.replies:
	default:
	}

	if err := p.write(map[string]any{
		"op":   "invoke",
		"data": json.RawMessage(input),
		"args": map[string]any{"space_id": in.SpaceID, "user_id": in.UserID, "slug": in.Slug},
	}); err != nil {
		p.fail(err)
		return "", fmt.Errorf("发送调用帧失败: %w", err)
	}

	select {
	case frame := <-p.replies:
		if over := p.overflowText(); over != "" {
			return "", errors.New(over)
		}
		if frame.Op == "error" || frame.Error != "" {
			msg := frame.Error
			if msg == "" {
				var e struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(frame.Data, &e)
				msg = e.Message
			}
			if msg == "" {
				msg = "插件返回错误"
			}
			return "", errors.New(msg)
		}
		return frameData(frame.Data), nil
	case <-p.done:
		return "", p.exitErr()
	case <-ictx.Done():
		// 进程状态未知：杀掉，下次调用重新起一个
		p.fail(fmt.Errorf("插件执行超时（%dms）", in.Limits.TimeoutMS))
		return "", fmt.Errorf("插件执行超时（%dms）", in.Limits.TimeoutMS)
	}
}

func (p *residentProc) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.proc.Write(append(body, '\n'))
}

func (p *residentProc) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.proc.Kill()
	p.once.Do(func() { close(p.done) })
}

func (p *residentProc) close() {
	p.proc.Close()
	p.once.Do(func() {
		p.mu.Lock()
		if p.err == nil {
			p.err = errors.New("插件进程已关闭")
		}
		p.mu.Unlock()
		close(p.done)
	})
}
