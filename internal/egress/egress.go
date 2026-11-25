// Package egress 是沙箱唯一的出网通道：宿主侧 SOCKS5 代理 + 域名白名单。
//
// 设计要点（docs/插件安全基线.md §2.3）：
//   - 沙箱保持 --unshare-net，代理以 unix socket 挂进去，是唯一的对外路径；
//   - 域名判定只在这里做一次，插件与依赖拿到的是「要么通、要么不通」；
//   - 连接用「解析→校验全部 IP→直连校验过的 IP」，DNS 结果变化不影响判定；
//   - 一切放行与拒绝都产出审计事件。
//
// 不做透明劫持：那需要 netns 内 REDIRECT + SNI 嗅探，只在确有需求时再上。
package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SOCKS5 常量（RFC 1928）。只实现 CONNECT + 无认证，其余一律拒绝：
// 这个代理是给沙箱用的，能力越小越好。
const (
	socksVersion = 0x05
	cmdConnect   = 0x01
	atypIPv4     = 0x01
	atypDomain   = 0x03
	atypIPv6     = 0x04

	repSuccess        = 0x00
	repGeneralFailure = 0x01
	repNotAllowed     = 0x02
	repHostUnreach    = 0x04
	repCmdNotSupport  = 0x07
	repAddrNotSupport = 0x08
)

// 默认值：握手与建连各自限时，避免半开连接把代理挂住。
const (
	DefaultHandshakeTimeout = 10 * time.Second
	DefaultDialTimeout      = 15 * time.Second
	DefaultMaxConns         = 64
)

// ErrBlocked 表示目标不在白名单内（审计里要能区分「拒绝」和「失败」）。
var ErrBlocked = errors.New("egress: 目标不在白名单内")

// Policy 决定一个域名是否放行。返回值不带错误：解析失败也算不放行。
type Policy interface {
	Allow(host string) bool
}

// Allowlist 是域名白名单。匹配是「域名边界」：
// 写 bocha.com 会放行 bocha.com 与 api.bocha.com，但不会放行 bocha.com.evil.com。
type Allowlist struct {
	domains []string
}

// NewAllowlist 规整白名单：小写、去空格、去前导点。
func NewAllowlist(domains []string) *Allowlist {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		d = strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" {
			out = append(out, d)
		}
	}
	return &Allowlist{domains: out}
}

// Allow 实现 Policy。空白名单等于全部拒绝——默认拒绝是这套设计的出发点。
func (a *Allowlist) Allow(host string) bool {
	host = strings.Trim(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	for _, d := range a.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// HostPorts 是精确到端口的白名单，与 Allowlist 是两套口径：只放行列出的
// 「主机 + 端口」组合，不按域名后缀匹配，也不顺带放行同主机的其他端口。
//
// 它只服务于一种目标——平台自己的接口地址（APP_BASE_URL）。宿主地址是部署
// 事实，插件既无从声明也不该声明；而按域名放行 127.0.0.1 等于把本机所有
// 端口都交给插件，所以这类目标必须精确到端口。
type HostPorts struct {
	pairs map[string]bool
}

// NewHostPorts 规整精确白名单：条目必须是可解析的 host:port。
func NewHostPorts(list []string) *HostPorts {
	pairs := make(map[string]bool, len(list))
	for _, item := range list {
		host, port, err := net.SplitHostPort(strings.TrimSpace(item))
		if err != nil || host == "" || port == "" {
			continue
		}
		if _, err := parsePort(port); err != nil {
			continue
		}
		pairs[hostPortKey(host, port)] = true
	}
	return &HostPorts{pairs: pairs}
}

// Allow 判断某个「主机 + 端口」是否在精确白名单内。
func (h *HostPorts) Allow(host string, port int) bool {
	if h == nil || port <= 0 {
		return false
	}
	return h.pairs[hostPortKey(host, strconv.Itoa(port))]
}

func hostPortKey(host, port string) string {
	return strings.ToLower(strings.Trim(host, ".")) + "|" + port
}

// Event 是一条出网审计。拒绝也要有记录，否则「没日志」会被当成「没发生」。
type Event struct {
	Host     string
	Port     int
	Allowed  bool
	Reason   string
	RemoteIP string
	Duration time.Duration
	// Bytes 是双向传输总量，用于发现异常外传。
	Bytes int64
	Err   error
}

// Options 是代理的装配参数。
type Options struct {
	Policy Policy
	Logger *slog.Logger
	// OnEvent 在每次连接结束时回调，调用方在这里落审计。
	OnEvent func(Event)
	// MaxConns 是并发上限，<=0 用 DefaultMaxConns。
	MaxConns int
	// AllowIPConnect 允许客户端直接给 IP（不走域名）。
	// 默认关闭：IP 直连无法做域名策略，等于绕过白名单。
	AllowIPConnect bool
	// Verbs 是允许的 HTTP 方法（仅 HTTP 代理用）；空表示不限制。
	// 清单里的 verbs 声明不能只用来算风险等级——代理看得见方法，就得管。
	Verbs []string
	// ConnectHosts 是允许 CONNECT 的域名（来自清单的 ws.connect）。
	// 为空表示不允许隧道：默认拒绝，隧道是按插件开关的。
	//
	// 隧道里看不见明文，所以授权口径要收得比普通请求更窄：
	// 只能由清单显式声明，不存在「复用主域名白名单」这种放宽开关。
	ConnectHosts []string
	// HostPorts 是精确到端口的白名单（"host:port"）。它的唯一来源是平台
	// 自己注入的宿主地址，清单里写不出来（清单的 hosts 只收域名）。
	HostPorts []string
	// Dial 只在测试里注入，用来把「拨号」换成连本地 echo 服务；
	// 生产路径永远走 dialChecked（解析后校验全部 IP，私网拒绝）。
	Dial func(ctx context.Context, host string, port int) (net.Conn, error)
}

// Server 是监听在 unix socket 上的 SOCKS5 代理。
type Server struct {
	path     string
	ln       net.Listener
	policy   Policy
	log      *slog.Logger
	onEvent  func(Event)
	sem      chan struct{}
	allowIP  bool
	closeOne sync.Once
	// dialFn 是建连实现，默认走 dial（解析 + 校验 + 直连）。
	// 单独抽出来只为一个用途：测试时注入本地目标，把「策略—应答—转发」整条链路跑通，
	// 而不必绕过 loopback 限制。
	dialFn func(ctx context.Context, host string, port int) (net.Conn, string, error)
}

// ListenTCP 在 TCP 地址上监听。
//
// 与 unix socket 的取舍：unix socket 更严（路径要显式挂进沙箱，父目录不可见），
// 但只有自己实现 HTTP 客户端的运行时（Deno）能用它。mise / npm / curl 这些
// 只认 HTTP_PROXY 里的 http://host:port，而 --unshare-net 的沙箱连 loopback 都
// 不可达，所以这类工具必须配 --share-net + TCP 代理。
//
// 代价要说清：share-net 之后沙箱能绕过代理直连。域名策略因此只在「客户端愿意
// 遵守代理变量」时成立，不是内核级强制。需要硬边界时用 unix socket 那条路。
func ListenTCP(addr string, opt Options) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("监听 TCP 代理失败: %w", err)
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.MaxConns <= 0 {
		opt.MaxConns = DefaultMaxConns
	}
	return &Server{
		ln:      ln,
		policy:  opt.Policy,
		log:     opt.Logger,
		onEvent: opt.OnEvent,
		sem:     make(chan struct{}, opt.MaxConns),
		allowIP: opt.AllowIPConnect,
	}, nil
}

// Addr 返回实际监听地址（端口为 0 时由内核分配，只有这里能拿到真实值）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Listen 在 path 上创建 unix socket 并开始监听。
//
// 目录权限固定 0700：socket 只允许宿主进程与沙箱内进程（同一个 uid）访问。
func Listen(p string, opt Options) (*Server, error) {
	if p == "" {
		return nil, errors.New("egress: 未指定 socket 路径")
	}
	// unix socket 路径有内核长度上限（Linux 108、macOS 104 字节）。
	// 超了会得到 bind: invalid argument 这种完全看不出原因的错，提前挡掉。
	const maxUnixPathLen = 100
	if len(p) > maxUnixPathLen {
		return nil, fmt.Errorf("代理 socket 路径过长（%d 字节，上限 %d）：请改用更短的目录，例如 /tmp/wa-egress.sock", len(p), maxUnixPathLen)
	}

	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建代理目录失败: %w", err)
	}
	// 残留 socket 会让 bind 失败（EADDRINUSE），先清掉。
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("清理旧 socket 失败: %w", err)
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, fmt.Errorf("监听 unix socket 失败: %w", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置 socket 权限失败: %w", err)
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.MaxConns <= 0 {
		opt.MaxConns = DefaultMaxConns
	}
	return &Server{
		path:    p,
		ln:      ln,
		policy:  opt.Policy,
		log:     opt.Logger,
		onEvent: opt.OnEvent,
		sem:     make(chan struct{}, opt.MaxConns),
		allowIP: opt.AllowIPConnect,
	}, nil
}

// Path 返回 unix socket 路径：沙箱侧要把同一个路径挂进去并据此授权。
// TCP 模式下为空，调用方应改用 Addr。
func (s *Server) Path() string { return s.path }

// Serve 接受连接直到 ctx 结束或 listener 被关闭。
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
		default:
			// 并发打满时直接断开：让插件看到明确失败，而不是无限等待。
			_ = conn.Close()
			continue
		}
		go func(c net.Conn) {
			defer func() { <-s.sem }()
			s.handle(ctx, c)
		}(conn)
	}
}

// Close 关闭监听并删除 socket 文件。
func (s *Server) Close() error {
	var err error
	s.closeOne.Do(func() {
		err = s.ln.Close()
		if s.path != "" {
			_ = os.Remove(s.path)
		}
	})
	return err
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	defer client.Close()
	start := time.Now()

	host, port, err := s.handshake(client)
	if err != nil {
		// 握手阶段就失败：没有可信目标可审计，只记录原因。
		if !errors.Is(err, io.EOF) {
			s.emit(Event{Allowed: false, Reason: "handshake", Duration: time.Since(start), Err: err})
		}
		return
	}

	ev := Event{Host: host, Port: port, Duration: time.Since(start)}
	if s.policy == nil || !s.policy.Allow(host) {
		ev.Allowed = false
		ev.Reason = "not_allowlisted"
		_ = writeReply(client, repNotAllowed)
		s.emit(ev)
		return
	}

	upstream, ip, err := s.dialUp(ctx, host, port)
	if err != nil {
		ev.Allowed = false
		ev.Reason = "dial_failed"
		ev.Err = err
		_ = writeReply(client, repHostUnreach)
		s.emit(ev)
		return
	}
	defer upstream.Close()

	if err := writeReply(client, repSuccess); err != nil {
		ev.Err = err
		s.emit(ev)
		return
	}
	ev.Allowed = true
	ev.RemoteIP = ip
	n := pipe(client, upstream)
	ev.Duration = time.Since(start)
	ev.Bytes = n
	s.emit(ev)
}

func (s *Server) dialUp(ctx context.Context, host string, port int) (net.Conn, string, error) {
	if s.dialFn != nil {
		return s.dialFn(ctx, host, port)
	}
	return s.dial(ctx, host, port)
}

// handshake 完成 SOCKS5 协商并读出目标地址。
func (s *Server) handshake(c net.Conn) (string, int, error) {
	_ = c.SetDeadline(time.Now().Add(DefaultHandshakeTimeout))
	defer func() { _ = c.SetDeadline(time.Time{}) }()

	// 版本 + 方法列表
	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return "", 0, err
	}
	if head[0] != socksVersion {
		return "", 0, fmt.Errorf("不支持的协议版本 %d", head[0])
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", 0, err
	}
	// 只接受「无认证」：代理不持有任何身份，认证在上游域名策略里。
	if !containsByte(methods, 0x00) {
		_, _ = c.Write([]byte{socksVersion, 0xFF})
		return "", 0, errors.New("客户端未提供无认证方式")
	}
	if _, err := c.Write([]byte{socksVersion, 0x00}); err != nil {
		return "", 0, err
	}

	// 请求头
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", 0, err
	}
	if req[0] != socksVersion {
		return "", 0, fmt.Errorf("请求版本非法 %d", req[0])
	}
	if req[1] != cmdConnect {
		_ = writeReply(c, repCmdNotSupport)
		return "", 0, fmt.Errorf("只支持 CONNECT，收到命令 %d", req[1])
	}

	var host string
	switch req[3] {
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return "", 0, err
		}
		if l[0] == 0 {
			return "", 0, errors.New("域名为空")
		}
		buf := make([]byte, l[0])
		if _, err := io.ReadFull(c, buf); err != nil {
			return "", 0, err
		}
		host = strings.ToLower(string(buf))
	case atypIPv4, atypIPv6:
		if !s.allowIP {
			// 默认拒绝 IP 直连：没有域名就没有策略，等于绕过白名单。
			_ = writeReply(c, repAddrNotSupport)
			return "", 0, ErrBlocked
		}
		size := net.IPv4len
		if req[3] == atypIPv6 {
			size = net.IPv6len
		}
		ip := make([]byte, size)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", 0, err
		}
		host = net.IP(ip).String()
	default:
		_ = writeReply(c, repAddrNotSupport)
		return "", 0, fmt.Errorf("不支持的地址类型 %d", req[3])
	}

	var p [2]byte
	if _, err := io.ReadFull(c, p[:]); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(p[:])), nil
}

// dial 解析域名、校验全部 IP、直连校验过的地址。
//
// 关键点：解析结果只在这一刻可信，所以连的必须是这个 IP，
// 不能让底层再解析一次（否则中间的时间差就是 rebinding 的窗口）。
func (s *Server) dial(ctx context.Context, host string, port int) (net.Conn, string, error) {
	if port <= 0 || port > 65535 {
		return nil, "", fmt.Errorf("端口非法 %d", port)
	}
	dctx, cancel := context.WithTimeout(ctx, DefaultDialTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(dctx, host)
	if err != nil {
		return nil, "", fmt.Errorf("解析 %s 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, "", fmt.Errorf("解析 %s 无结果", host)
	}
	// 任何一条记录落在私网就整体拒绝：多 A 记录轮询是 rebinding 的常见变体。
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return nil, "", fmt.Errorf("目标 %s 解析到受限地址 %s", host, ip.IP)
		}
	}

	var lastErr error
	for _, ip := range ips {
		conn, err := (&net.Dialer{Timeout: DefaultDialTimeout}).DialContext(dctx, "tcp", net.JoinHostPort(ip.IP.String(), fmt.Sprintf("%d", port)))
		if err == nil {
			return conn, ip.IP.String(), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("无可用地址")
	}
	return nil, "", lastErr
}

// blockedIP 覆盖私网、回环、链路本地（含 169.254.169.254）、未指定地址。
// IPv4-mapped IPv6 也按 IPv4 判断，避免 ::ffff:127.0.0.1 绕过。
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}

func (s *Server) emit(ev Event) {
	if s.onEvent != nil {
		s.onEvent(ev)
	}
	if !ev.Allowed {
		s.log.Warn("出网被拒", "host", ev.Host, "port", ev.Port, "reason", ev.Reason, "err", ev.Err)
	}
}

// writeReply 写 SOCKS5 应答。
func writeReply(c net.Conn, rep byte) error {
	_, err := c.Write([]byte{socksVersion, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}

// pipe 双向拷贝并支持半关闭，返回双向字节数之和。
func pipe(a, b net.Conn) int64 {
	var wg sync.WaitGroup
	var n1, n2 int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		n1, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		n2, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
	return n1 + n2
}

// closeWrite 只关写方向，让对端读到 EOF 而不是被直接切断。
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
