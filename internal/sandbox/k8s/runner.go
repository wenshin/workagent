package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/sandbox/remote"
)

// Runner 是容器化执行面：每次调用先 ensure 出对应容器，再交给容器内的
// sandbox-agent。它自己不执行命令，也不判断策略。
type Runner struct {
	cfg Config
	cs  kubernetes.Interface
	log *slog.Logger
	// rc 是建隧道用的 REST 配置（外部注入客户端时为空，就只能直连）。
	rc *rest.Config

	mu      sync.Mutex
	conns   map[string]*conn
	tenants map[string]string
	// active 是在跑的调用数：一条命令跑得比空闲 TTL 还久时，
	// 回收协程不能把它当成空转容器删掉。
	active  map[string]int
	backend string
	facts   sandbox.EnvFacts

	stopOnce sync.Once
	stop     chan struct{}
}

// conn 是「一个 Pod」对应的远端客户端。
//
// 必须带 uid：Pod 被回收后重建会复用名字，只按名字缓存会一直往旧 IP 发请求。
type conn struct {
	uid      string
	ip       string
	client   *remote.Client
	lastUsed time.Time
	// tunnel 非空说明这条连接走的是 API Server 端口转发，换 Pod 时要关掉。
	tunnel io.Closer
}

type target struct {
	kind       sandbox.Kind
	space      string
	tenant     string
	tenantHash string
	name       string
}

// New 构造调度层并启动空间容器回收协程。
func New(cfg Config) (*Runner, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	var rc *rest.Config
	cs := cfg.Client
	if cs == nil {
		var err error
		rc, err = restConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("连接集群失败: %w", err)
		}
		// 每次调用都要 GET 一次 Pod 校验 UID / IP，默认 QPS 5 会成为瓶颈。
		rc.QPS, rc.Burst = 50, 100
		cs, err = kubernetes.NewForConfig(rc)
		if err != nil {
			return nil, fmt.Errorf("连接集群失败: %w", err)
		}
	}
	r := &Runner{
		cfg:     cfg,
		cs:      cs,
		rc:      rc,
		log:     cfg.Log,
		conns:   map[string]*conn{},
		tenants: map[string]string{},
		active:  map[string]int{},
		backend: "k8s",
		stop:    make(chan struct{}),
	}
	go r.reapLoop()
	r.log.Info("容器化执行面已就绪",
		"namespace", cfg.Namespace, "image", cfg.Image,
		"storage_pvc", cfg.StoragePVCName, "shell_idle", cfg.ShellIdle)
	return r, nil
}

// restConfig 依次尝试三种凭据来源：
//
//  1. SANDBOX_KUBECONFIG 显式指定的路径；
//  2. Pod 内的 ServiceAccount（in-cluster）；
//  3. 本机 kubeconfig（$KUBECONFIG 或 ~/.kube/config）。
//
// 第 3 条是给 make run / CI 用的：本机进程不在集群里，in-cluster 必然失败，
// 若直接把它当成结论，报错只有「KUBERNETES_SERVICE_HOST 未定义」，
// 看不出该配什么。
func restConfig(cfg Config) (*rest.Config, error) {
	if p := strings.TrimSpace(cfg.Kubeconfig); p != "" {
		rc, err := clientcmd.BuildConfigFromFlags("", p)
		if err != nil {
			return nil, fmt.Errorf("读取 SANDBOX_KUBECONFIG=%s 失败: %w", p, err)
		}
		return rc, nil
	}
	if rc, err := rest.InClusterConfig(); err == nil {
		return rc, nil
	}
	if rc, err := localKubeconfig(); err == nil {
		return rc, nil
	}
	return nil, errors.New("没有可用的集群凭据：既不在集群内，也没找到 kubeconfig；" +
		"本机调试请设置 SANDBOX_KUBECONFIG（或把集群上下文写进 ~/.kube/config）")
}

// localKubeconfig 按 kubectl 的规则找本机配置：KUBECONFIG（支持多路径）→ ~/.kube/config。
//
// 没有配置时 client-go 可能返回一份 Host 为空的 Config 而不报错，所以要显式检查 Host。
func localKubeconfig() (*rest.Config, error) {
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{})
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	if rc == nil || strings.TrimSpace(rc.Host) == "" {
		return nil, errors.New("kubeconfig 里没有可用的集群")
	}
	return rc, nil
}

// Name 返回调度形态。真实的沙箱后端由每次握手校验（见 clientFor），
// 审计里的后端名取 Result.Sandbox，不取这里。
func (r *Runner) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.backend
}

// Transport 仅用于日志。
func (r *Runner) Transport() string { return "k8s" }

// Env 返回最近一次握手拿到的执行环境自述，实现 sandbox.EnvProvider。
// 第一个容器就绪之前为空，提示词里会整段省略执行环境一节。
func (r *Runner) Env() sandbox.EnvFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.facts
}

// Close 停止回收协程；已建的 Pod 留给 k8s（或下一次启动按空闲回收）。
func (r *Runner) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	for name, e := range r.conns {
		if e.tunnel != nil {
			_ = e.tunnel.Close()
		}
		delete(r.conns, name)
	}
	r.mu.Unlock()
}

// Executable 返回裸命令名：命令在执行体容器里跑，由容器里的 PATH 解析。
func (r *Runner) Executable(name string) string { return name }

// Run 在一个空间 / 租户容器里执行一次命令。
func (r *Runner) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	c, name, err := r.clientFor(ctx, spec.Scope)
	if err != nil {
		return sandbox.Result{Sandbox: r.Name()}, err
	}
	r.begin(name)
	defer r.end(name)

	res, err := c.Run(ctx, spec)
	if err != nil {
		// 容器在两次调用之间被回收 / 驱逐时，缓存里的 IP 已经失效。
		// 只清缓存不重放：命令可能已经执行过，重跑会重复副作用。
		r.forget(c)
	}
	return res, err
}

// Start 在容器里起一个常驻进程（插件运行时）。
func (r *Runner) Start(ctx context.Context, spec sandbox.Spec) (*sandbox.Proc, error) {
	c, _, err := r.clientFor(ctx, spec.Scope)
	if err != nil {
		return nil, err
	}
	proc, err := c.Start(ctx, spec)
	if err != nil {
		r.forget(c)
	}
	return proc, err
}

func (r *Runner) begin(name string) {
	r.mu.Lock()
	r.active[name]++
	r.mu.Unlock()
}

func (r *Runner) end(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[name] > 1 {
		r.active[name]--
	} else {
		delete(r.active, name)
	}
	if e := r.conns[name]; e != nil {
		e.lastUsed = time.Now()
	}
}

func (r *Runner) clientFor(ctx context.Context, scope sandbox.Scope) (*remote.Client, string, error) {
	t, err := r.resolve(ctx, scope)
	if err != nil {
		return nil, "", err
	}
	// 首次创建容器要等调度 + 拉镜像，可能比单条命令的超时长得多。
	// 用独立超时，否则「空间里第一次调用必然超时」，第二次才成功。
	ensureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.ReadyTimeout+30*time.Second)
	defer cancel()

	// 存储卷必须存在：控制面把文件写在同一个卷上，容器里没有这个卷时
	// 文件工具会报「找不到文件」，比「卷不存在」更难查。
	if err := r.ensurePVC(ensureCtx, r.buildStoragePVC()); err != nil {
		return nil, "", fmt.Errorf("准备存储卷失败: %w", err)
	}
	if r.cfg.CacheShared {
		if err := r.ensurePVC(ensureCtx, r.buildCachePVC(t.tenant)); err != nil {
			return nil, "", fmt.Errorf("准备租户缓存卷失败: %w", err)
		}
	}

	pod, err := r.ensurePod(ensureCtx, t)
	if err != nil {
		return nil, "", err
	}
	ip := pod.Status.PodIP
	if ip == "" {
		return nil, "", fmt.Errorf("执行体 %s 已就绪但没有 Pod IP", pod.Name)
	}

	r.mu.Lock()
	if e := r.conns[t.name]; e != nil && e.uid == string(pod.UID) && e.ip == ip {
		e.lastUsed = time.Now()
		c := e.client
		r.mu.Unlock()
		return c, t.name, nil
	}
	r.mu.Unlock()

	c, tunnel, err := r.dial(ensureCtx, pod)
	if err != nil {
		return nil, "", err
	}
	// 执行体自报不是真隔离时不能继续：容器边界不是「有隔离」的证据，
	// 镜像里没有 bwrap 一样会变成裸跑。
	if err := sandbox.Ensure(c); err != nil {
		return nil, "", fmt.Errorf("执行体 %s 没有可用的隔离后端: %w", pod.Name, err)
	}

	r.mu.Lock()
	e := r.conns[t.name]
	if e == nil {
		e = &conn{}
		r.conns[t.name] = e
	} else if e.tunnel != nil {
		// 换 Pod / 换 IP：旧隧道不能留着，否则每换一次就漏一条 SPDY 连接。
		_ = e.tunnel.Close()
	}
	e.uid, e.ip, e.client, e.lastUsed, e.tunnel = string(pod.UID), ip, c, time.Now(), tunnel
	r.backend = c.Name()
	r.facts = c.Env()
	r.mu.Unlock()
	return c, t.name, nil
}

// dial 连接执行体并完成握手，返回可选的隧道句柄（非空说明走了端口转发）。
//
// 先直连 Pod IP。直连是传输层错误（no route to host / 连接被拒）时退回经
// API Server 的端口转发：Pod 路由只对集群内调用方天然成立，控制面在宿主机上
// （make run / CI）时，本机网络策略、CNI 转发、macOS 的本地网络权限都会让
// dial 一直失败，而「本机 → API Server」这条路是通的（否则建不出 Pod）。
// 鉴权 / 协议错误不换传输方式，换了也一样失败。
func (r *Runner) dial(ctx context.Context, pod *corev1.Pod) (*remote.Client, io.Closer, error) {
	ip := pod.Status.PodIP
	if ip == "" {
		return nil, nil, fmt.Errorf("执行体 %s 已就绪但没有 Pod IP", pod.Name)
	}
	addr := fmt.Sprintf("%s:%d", ip, r.cfg.RunnerPort)
	// 先用短超时探一下 TCP：IP 被静默丢弃时，握手的 10 秒超时会白等。
	cause := probeTCP(ctx, addr, 2*time.Second)
	var c *remote.Client
	if cause == nil {
		var err error
		c, err = remote.New("http://"+addr, r.cfg.Token)
		if err == nil {
			return c, nil, nil
		}
		if !isDialError(err) {
			return nil, nil, fmt.Errorf("连接执行体 %s 失败: %w", pod.Name, err)
		}
		cause = err
	}

	// 换传输方式要留痕：否则「多花了 2 秒」和「到底走了哪条路」都只能靠猜。
	r.log.Warn("直连执行体不通，改用 API Server 隧道", "pod", pod.Name, "ip", ip, "err", cause)

	tunnel, port, terr := r.openTunnel(pod)
	if terr != nil {
		return nil, nil, fmt.Errorf("连接执行体 %s 失败：直连 %s 不通（%v），隧道也不可用（%v）",
			pod.Name, ip, cause, terr)
	}
	tunneled, err := remote.New(fmt.Sprintf("http://127.0.0.1:%d", port), r.cfg.Token)
	if err != nil {
		_ = tunnel.Close()
		return nil, nil, fmt.Errorf("连接执行体 %s 失败（经隧道）: %w", pod.Name, err)
	}
	return tunneled, tunnel, nil
}

// probeTCP 用短超时探一次连通性，失败即返回原因。
func probeTCP(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// isDialError 判断是不是传输层错误（no route to host、连接被拒、握手超时）。
// HTTP 状态码、鉴权、协议错误都不算。
func isDialError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}

// forget 按客户端实例清缓存：插件容器的名字由租户决定，
// 从 Scope 反推名字会算错，直接认指针。
func (r *Runner) forget(c *remote.Client) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, e := range r.conns {
		if e.client == c {
			if e.tunnel != nil {
				_ = e.tunnel.Close()
			}
			delete(r.conns, name)
			return
		}
	}
}

// resolve 把 Scope 换算成容器目标。租户未知时退化为按空间隔离，
// 不把两个企业的插件进程塞进同一个 Pod。
func (r *Runner) resolve(ctx context.Context, scope sandbox.Scope) (target, error) {
	space := strings.TrimSpace(scope.Space)
	if space == "" {
		return target{}, errors.New("执行体缺少空间归属：Spec.Scope.Space 不能为空")
	}
	kind := scope.Kind
	if kind == "" {
		kind = sandbox.KindShell
	}
	if kind != sandbox.KindShell && kind != sandbox.KindPlugin {
		return target{}, fmt.Errorf("未知执行体种类 %q：只能是 shell / plugin", kind)
	}

	tenant := strings.TrimSpace(scope.Tenant)
	if tenant == "" {
		tenant = r.tenantOf(ctx, space)
	}
	if tenant == "" {
		tenant = space
	}
	key := space
	if kind == sandbox.KindPlugin {
		key = tenant
	}
	return target{
		kind:       kind,
		space:      space,
		tenant:     tenant,
		tenantHash: shortHash(tenant),
		name:       podNameFor(kind, key),
	}, nil
}

func (r *Runner) tenantOf(ctx context.Context, space string) string {
	r.mu.Lock()
	if v, ok := r.tenants[space]; ok {
		r.mu.Unlock()
		return v
	}
	r.mu.Unlock()

	if r.cfg.TenantOf == nil {
		return ""
	}
	tenant, err := r.cfg.TenantOf(ctx, space)
	if err != nil {
		r.log.Warn("解析租户失败，插件容器退化为按空间隔离", "space", space, "err", err)
		return ""
	}
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		// 解析不出来不要缓存成空值：下次可能查得到，
		// 缓存空值会把「一次数据库抖动」固化成永久的按空间隔离。
		return ""
	}
	r.mu.Lock()
	r.tenants[space] = tenant
	r.mu.Unlock()
	return tenant
}

func (r *Runner) ensurePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	_, err := r.cs.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if _, err := r.cs.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensurePod 保证目标 Pod 存在且就绪，返回最新的 Pod。
func (r *Runner) ensurePod(ctx context.Context, t target) (*corev1.Pod, error) {
	pod, err := r.getPod(ctx, t.name)
	switch {
	case apierrors.IsNotFound(err):
		return r.createAndWait(ctx, t)
	case err != nil:
		return nil, err
	}

	if pod.DeletionTimestamp != nil {
		if err := r.waitGone(ctx, t.name); err != nil {
			return nil, err
		}
		return r.createAndWait(ctx, t)
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		// 容器退出后 Pod 不会自己回来：restartPolicy 只管容器，不管 Pod。
		if err := r.deletePod(ctx, t.name); err != nil {
			return nil, err
		}
		if err := r.waitGone(ctx, t.name); err != nil {
			return nil, err
		}
		return r.createAndWait(ctx, t)
	}
	if err := r.waitReady(ctx, t.name); err != nil {
		return nil, err
	}
	return r.getPod(ctx, t.name)
}

func (r *Runner) createAndWait(ctx context.Context, t target) (*corev1.Pod, error) {
	pod := r.buildPod(t)
	if _, err := r.cs.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("创建执行体 %s 失败: %w", pod.Name, err)
	}
	if err := r.waitReady(ctx, pod.Name); err != nil {
		return nil, err
	}
	return r.getPod(ctx, pod.Name)
}

func (r *Runner) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return r.cs.CoreV1().Pods(r.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *Runner) deletePod(ctx context.Context, name string) error {
	grace := int64(0)
	err := r.cs.CoreV1().Pods(r.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *Runner) waitGone(ctx context.Context, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err := r.getPod(ctx, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("执行体 %s 没有在 30 秒内终止", name)
}

func (r *Runner) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(r.cfg.ReadyTimeout)
	var last string
	for {
		pod, err := r.getPod(ctx, name)
		if err != nil {
			return err
		}
		switch pod.Status.Phase {
		case corev1.PodFailed, corev1.PodSucceeded:
			return fmt.Errorf("执行体 %s 未能就绪（%s）: %s", name, pod.Status.Phase, podTerminalReason(pod))
		}
		if podReady(pod) && pod.Status.PodIP != "" {
			return nil
		}
		last = podStatusText(pod)
		if time.Now().After(deadline) {
			return fmt.Errorf("等待执行体 %s 就绪超时（%s）：%s", name, r.cfg.ReadyTimeout, last)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podStatusText(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		}
	}
	return string(pod.Status.Phase)
}

func podTerminalReason(pod *corev1.Pod) string {
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return podStatusText(pod)
}

// reapLoop 回收空闲的空间容器。插件容器常驻，永不在这里被删。
func (r *Runner) reapLoop() {
	ticker := time.NewTicker(r.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.reapOnce(context.Background())
		}
	}
}

func (r *Runner) reapOnce(ctx context.Context) {
	sel := fmt.Sprintf("%s=sandbox-runner,%s=shell", labelApp, labelKind)
	list, err := r.cs.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		r.log.Warn("回收空间容器失败（列举）", "err", err)
		return
	}
	now := time.Now()
	for i := range list.Items {
		pod := &list.Items[i]
		idle := now.Sub(pod.CreationTimestamp.Time)

		r.mu.Lock()
		e := r.conns[pod.Name]
		busy := r.active[pod.Name] > 0
		if e != nil {
			idle = now.Sub(e.lastUsed)
		}
		r.mu.Unlock()

		// 正在跑长命令的容器不是空转：删掉等于中断执行。
		if busy || idle < r.cfg.ShellIdle {
			continue
		}
		if err := r.deletePod(ctx, pod.Name); err != nil {
			r.log.Warn("回收空间容器失败", "pod", pod.Name, "err", err)
			continue
		}
		r.mu.Lock()
		if e := r.conns[pod.Name]; e != nil && e.tunnel != nil {
			_ = e.tunnel.Close()
		}
		delete(r.conns, pod.Name)
		r.mu.Unlock()
		r.log.Info("空间容器已回收", "pod", pod.Name, "idle", idle.Round(time.Second))
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
