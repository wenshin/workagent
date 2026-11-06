//go:build unix

// Package sandbox 提供「在受限环境里执行命令」的统一入口 Runner。
//
// 目标不是防住所有攻击，而是把 agent 执行命令的最坏情况变成可控：
// 文件系统只读（workspace 可写）、默认不出网、超时、输出上限、进程组整体回收。
//
// 三个后端，隔离强度递减：
//
//	bwrap         Linux，bubblewrap（user namespace + bind mount）
//	sandbox-exec  macOS，Seatbelt profile
//	local         无隔离，仅用于开发或明确降级；Name() 会如实报告
//
// 调用方必须自己检查 Name()：把 local 当成沙箱用是错误用法。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Mode 是执行策略档位，语义与 Codex CLI 的 sandbox_mode 对齐。
type Mode string

const (
	// ModeReadOnly 全盘只读，唯一可写的是本次调用的临时目录（TMPDIR/HOME）。
	ModeReadOnly Mode = "read-only"
	// ModeWorkspaceWrite 只有 Spec.Dir 与临时目录可写，其余只读。
	ModeWorkspaceWrite Mode = "workspace-write"
	// ModeFull 不套沙箱，只保留超时、输出上限、进程组回收与环境清理。
	// 仅用于受信任的运维命令，审计里会是 name=none。
	ModeFull Mode = "full"
)

// 默认值：命令是给人看的，30 秒足够；输出上限按工具层 16KB 的惯例放宽一档。
const (
	DefaultTimeout   = 30 * time.Second
	DefaultMaxOutput = 64 << 10
)

var (
	// ErrUnavailable 表示后端在这台机器上不可用（没装、user namespace 被禁、平台不符）。
	ErrUnavailable = errors.New("沙箱后端不可用")
	// ErrTimeout 表示命令超时被强制终止（进程组已整体回收）。
	ErrTimeout = errors.New("命令执行超时")
	// ErrOutputTooLarge 表示 stdout/stderr 超过上限，已终止进程，避免刷爆内存。
	ErrOutputTooLarge = errors.New("命令输出超过上限")
)

// Spec 描述一次命令执行。
type Spec struct {
	// Cmd 是要执行的命令，第一个元素是程序名；不做 shell 解释。
	Cmd []string
	// Dir 是工作目录，必须是绝对路径。ModeWorkspaceWrite 下它也是唯一可写目录。
	Dir string
	// Env 是追加/覆盖的环境变量（KEY=VALUE）。
	// 父进程环境不会继承，只有 PATH 等少数无害变量会带进去，避免密钥泄漏。
	Env []string
	// Stdin 是喂给命令的标准输入。密钥这类敏感数据只走这里：
	// argv 会出现在 ps 里，环境变量会出现在 /proc/<pid>/environ 里，stdin 不会。
	Stdin []byte
	// Mode 决定文件系统与网络边界，零值按 ModeReadOnly 处理。
	Mode Mode
	// Timeout 是墙钟上限，<=0 用 DefaultTimeout。
	Timeout time.Duration
	// MaxOutput 是 stdout / stderr 各自的上限，<=0 用 DefaultMaxOutput。
	MaxOutput int
	// Network 为 true 才允许出网；默认 false（ModeFull 下无意义）。
	Network bool
}

// Result 是一次执行的结果。命令正常退出（含非零退出码）时 error 为 nil，
// 退出码在 ExitCode 里；启动失败 / 超时 / 输出超限才返回 error。
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	// Truncated 表示输出被截断过（超限时进程已被终止）。
	Truncated bool
	// Sandbox 是实际生效的后端名：bwrap / sandbox-exec / local / none。
	Sandbox string
}

// Runner 是执行器。实现必须保证：超时后整棵进程树被回收，不留僵尸。
type Runner interface {
	// Name 是后端名，调用方用它判断是否真的隔离了。
	Name() string
	Run(ctx context.Context, spec Spec) (Result, error)
}

// 后端名。判定「有没有真隔离」是安全语义的一部分，不要用字面量比较。
const (
	NameBwrap    = "bwrap"
	NameSeatbelt = "sandbox-exec"
	NameLocal    = "local"
)

// ErrNotIsolated 表示当前没有可用的隔离后端，只有无隔离的 local。
var ErrNotIsolated = errors.New("sandbox: 没有可用的隔离后端")

// Detect 返回当前平台最强的可用后端；都不可用时回退 local。
// 调用方应检查 Name() 并记录告警，不要假设一定隔离。
func Detect() Runner {
	if r, err := NewBwrap(); err == nil {
		return r
	}
	if r, err := NewSeatbelt(); err == nil {
		return r
	}
	return NewLocal()
}

// Ensure 校验探测结果。requireIsolation 为真时，降级到 local 直接报错，
// 把「以为有沙箱、其实在裸跑」挡在启动阶段，而不是等出事才发现。
func Ensure(r Runner, requireIsolation bool) error {
	if r == nil {
		return fmt.Errorf("%w：没有可用的执行器", ErrNotIsolated)
	}
	if requireIsolation && r.Name() == NameLocal {
		return fmt.Errorf("%w：探测不到 bwrap / sandbox-exec（Linux 需安装 bubblewrap 且内核允许 user namespace）；"+
			"开发机可设 SANDBOX_REQUIRE=false 跳过该校验", ErrNotIsolated)
	}
	return nil
}

// NewLocal 是不做任何隔离的执行器，只保留超时 / 输出上限 / 环境清理。
// 它存在的意义是让开发机和 CI 能跑通，不是给生产用的。
func NewLocal() Runner {
	return &runner{name: NameLocal, build: nil}
}

// buildFunc 把 Spec 包装成最终命令行；env 由 Run 统一生成，bwrap 需要用它
// 逐条转成 --setenv（因为要 --clearenv）。
type buildFunc func(spec Spec, tmp string, env []string) ([]string, error)

type runner struct {
	name  string
	build buildFunc
}

func (r *runner) Name() string { return r.name }

func (r *runner) Run(ctx context.Context, spec Spec) (Result, error) {
	spec, err := spec.normalize()
	if err != nil {
		return Result{Sandbox: r.name}, err
	}

	// 每次调用一个私有临时目录：TMPDIR / HOME 都指向它，结束即删。
	// 这样 read-only 档位下命令依然能写临时文件，但不会残留到下一次调用。
	tmp, err := os.MkdirTemp("", "sandbox-")
	if err != nil {
		return Result{Sandbox: r.name}, fmt.Errorf("创建临时目录失败: %w", err)
	}
	// macOS 上 /var 是指向 /private/var 的符号链接，内核按真实路径匹配策略，
	// 不先解开就会出现「明明放开了写权限却 Operation not permitted」。
	if real, evalErr := filepath.EvalSymlinks(tmp); evalErr == nil {
		tmp = real
	}
	defer os.RemoveAll(tmp)
	// 同理：workspace 也统一成真实路径，才能和挂载点 / 策略里的路径对上。
	if real, evalErr := filepath.EvalSymlinks(spec.Dir); evalErr == nil {
		spec.Dir = real
	}

	env := buildEnv(spec, tmp)
	name := r.name
	argv := spec.Cmd
	if spec.Mode == ModeFull {
		// 明确声明不做隔离：审计里记 none，别让人误以为有边界。
		name = "none"
	} else if r.build != nil {
		argv, err = r.build(spec, tmp, env)
		if err != nil {
			return Result{Sandbox: name}, err
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	// 必须用 CommandContext：Cancel 只有在这种情况下才会被 exec 包接管。
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	stdout := newCapWriter(spec.MaxOutput, cancel)
	stderr := newCapWriter(spec.MaxOutput, cancel)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// 独立进程组：超时 / 输出超限时整组 kill，避免脚本 fork 出的子进程变孤儿。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	start := time.Now()
	runErr := cmd.Run()
	res := Result{
		Sandbox:   name,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Duration:  time.Since(start),
		Truncated: stdout.Overflowed() || stderr.Overflowed(),
	}

	// 先看输出超限：它也是通过 cancel 触发的，不先判会被误报成超时。
	if res.Truncated {
		return res, fmt.Errorf("%w（上限 %d 字节）", ErrOutputTooLarge, spec.MaxOutput)
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%w（%s）", ErrTimeout, spec.Timeout)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("启动命令失败: %w", runErr)
	}
	return res, nil
}

func (s Spec) normalize() (Spec, error) {
	if len(s.Cmd) == 0 || strings.TrimSpace(s.Cmd[0]) == "" {
		return s, fmt.Errorf("命令为空")
	}
	if s.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return s, fmt.Errorf("无法确定工作目录: %w", err)
		}
		s.Dir = wd
	}
	if !filepath.IsAbs(s.Dir) {
		abs, err := filepath.Abs(s.Dir)
		if err != nil {
			return s, fmt.Errorf("工作目录 %q 需要绝对路径: %w", s.Dir, err)
		}
		s.Dir = abs
	}
	if s.Mode == "" {
		s.Mode = ModeReadOnly
	}
	switch s.Mode {
	case ModeReadOnly, ModeWorkspaceWrite, ModeFull:
	default:
		return s, fmt.Errorf("未知的执行模式 %q", s.Mode)
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.MaxOutput <= 0 {
		s.MaxOutput = DefaultMaxOutput
	}
	return s, nil
}

// baseEnvKeys 是允许从父进程继承的环境变量：都是无害的本地化 / 路径信息。
// 其余（含各类 *_KEY / *_TOKEN）一律不带，避免沙箱里的 cat / env 顺手把密钥读走。
var baseEnvKeys = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}

const fallbackPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func buildEnv(spec Spec, tmp string) []string {
	env := []string{
		"HOME=" + tmp,
		"TMPDIR=" + tmp,
		"TERM=dumb",
		// 关掉语言运行时往 HOME 之外写缓存的行为，减少只读档位下的假失败
		"XDG_CACHE_HOME=" + tmp,
	}
	hasPath := false
	for _, k := range baseEnvKeys {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			env = append(env, k+"="+v)
			if k == "PATH" {
				hasPath = true
			}
		}
	}
	if !hasPath {
		env = append(env, "PATH="+fallbackPath)
	}
	return append(env, spec.Env...)
}

// capWriter 收集输出并在超限时触发终止。
// 永远返回 len(p), nil：让子进程继续跑（由 Kill 收场），而不是收到 EPIPE 后
// 以另一种错误退出，把真正的失败原因盖掉。
type capWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	overflow bool
	fired    bool
	onFull   func()
}

func newCapWriter(limit int, onFull func()) *capWriter {
	return &capWriter{limit: limit, onFull: onFull}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	room := w.limit - w.buf.Len()
	switch {
	case room <= 0:
		w.overflow = true
	case len(p) > room:
		w.buf.Write(p[:room])
		w.overflow = true
	default:
		w.buf.Write(p)
	}
	if w.overflow && !w.fired && w.onFull != nil {
		w.fired = true
		go w.onFull()
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *capWriter) Overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}
