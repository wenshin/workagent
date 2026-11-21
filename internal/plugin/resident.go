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

// 常驻进程的策略。
//
// 常驻进程**不按空闲回收**：声明 runtime.resident 的插件之所以常驻，就是为了
// 跨调用保留连接、缓存与模块级状态；空闲几分钟就杀掉，等于把「常驻」退化成
// 「碰巧复用过一次」，下一次调用又付 50–180ms 冷启动，还会丢掉进程内状态。
//
// 但「不回收」不等于「不守护」：常驻进程会自己退出（插件抛未捕获异常、
// OOM、连接被对端关闭后没兜住）。业务侧不能依赖「下一次调用碰巧会重建」——
// 那一次调用本身就是失败。所以这里做两层：
//
//  1. 崩溃即重启，带指数退避（300ms → 5s）；
//  2. 连续崩溃超过 MaxRestarts 次就停下并熔断，不再每次调用都付一遍
//     冷启动 + 崩溃的开销，改为直接把「插件不可用」报给调用方。
//
// 熔断不是终点：重新安装/升级/改配置/改草稿都会走 closePrefix 清掉计数，
// 修好的插件下一次调用就会重新拉起。
const (
	DefaultProcMax = 32
	// DefaultProcMaxRestarts 是连续崩溃的重启上限。单次崩溃很常见（插件里
	// 一个未捕获异常就够），5 次给「偶发崩溃」留足恢复机会，又不至于让一个
	// 确定坏掉的插件无限重启。
	DefaultProcMaxRestarts = 5
	// 重启退避：第一次几乎立刻起，之后翻倍。等太久会让「进程刚崩过一次」
	// 的正常业务请求排队，等太短会让反复崩溃的插件把 CPU 打满。
	procRestartBase = 300 * time.Millisecond
	procRestartCap  = 5 * time.Second
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
	mu    sync.Mutex
	procs map[string]*residentProc
	// starters 保存每个键的启动闭包，守护重启时复用同一份参数（目录、FS、
	// 出网名单、配置）。参数一变（改配置/换版本/改草稿）就走 closePrefix
	// 把这条一起清掉——否则会「用旧参数重启新代码」。
	starters map[string]func() (*residentProc, error)
	// fails 记录每个键**连续**崩溃次数，成功执行一次就清零。
	// 用连续而不是累计：一个稳定跑几天偶尔崩一次的插件不该被熔断。
	fails       map[string]int
	max         int
	maxRestarts int
	stop        chan struct{}
	once        sync.Once
	logger      *slog.Logger
}

func newResidentPool(max, maxRestarts int, log *slog.Logger) *residentPool {
	if max <= 0 {
		max = DefaultProcMax
	}
	if maxRestarts < 0 {
		maxRestarts = DefaultProcMaxRestarts
	}
	if log == nil {
		log = slog.Default()
	}
	return &residentPool{
		procs:       map[string]*residentProc{},
		starters:    map[string]func() (*residentProc, error){},
		fails:       map[string]int{},
		max:         max,
		maxRestarts: maxRestarts,
		stop:        make(chan struct{}),
		logger:      log,
	}
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
	if n := p.fails[key]; n > p.maxRestarts {
		p.mu.Unlock()
		return nil, fmt.Errorf("插件常驻进程连续崩溃 %d 次，已停止自动重启；重新安装或修改插件后恢复", n)
	}
	// 到达上限时腾一个位置：淘汰最久没被调用过的**空闲**进程，在跑的不动。
	// 这不等于空闲回收——没有「多久没用就杀」的阈值，只在池满时发生。
	// 如果全部都在跑，宁可控不住上限也不打断正在执行的调用。
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
			delete(p.starters, victimKey)
			delete(p.fails, victimKey)
		}
	}
	p.mu.Unlock()
	if victim != nil {
		victim.close()
		p.logger.Info("插件常驻进程达到上限，淘汰最久未用", "key", victim.key, "max", p.max)
	}
	return p.spawn(key, rt, start)
}

// spawn 起第一个进程并交给守护协程。
func (p *residentPool) spawn(key string, rt *toolRuntime, start func() (*sandbox.Proc, error)) (*residentProc, error) {
	proc, err := start()
	if err != nil {
		// 起不来也是一次失败：一个启动就崩的插件不能每次调用都重付启动开销。
		p.noteFailure(key, nil)
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
	p.starters[key] = func() (*residentProc, error) {
		proc, err := start()
		if err != nil {
			return nil, err
		}
		return newresidentProc(key, proc, rt, p.logger), nil
	}
	p.mu.Unlock()

	go p.supervise(key, rp)
	return rp, nil
}

// spawnFrom 用已保存的启动闭包重启（守护专用）。
func (p *residentPool) spawnFrom(key string, starter func() (*residentProc, error)) (*residentProc, error) {
	rp, err := starter()
	if err != nil {
		p.noteFailure(key, nil)
		return nil, err
	}
	p.mu.Lock()
	if existing, ok := p.procs[key]; ok && !existing.dead() {
		// 并发下别的调用已经拉起了一个：用现成的，别留两个进程
		p.mu.Unlock()
		rp.close()
		return existing, nil
	}
	p.procs[key] = rp
	p.mu.Unlock()

	go p.supervise(key, rp)
	return rp, nil
}

// supervise 等一个进程结束，异常退出就按退避策略重启。
//
// 主动关闭（停用/卸载/换版本/空间重载/池满淘汰/服务退出）不重启——那些是
// 管理动作，不是故障。区分靠 residentProc.crashed：只有 fail() 会置位。
func (p *residentPool) supervise(key string, rp *residentProc) {
	<-rp.done
	if !rp.isCrashed() {
		return
	}

	p.mu.Lock()
	cur, ok := p.procs[key]
	if !ok || cur != rp {
		// 已被替换或已被 closePrefix 摘掉：不是我们要管的那个进程
		p.mu.Unlock()
		return
	}
	delete(p.procs, key)
	p.fails[key]++
	attempts := p.fails[key]
	starter := p.starters[key]
	stop := p.stop
	p.mu.Unlock()

	if attempts > p.maxRestarts {
		p.logger.Error("插件常驻进程反复崩溃，停止自动重启",
			"key", key, "attempts", attempts, "max", p.maxRestarts, "err", rp.exitErr())
		return
	}
	if starter == nil {
		return
	}

	p.logger.Warn("插件常驻进程异常退出，将自动重启",
		"key", key, "attempt", attempts, "delay", restartDelay(attempts), "err", rp.exitErr())
	go p.restartLoop(key, starter, stop, restartDelay(attempts))
}

// restartLoop 反复尝试拉起，直到成功、被摘掉或达到重启上限。
//
// 单独成一个循环而不是「失败就交给下一次调用」：插件崩在启动阶段
// 是常见的（缺依赖、配置错），没有这层就等于永远起不来。
func (p *residentPool) restartLoop(key string, starter func() (*residentProc, error), stop <-chan struct{}, delay time.Duration) {
	for {
		t := time.NewTimer(delay)
		select {
		case <-stop:
			t.Stop()
			return
		case <-t.C:
		}

		p.mu.Lock()
		if _, ok := p.starters[key]; !ok {
			// 期间被 closePrefix 清掉（停用/卸载/换版本）：别再拉了
			p.mu.Unlock()
			return
		}
		if cur, ok := p.procs[key]; ok && !cur.dead() {
			p.mu.Unlock()
			return
		}
		broken := p.fails[key] > p.maxRestarts
		p.mu.Unlock()
		if broken {
			return
		}

		if _, err := p.spawnFrom(key, starter); err == nil {
			return
		} else {
			p.logger.Warn("插件常驻进程重启失败", "key", key, "err", err)
		}

		delay *= 2
		if delay > procRestartCap {
			delay = procRestartCap
		}
	}
}

// restartDelay 返回第 n 次连续崩溃后的等待时间。
func restartDelay(attempts int) time.Duration {
	d := procRestartBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= procRestartCap {
			return procRestartCap
		}
	}
	return d
}

// noteFailure 记一次启动失败。err 为 nil 表示进程已经起过但立刻崩了。
func (p *residentPool) noteFailure(key string, err error) {
	p.mu.Lock()
	p.fails[key]++
	n := p.fails[key]
	p.mu.Unlock()
	if n > p.maxRestarts {
		p.logger.Error("插件常驻进程连续启动失败，停止自动重启",
			"key", key, "attempts", n, "max", p.maxRestarts, "err", err)
	}
}

// markHealthy 清零连续崩溃计数：跑成功一次就说明插件是好的，
// 后面偶发崩溃从 1 开始数，不继承历史。
func (p *residentPool) markHealthy(key string) {
	p.mu.Lock()
	delete(p.fails, key)
	p.mu.Unlock()
}

// failed 返回该键是否已熔断（连续崩溃超过上限）。
func (p *residentPool) failed(key string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.fails[key]
	return n, n > p.maxRestarts
}

// closePrefix 关闭键前缀匹配的进程（空间级失效），并清掉守护状态：
// 参数可能已经变了，不能再拿旧闭包重启。
func (p *residentPool) closePrefix(prefix string) int {
	p.mu.Lock()
	var list []*residentProc
	for k, rp := range p.procs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			list = append(list, rp)
			delete(p.procs, k)
		}
	}
	for k := range p.starters {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(p.starters, k)
		}
	}
	for k := range p.fails {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(p.fails, k)
		}
	}
	p.mu.Unlock()
	for _, rp := range list {
		rp.close()
	}
	return len(list)
}

// CloseAll 关闭全部进程并停掉待重启的守护协程（服务退出）。
func (p *residentPool) CloseAll() {
	p.once.Do(func() { close(p.stop) })
	p.mu.Lock()
	list := make([]*residentProc, 0, len(p.procs))
	for k, rp := range p.procs {
		list = append(list, rp)
		delete(p.procs, k)
	}
	p.starters = map[string]func() (*residentProc, error){}
	p.fails = map[string]int{}
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
	// crashed 区分「异常退出」与「主动关闭」：只有前者才触发守护重启。
	crashed bool

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

func (p *residentProc) isCrashed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.crashed
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
// 超时即杀进程：协议可能停在半截（插件还没回 return），留着复用会得到
// 「上一个请求的返回值」这种最难查的错。杀掉的进程由守护协程重启。
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
		// 进程状态未知：杀掉，交给守护重启
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

// fail 是异常退出：置 crashed 让守护协程重启。
func (p *residentProc) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.crashed = true
	p.mu.Unlock()
	p.proc.Kill()
	p.once.Do(func() { close(p.done) })
}

// close 是主动关闭：不置 crashed，守护协程不会重启它。
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
