//go:build unix

// Package sandbox 提供「在受限环境里执行命令」的统一入口 Runner。
//
// 目标不是防住所有攻击，而是把 agent 执行命令的最坏情况变成可控：
// 文件系统只读（workspace 可写）、默认不出网、超时、输出上限、进程组整体回收。
//
// 两个后端：
//
//	bwrap  Linux，bubblewrap（user namespace + bind mount）
//	local  无隔离，只给测试与本机工具用；Name() 会如实报告
//
// 路径也由这一层统一虚拟化：宿主目录只出现在 Mount.Source 里，沙箱内一律是
// /workspace、/pkg、/cache 这类固定路径，模型与环境变量看不到宿主结构。
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// ModeSystemWrite 在 workspace-write 之上放开系统安装目录（/usr、/var/lib/dpkg 等），
	// 用于 agent 在空间里直接 apt install / pip install / npm i -g。
	//
	// 装进容器的可写层，Pod 重建就丢，缺什么再装一次——这是"允许正常安装"的代价，
	// 也是它成立的前提。缓存与索引由部署挂到租户缓存，重装才有意义。
	ModeSystemWrite Mode = "system-write"
)

// systemWrite 是系统安装档位放开的路径。
//
// /etc 不整体放开：/etc/shadow、/etc/sudoers 必须留在只读侧，
// 所以这里只列安装真正会写的子路径。
// /var/lib/apt/lists 与 /var/cache/apt/archives 在部署里指向租户缓存，
// 漏挂的表现是每次 Pod 重建都要重新 apt-get update。
var (
	systemWriteMu sync.RWMutex
	systemWrite   = []string{
		"/usr",
		"/var/lib/dpkg",
		"/var/lib/apt/lists",
		"/var/cache/apt/archives",
		"/etc/alternatives",
		"/etc/ld.so.conf.d",
	}
)

// SetSystemWritePaths 覆盖系统可写路径，传空恢复默认。
// 只为适配非标准布局，正常部署不需要调用。
func SetSystemWritePaths(paths []string) {
	systemWriteMu.Lock()
	defer systemWriteMu.Unlock()
	if len(paths) == 0 {
		systemWrite = []string{
			"/usr",
			"/var/lib/dpkg",
			"/var/lib/apt/lists",
			"/var/cache/apt/archives",
			"/etc/alternatives",
			"/etc/ld.so.conf.d",
		}
		return
	}
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" && filepath.IsAbs(p) {
			clean = append(clean, filepath.Clean(p))
		}
	}
	systemWrite = clean
}

// SystemWritePaths 返回当前系统可写路径（拷贝）。
func SystemWritePaths() []string {
	systemWriteMu.RLock()
	defer systemWriteMu.RUnlock()
	return append([]string(nil), systemWrite...)
}

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

// 沙箱内的固定路径。宿主的真实目录不出现在命令行、环境变量与工作目录里：
// 模型看到的永远是这一组虚拟路径，空间在磁盘上的位置只有执行面自己知道。
const (
	// WorkspaceDir 是空间工作区（spaces/<id>/files）的挂载点与默认工作目录。
	WorkspaceDir = "/workspace"
	// PkgDir 是空间内的安装目录（spaces/<id>/.pkg）：venv 与 npm 全局包。
	PkgDir = "/pkg"
	// CacheDir 是租户缓存根（WA_CACHE_DIR）。
	CacheDir = "/cache"
	// HomeDir 是本次调用的私有 HOME / TMPDIR / XDG_CACHE_HOME。
	HomeDir = "/tmp/wa-home"
	// EgressProxy 是出网代理 socket 在沙箱里的固定路径。
	EgressProxy = "/tmp/wa-egress.sock"
	// PluginDir 是插件自己的目录（可写，运行时缓存落这里）。
	PluginDir = "/plugin"
	// SpaceDir 是插件声明 fs.read / fs.write 之后看到的空间工作区根。
	SpaceDir = "/space"
	// RuntimeDir 是注入的 bootstrap.ts / host_sdk.ts 所在目录。
	RuntimeDir = "/runtime"
	// DenoDir 是该插件版本的 DENO_DIR。
	DenoDir = "/deno"
)

// MountRoot 指明 Mount.Key 相对哪个根解析。
type MountRoot string

const (
	// RootStorage 是存储根（控制面的 STORAGE_ROOT / 执行体的 SANDBOX_AGENT_ROOT）。
	RootStorage MountRoot = ""
	// RootCache 是租户缓存根（WA_CACHE_DIR）。
	RootCache MountRoot = "cache"
)

// Mount 是一个要挂进沙箱的目录。
//
// 两种形态：
//
//	空间数据：Key（相对 Root）非空。过线的是 Key，两侧各自解析成自己的绝对路径，
//	          所以控制面与执行体不需要看到同一个路径；沙箱里只出现 Target。
//	系统路径：Key 为空，Source 是容器内一致的路径（解释器、平台脚本），
//	          原样挂到 Target（通常等于 Source）。这类路径不含业务数据。
type Mount struct {
	// Source 是本机真实绝对路径。本地执行时必须填；过线前会被清空，由执行体解析 Key。
	Source string
	// Key 是相对 Root 的路径，如 spaces/<id>/files。
	Key string
	// Root 决定 Key 相对哪个根解析。
	Root MountRoot
	// Target 是沙箱内挂载点，必须是绝对路径。
	Target string
	// Write 为 true 时可写；ModeReadOnly 下会被忽略。
	Write bool
}

// Kind 是执行体种类：决定这次调用落到哪一类容器上。
type Kind string

const (
	// KindShell 是空间容器：每空间一个，空闲超时销毁。
	KindShell Kind = "shell"
	// KindPlugin 是插件容器：每租户一个，常驻；插件运行与插件开发都落在这里。
	KindPlugin Kind = "plugin"
)

// Scope 是这次执行归属的租户 / 空间。
//
// 容器化执行体（internal/sandbox/k8s）按它解析到具体容器：空间容器按 Space
// 隔离，插件容器按 Tenant 共享。空值不是「无所谓」——Space 为空时执行体只能
// 拒绝，否则所有调用会挤进同一个默认容器。
type Scope struct {
	// Space 是空间 ID。
	Space string
	// Tenant 是租户标识；空表示执行体自行解析（插件容器据此共享）。
	Tenant string
	// Kind 决定容器种类，零值按 KindShell 处理。
	Kind Kind
}

// Spec 描述一次命令执行。
type Spec struct {
	// Scope 是这次执行的归属：执行体按它选容器。
	Scope Scope
	// Cmd 是要执行的命令，第一个元素是程序名；不做 shell 解释。
	Cmd []string
	// Dir 是沙箱内的工作目录（如 /workspace），默认 WorkspaceDir；
	// 没有工作区挂载时是 /。
	Dir string
	// Mounts 是要挂进沙箱的目录：工作区、安装目录、租户缓存、插件目录都在这里。
	// 路径的解析与挂载点见 Mount。
	Mounts []Mount
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
	//
	// 出网时不建代理、也不注入 HTTP_PROXY。应用层域名白名单在 share-net 下
	// 本来就能被 --noproxy 绕过，留着只会让人误以为有边界；真正的边界交给
	// 集群 NetworkPolicy 在网络层封禁私网段。
	//
	// 给网不改变文件可见面：Mounts 始终整体挂载。外发风险由「网络工具先审批」
	// 与 NetworkPolicy 承担——遮蔽工作区换来的那点纵深，代价是 `curl -o out.json`
	// 写下的文件凭空消失、`curl … && python …` 必须拆成两条。
	Network bool
	// Proxy 是出网代理的 unix socket 路径（见 internal/egress）。
	//
	// 非空时 Network 必须为 false：沙箱仍然 --unshare-net，这个 socket 是唯一的
	// 对外通道，域名白名单在宿主侧判定。两者同时设置会直接报错——否则
	// 「以为走了代理、其实直连」这种事故只会出现在线上。
	//
	// 目前只有插件运行时用它：Deno 能自己解析 unix: 前缀，npm / fnm / uv 不能。
	//
	// 这里填的是「建 socket 的那一侧」的真实路径，沙箱里它被挂到固定的
	// EgressProxy，环境变量 WA_EGRESS_PROXY 也指向那个固定路径——
	// 插件因此永远看不到宿主的临时目录。
	Proxy string
	// Egress 非空表示「白名单在这里，代理要由执行的一方自己建在 Proxy 上」。
	//
	// 容器化之后控制面与执行体不在同一个文件系统命名空间：控制面建好的
	// socket 在执行体里根本不存在，路径传过去只会得到 bind 失败。所以过线
	// 的是白名单而不是已建好的通道。Proxy 仍然是路径——但它只是沙箱里的
	// 挂载点，socket 文件由持有沙箱的那一侧创建。
	//
	// 本地模式下调用方自己建代理并可以顺手填上它；两种模式对后端没有区别，
	// 后端只认 Proxy。
	Egress *EgressSpec
	// Resident 表示这是长驻进程（插件运行时）：不施加墙钟上限，
	// 生命周期由调用方 Close 决定，而不是靠 Timeout 杀掉。
	Resident bool
}

// EgressSpec 是出网白名单，与 Spec.Proxy 配对使用。
//
// 分成三种名单而不是一张表：HTTP 请求、CONNECT 隧道、允许的方法各有各的
// 判定口径，合成一张就只能取最宽的那个。
type EgressSpec struct {
	// Hosts 是 HTTP 请求的目标域名（absolute-form 与 CONNECT 之外）。
	Hosts []string
	// WSHosts 是允许 CONNECT 隧道的域名（清单的 ws.connect）。
	WSHosts []string
	// Verbs 是允许的 HTTP 方法；空表示不限制。
	Verbs []string
	// HostPorts 是精确到端口的白名单（"host:port"），只有平台自己注入的
	// 宿主地址会落在这里：插件需要回调宿主（im.reply 的 inbox），而宿主
	// 地址是部署事实，清单里既写不出来（hosts 只收域名），也不该由作者写。
	HostPorts []string
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
	// Sandbox 是实际生效的后端名：bwrap / local / none。
	Sandbox string
}

// Runner 是执行器。实现必须保证：超时后整棵进程树被回收，不留僵尸。
type Runner interface {
	// Name 是后端名，调用方用它判断是否真的隔离了。
	Name() string
	// Executable 把命令名解析成「执行体所在文件系统」上的可执行路径，
	// 解析不到返回空串。
	//
	// 为什么必须由执行器回答：容器化之后控制面与执行体不是同一个文件系统。
	// 控制面跑在 macOS 上时 exec.LookPath("deno") 给出 /Users/.../bin/deno，
	// 这个路径在执行容器里不存在，现象是 `bwrap: execvp ...: No such file or
	// directory`——进程起来就退，宿主只读到 EOF，看日志像「插件自己崩了」。
	// 所以过线的只能是命令名，由执行体按容器里的 PATH 解析。
	Executable(name string) string
	Run(ctx context.Context, spec Spec) (Result, error)
	// Start 启动一个长驻进程，暴露 stdin/stdout 供双向协议使用（插件运行时）。
	// 超时、进程组回收、环境清理与 Run 一致；调用方必须 Close。
	Start(ctx context.Context, spec Spec) (*Proc, error)
}

// Proc 是一次长驻进程。它只负责进程生命周期，不解析协议。
//
// stdout 不做字节上限：长连接下每帧大小与帧总数由调用方（插件 runtime）
// 控制，沙箱层再做一次截断会把合法的长会话误杀。stderr 仍然限量收集，
// 用于失败时回传原因。
type Proc struct {
	Stdout  *bufio.Reader
	Stdin   io.WriteCloser
	name    string
	cmd     *exec.Cmd
	stderr  *capWriter
	runCtx  context.Context
	cancel  context.CancelFunc
	cleanup func()
	start   time.Time
	exited  chan struct{}
	waitErr error
	once    sync.Once
	// remote 表示这是一个远端句柄：没有 cmd，终止靠 closeFn 断连接。
	remote  bool
	closeFn func()
}

// NewProc 用外部管道构造一个长驻进程句柄，供远端执行体实现使用。
//
// 为什么必须从这里构造：Proc 的字段绑在 *exec.Cmd 上，而远端实现手里
// 只有两条流。让它们各自拼一个 Proc 就会漏掉 Close / Wait / Kill 的
// 回收语义，而这正是上层（常驻池）唯一依赖的部分。
//
// closeFn 负责通知对端终止进程（关连接），必须可重复调用。
func NewProc(name string, stdout io.Reader, stdin io.WriteCloser, closeFn func()) *Proc {
	p := &Proc{
		Stdout:  bufio.NewReaderSize(stdout, 64<<10),
		Stdin:   stdin,
		name:    name,
		runCtx:  context.Background(),
		cancel:  func() {},
		cleanup: func() {},
		start:   time.Now(),
		remote:  true,
		closeFn: closeFn,
		exited:  make(chan struct{}),
	}
	// 远端进程的退出由流断开来表达，没有可等待的 exec.Cmd：
	// 预关闭 exited 让 Wait 立刻返回，而不是永久阻塞。
	close(p.exited)
	return p
}

// Write 写入一个协议帧；调用方负责加换行。
func (p *Proc) Write(b []byte) error {
	if p.Stdin == nil {
		return errors.New("sandbox: 进程 stdin 不可写")
	}
	_, err := p.Stdin.Write(b)
	return err
}

// Name 返回实际生效的后端名（ModeFull 下为 none）。
func (p *Proc) Name() string { return p.name }

// Stderr 返回目前已收集的错误输出。
//
// 远端句柄没有本地管道可收集：错误由流断开时带回，这里返回空串。
func (p *Proc) Stderr() string {
	if p.stderr == nil {
		return ""
	}
	return p.stderr.String()
}

// Kill 立即终止整棵进程树；远端句柄等价于断开连接。
func (p *Proc) Kill() {
	if p.remote {
		if p.closeFn != nil {
			p.closeFn()
		}
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Wait 等待进程退出，返回退出码与耗时。
//
// 与 Run 一致：非零退出码不是 error，超时 / 输出超限才是。
func (p *Proc) Wait() (Result, error) {
	<-p.exited
	res := Result{
		Stderr:   p.Stderr(),
		Duration: time.Since(p.start),
		Sandbox:  p.name,
	}
	if p.runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%w", ErrTimeout)
	}
	if p.waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(p.waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("启动命令失败: %w", p.waitErr)
	}
	return res, nil
}

// Close 关闭 stdin、回收进程与临时目录，可重复调用。
func (p *Proc) Close() {
	p.once.Do(func() {
		if p.Stdin != nil {
			_ = p.Stdin.Close()
		}
		p.Kill()
		<-p.exited
		p.cancel()
		p.cleanup()
	})
}

// 后端名。判定「有没有真隔离」是安全语义的一部分，不要用字面量比较。
//
// 只有 bwrap 一种隔离后端：路径虚拟化需要挂载命名空间，sandbox-exec 做不到，
// 而它本来就是「开发机自动探测」时代的遗留（该档位已删除）。
const (
	NameBwrap = "bwrap"
	NameLocal = "local"
)

// ErrNotIsolated 表示当前没有可用的隔离后端，只有无隔离的 local。
var ErrNotIsolated = errors.New("sandbox: 没有可用的隔离后端")

// Detect 返回当前平台最强的可用后端；都不可用时回退 local。
// 只给测试与本机工具用：控制面（internal/app）不得调用它——
// 在服务端执行 agent 一律走远端执行体，形态不能由「这台机器装了什么」决定。
func Detect() Runner {
	if r, err := NewBwrap(); err == nil {
		return r
	}
	return NewLocal()
}

// Ensure 校验执行面真的是隔离后端，降级到 local 直接报错，
// 把「以为有沙箱、其实在裸跑」挡在启动阶段，而不是等出事才发现。
// 没有「允许降级」的参数：控制面不接受无隔离执行面，任何形态都不例外。
func Ensure(r Runner) error {
	if r == nil {
		return fmt.Errorf("%w：没有可用的执行器", ErrNotIsolated)
	}
	if r.Name() == NameLocal {
		return fmt.Errorf("%w：探测不到 bwrap（Linux 需安装 bubblewrap 且内核允许 user namespace）",
			ErrNotIsolated)
	}
	return nil
}

// NewLocal 是不做任何隔离的执行器，只保留超时 / 输出上限 / 环境清理。
// 只用于测试与本机工具，不是控制面的可选执行面。
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

// Executable 在本机解析：local / bwrap 后端与调用方同一个文件系统，绝对路径才对。
func (r *runner) Executable(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

func (r *runner) Run(ctx context.Context, spec Spec) (Result, error) {
	spec, err := spec.normalize()
	if err != nil {
		return Result{Sandbox: r.name}, err
	}
	p, err := r.prepare(spec)
	if err != nil {
		return Result{Sandbox: r.name}, err
	}
	defer p.cleanup()

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := p.command(runCtx)
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	stdout := newCapWriter(spec.MaxOutput, cancel)
	stderr := newCapWriter(spec.MaxOutput, cancel)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	res := Result{
		Sandbox:   p.name,
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

// Start 启动长驻进程，供插件 runtime 做 NDJSON 双向协议。
//
// spec.Stdin 若有内容，会在启动后先写入再关闭——这样一次性输入的插件
// 与长连接插件共用同一套调用约定。
func (r *runner) Start(ctx context.Context, spec Spec) (*Proc, error) {
	spec, err := spec.normalize()
	if err != nil {
		return nil, err
	}
	p, err := r.prepare(spec)
	if err != nil {
		return nil, err
	}

	// 长驻进程不给墙钟：插件进程要在多次调用之间活着，
	// 用 ctx 超时杀它会退化成「一次调用一进程」，也就没有常驻的意义了。
	runCtx, cancel := context.Background(), context.CancelFunc(func() {})
	if !spec.Resident {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	cmd := p.command(runCtx)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		p.cleanup()
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		p.cleanup()
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr := newCapWriter(spec.MaxOutput, cancel)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		cancel()
		p.cleanup()
		return nil, fmt.Errorf("启动命令失败: %w", err)
	}

	proc := &Proc{
		Stdout:  bufio.NewReaderSize(stdoutPipe, 64<<10),
		Stdin:   stdin,
		name:    p.name,
		cmd:     cmd,
		stderr:  stderr,
		runCtx:  runCtx,
		cancel:  cancel,
		cleanup: p.cleanup,
		start:   time.Now(),
		exited:  make(chan struct{}),
	}
	go func() {
		proc.waitErr = cmd.Wait()
		close(proc.exited)
	}()

	if len(spec.Stdin) > 0 {
		go func() {
			_, _ = stdin.Write(spec.Stdin)
			_ = stdin.Close()
		}()
	}
	return proc, nil
}

// prepared 是一次执行的静态部分：命令、环境、工作目录与临时目录。
type prepared struct {
	argv    []string
	env     []string
	dir     string
	name    string
	cleanup func()
}

func (r *runner) prepare(spec Spec) (prepared, error) {
	// 每次调用一个私有临时目录：TMPDIR / HOME 都指向它，结束即删。
	// 这样 read-only 档位下命令依然能写临时文件，但不会残留到下一次调用。
	tmp, err := os.MkdirTemp("", "sandbox-")
	if err != nil {
		return prepared{}, fmt.Errorf("创建临时目录失败: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	// 出网不再建代理：share-net 下沙箱可以直接绕过 HTTP_PROXY 直连，
	// 应用层白名单只是「默认路径可达」，当成边界会误导。真正的封禁在
	// NetworkPolicy（私网段不可达），数据面靠「网络工具先审批」。

	// macOS 上 /var 是指向 /private/var 的符号链接，内核按真实路径匹配策略，
	// 不先解开就会出现「明明放开了写权限却 Operation not permitted」。
	if real, evalErr := filepath.EvalSymlinks(tmp); evalErr == nil {
		tmp = real
	}
	// 有挂载命名空间时 HOME/TMPDIR 指向沙箱内的固定路径：宿主的临时目录
	// （/Users/.../TMPDIR、/var/folders/...）不进入任何环境变量与报错。
	virtual := r.build != nil && spec.Mode != ModeFull
	home := tmp
	dir := spec.realDir(tmp)
	if virtual {
		home = HomeDir
		// 外层进程不需要工作目录：bwrap 自己 --chdir 到沙箱内路径。
		dir = ""
	}
	env := buildEnv(spec, home)
	name := r.name
	argv := spec.Cmd
	if spec.Mode == ModeFull {
		// 明确声明不做隔离：审计里记 none，别让人误以为有边界。
		name = "none"
	} else if r.build != nil {
		argv, err = r.build(spec, tmp, env)
		if err != nil {
			cleanup()
			return prepared{}, err
		}
	}
	return prepared{argv: argv, env: env, dir: dir, name: name, cleanup: cleanup}, nil
}

// command 用准备好的静态部分构造 exec.Cmd，统一挂上进程组回收与取消语义。
func (p prepared) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...)
	cmd.Dir = p.dir
	cmd.Env = p.env
	// 独立进程组：超时 / 输出超限时整组 kill，避免脚本 fork 出的子进程变孤儿。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func (s Spec) normalize() (Spec, error) {
	if len(s.Cmd) == 0 || strings.TrimSpace(s.Cmd[0]) == "" {
		return s, fmt.Errorf("命令为空")
	}
	mounts, err := normalizeMounts(s.Mounts)
	if err != nil {
		return s, err
	}
	s.Mounts = mounts
	if s.Dir == "" {
		s.Dir = s.defaultDir()
	}
	if !filepath.IsAbs(s.Dir) || filepath.Clean(s.Dir) != s.Dir {
		return s, fmt.Errorf("工作目录 %q 必须是沙箱内的绝对路径（如 %s）", s.Dir, WorkspaceDir)
	}
	if s.Mode == "" {
		s.Mode = ModeReadOnly
	}
	switch s.Mode {
	case ModeReadOnly, ModeWorkspaceWrite, ModeSystemWrite, ModeFull:
	default:
		return s, fmt.Errorf("未知的执行模式 %q", s.Mode)
	}
	if s.Timeout <= 0 && !s.Resident {
		// 长驻进程没有「这次执行多久」的概念，不设默认值，
		// 否则 30 秒后 Start 建的进程会被 context 杀掉。
		s.Timeout = DefaultTimeout
	}
	if s.MaxOutput <= 0 {
		s.MaxOutput = DefaultMaxOutput
	}
	if s.Proxy != "" {
		if s.Network {
			return s, fmt.Errorf("Proxy 与 Network 不能同时设置：有代理时必须断网")
		}
		if !filepath.IsAbs(s.Proxy) {
			return s, fmt.Errorf("代理 socket 路径 %q 需要绝对路径", s.Proxy)
		}
	}
	return s, nil
}

// normalizeMounts 校验并去重挂载表：Target 唯一（可写优先），Key 不能越界。
func normalizeMounts(in []Mount) ([]Mount, error) {
	idx := make(map[string]int, len(in))
	out := make([]Mount, 0, len(in))
	for _, m := range in {
		m.Source = strings.TrimSpace(m.Source)
		m.Target = strings.TrimSpace(m.Target)
		rawKey := strings.TrimSpace(m.Key)
		if strings.HasPrefix(rawKey, "/") {
			return nil, fmt.Errorf("挂载 key %q 必须是相对路径", rawKey)
		}
		m.Key = strings.Trim(rawKey, "/")
		if m.Target == "" {
			return nil, fmt.Errorf("挂载点不能为空")
		}
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target || m.Target == "/" {
			return nil, fmt.Errorf("挂载点 %q 必须是干净的绝对路径（且不能是 /）", m.Target)
		}
		switch m.Root {
		case RootStorage, RootCache:
		default:
			return nil, fmt.Errorf("未知的挂载根 %q", m.Root)
		}
		if m.Key == "" && m.Root == RootStorage {
			// 系统路径：两侧路径一致，必须给出本机绝对路径。
			if !filepath.IsAbs(m.Source) {
				return nil, fmt.Errorf("系统挂载 %q 需要绝对路径", m.Target)
			}
		} else if m.Key != "" {
			for _, seg := range strings.Split(m.Key, "/") {
				if seg == ".." {
					return nil, fmt.Errorf("挂载 key %q 不能越界", m.Key)
				}
			}
		}
		// Key 为空且 Root=RootCache 表示「缓存根本身」，允许。
		if m.Source != "" && !filepath.IsAbs(m.Source) {
			return nil, fmt.Errorf("挂载源 %q 必须是绝对路径", m.Source)
		}
		if m.Source != "" {
			if real, err := filepath.EvalSymlinks(m.Source); err == nil {
				m.Source = real
			}
		}
		// 同一个挂载点只留一条：可写的那条赢，否则「先只读后可写」取决于顺序，
		// 而顺序是调用方随手写的。
		if i, ok := idx[m.Target]; ok {
			if m.Write && !out[i].Write {
				out[i] = m
			}
			continue
		}
		idx[m.Target] = len(out)
		out = append(out, m)
	}
	return out, nil
}

// defaultDir 没有显式指定工作目录时用工作区挂载点；没有工作区就是根。
func (s Spec) defaultDir() string {
	for _, m := range s.Mounts {
		if m.Target == WorkspaceDir {
			return WorkspaceDir
		}
	}
	return "/"
}

// realDir 是没有挂载命名空间时（local 后端 / ModeFull）的进程工作目录。
// 沙箱内的虚拟路径在那种模式下不存在，必须落回真实路径。
func (s Spec) realDir(fallback string) string {
	for _, m := range s.Mounts {
		if m.Target == s.Dir && m.Source != "" {
			return m.Source
		}
	}
	return fallback
}

// interpreterDir 返回可执行文件的绝对目录；命令是相对名（依赖 PATH）时返回空。
func interpreterDir(bin string) string {
	if bin == "" {
		return ""
	}
	if filepath.IsAbs(bin) {
		return filepath.Dir(bin)
	}
	if abs, err := exec.LookPath(bin); err == nil {
		return filepath.Dir(abs)
	}
	return ""
}

// bind 是一次 bind mount 的参数对：源（宿主真实路径）与目标（沙箱内路径）。
type bind struct {
	source string
	target string
}

// plan 把 Spec 与后端基线合成最终的读 / 写挂载表。
//
// 去重后排序：后端命令行的稳定性对排障很重要（同样的 Spec 应该得到同样的 argv），
// 也让单测可以直接比字符串。
func (s Spec) plan(baseline []string) (read, write []bind) {
	for _, p := range baseline {
		read = append(read, bind{p, p})
	}
	// 解释器本身必须在沙箱里可见：deno 装在 /opt/deno/bin 这类非基线路径时，
	// 不自动加进去会得到「找不到文件」这种与权限完全无关、极难排查的报错。
	// 在执行的这一侧解析，所以不用过线。
	for _, p := range ExtraRead() {
		read = append(read, bind{p, p})
	}
	if dir := interpreterDir(s.Cmd[0]); dir != "" {
		read = append(read, bind{dir, dir})
	}
	writable := s.Mode == ModeWorkspaceWrite || s.Mode == ModeSystemWrite
	for _, m := range s.Mounts {
		if m.Write && writable {
			write = append(write, bind{m.Source, m.Target})
			continue
		}
		read = append(read, bind{m.Source, m.Target})
	}
	// 系统安装档位：可写表追加 /usr、/var/lib/dpkg 等。
	// 后挂的 --bind 会覆盖前面同路径的 --ro-bind，这是它能在
	// "--remount-ro /" 之后仍然可写的原因。
	//
	// 注意这里刻意不做空间级隔离：可写层就是执行体自己的可写层，
	// 空间之间共用。隔离靠的是「重建即回到基础镜像 + 安装命中租户缓存」，
	// 不是靠每个空间一份 /usr。
	if s.Mode == ModeSystemWrite {
		for _, p := range SystemWritePaths() {
			write = append(write, bind{p, p})
		}
	}
	return dedupeBind(read), dedupeBind(write)
}

// dedupeBind 去重并排序：同一目标只留一条（可写表里重复的源按目标合并）。
func dedupeBind(in []bind) []bind {
	seen := make(map[string]struct{}, len(in))
	out := make([]bind, 0, len(in))
	for _, b := range in {
		if b.source == "" || b.target == "" {
			continue
		}
		key := b.source + "\x00" + b.target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].source != out[j].source {
			return out[i].source < out[j].source
		}
		return out[i].target < out[j].target
	})
	return out
}

// extraRead 是运维补丁位：白名单化之后，装在非标准位置的运行时
// （npm 装的 deno 是 node 包装脚本、自定义 node 发行版、内网镜像的 python）
// 需要把安装前缀挂进来，否则会得到「找不到模块」这类与权限无关的报错。
//
// 只在启动时设置一次；放全局是因为它属于「这台机器的环境事实」，
// 而不是某一次调用的参数。
var (
	extraReadMu sync.RWMutex
	extraRead   []string
)

// SetExtraRead 设置全局额外只读路径，传空清空。
func SetExtraRead(paths []string) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" && filepath.IsAbs(p) {
			clean = append(clean, filepath.Clean(p))
		}
	}
	extraReadMu.Lock()
	extraRead = clean
	extraReadMu.Unlock()
}

// ExtraRead 返回当前全局额外只读路径（拷贝，调用方可安全修改）。
func ExtraRead() []string {
	extraReadMu.RLock()
	defer extraReadMu.RUnlock()
	return append([]string(nil), extraRead...)
}

func sortedUnique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// baseEnvKeys 是允许从父进程继承的环境变量：都是无害的本地化 / 路径信息。
// 其余（含各类 *_KEY / *_TOKEN）一律不带，避免沙箱里的 cat / env 顺手把密钥读走。
var baseEnvKeys = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	// 安装源：镜像里写死的国内镜像地址。不带进沙箱，模型装老版本时会去连官方源，
	// 在断网 / 白名单下表现为「卡住直到超时」，比直接报错更难排查。
	// 只放地址，不放 MISE_DATA_DIR / MISE_CONFIG_DIR 这类目录变量：
	// 那些必须由调用方指到可写位置，从父进程继承过来只会指到只读的系统目录。
	"NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "MISE_NODE_MIRROR",
}

const fallbackPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// FallbackPath 是容器里最保守的 PATH。
//
// 调用方要往 PATH 前面拼自己的目录（venv、npm 全局包）时，
// 必须以它为尾巴：--clearenv 之后 PATH 不一定能从父进程继承到，
// 直接覆盖成一个只有私有目录的值会让 /bin/sh 都找不到。
func FallbackPath() string { return fallbackPath }

func buildEnv(spec Spec, home string) []string {
	env := []string{
		"HOME=" + home,
		"TMPDIR=" + home,
		"TERM=dumb",
		// 关掉语言运行时往 HOME 之外写缓存的行为，减少只读档位下的假失败
		"XDG_CACHE_HOME=" + home,
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
	if spec.Proxy != "" {
		// 约定变量：插件与依赖从这里拿到唯一的出网地址。指向沙箱内的固定路径，
		// 不是建 socket 那一侧的临时路径。
		env = append(env, "WA_EGRESS_PROXY="+EgressProxy)
	}
	return mergeEnv(env, spec.Env)
}

// mergeEnv 让 override 覆盖 base 里的同名键。
//
// 不能简单 append：execve 收到的是一维数组，getenv 取第一个匹配，
// 后追加的同名项在 glibc / musl 下都不生效——表现是「设了但程序不认」。
// 之前 XDG_CACHE_HOME 就栽在这上面：默认值指向私有 tmp，
// 调用方想把它挪到租户缓存目录，结果缓存还是每次重下。
func mergeEnv(base, override []string) []string {
	if len(override) == 0 {
		return base
	}
	idx := make(map[string]int, len(base))
	for i, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok {
			idx[k] = i
		}
	}
	for _, kv := range override {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if i, exists := idx[k]; exists {
			base[i] = kv
			continue
		}
		idx[k] = len(base)
		base = append(base, kv)
	}
	return base
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
