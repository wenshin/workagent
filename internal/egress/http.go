package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// HTTPProxy 是跑在 unix socket 上的 HTTP 代理：插件发 absolute-form 请求，
// TLS 由宿主这侧完成，插件到代理这一段是本机 socket，不需要加密。
//
// 为什么不直接用 SOCKS5（同包的 Server）：Deno 在 unix socket 上做不了 TLS
// （Deno.startTls 只接受 TcpConn，传 UnixConn 直接 BadResource），也无法把
// 请求塞进自定义连接（createHttpClient 的 createConnection 已被移除）。
// 所以只能把 TLS 挪到宿主，代理侧用最朴素的 HTTP/1.1 语义。
//
// 代价是请求头与响应体在插件进程里是明文——它本来就是插件自己发的，
// 真正需要保密的是 secret:名称，那仍然由宿主在转发前替换。
type HTTPProxy struct {
	path    string
	ln      net.Listener
	srv     *http.Server
	policy  Policy
	connect *Allowlist
	// hostPorts 是平台自己注入的「精确 host:port」：宿主地址既不能写进清单，
	// 也不能按域名放行（127.0.0.1 会连带放开本机所有端口）。
	hostPorts *HostPorts
	dial      func(ctx context.Context, host string, port int) (net.Conn, error)
	verbs     map[string]bool
	log       *slog.Logger
	onEvent   func(Event)
	client    *http.Client
	// platformClient 是给精确白名单目标用的：平台地址本身就是内网 / 回环
	// （集群里它就是个 10.x 地址），走 dialChecked 会被私网拒绝把自己拒掉。
	// 它只在 hostPorts 命中时被选中，插件选不了目标。
	platformClient *http.Client
	closeOne       chan struct{}
}

// ListenHTTP 在 path 上起一个 HTTP 代理。
func ListenHTTP(p string, opt Options) (*HTTPProxy, error) {
	ln, err := listenUnix(p)
	if err != nil {
		return nil, err
	}
	return newHTTPProxy(ln, p, opt)
}

func newHTTPProxy(ln net.Listener, p string, opt Options) (*HTTPProxy, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	verbs := map[string]bool{}
	for _, v := range opt.Verbs {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			verbs[v] = true
		}
	}
	h := &HTTPProxy{
		path:      p,
		ln:        ln,
		policy:    opt.Policy,
		connect:   connectList(opt.ConnectHosts),
		hostPorts: NewHostPorts(opt.HostPorts),
		dial:      opt.Dial,
		verbs:     verbs,
		log:       opt.Logger,
		onEvent:   opt.OnEvent,
		closeOne:  make(chan struct{}),
	}
	// 两个客户端只有拨号策略不同：同一份白名单判定在 handle 里先做完，
	// 这里选哪条只是「允不允许目标是私网」，不改变放行集合。
	newClient := func(dial func(ctx context.Context, host string, port int) (net.Conn, error)) *http.Client {
		transport := &http.Transport{
			// 不走环境代理：这条链路本身就是代理。
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				p, err := parsePort(port)
				if err != nil {
					return nil, err
				}
				return dial(ctx, host, p)
			},
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:        8,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: DefaultDialTimeout,
		}
		return &http.Client{
			Transport: transport,
			// 不跟随跳转：域名白名单只对第一跳成立，跟随就等于让目标决定去哪。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       60 * time.Second,
		}
	}
	h.client = newClient(dialChecked)
	h.platformClient = newClient(dialResolved)
	h.srv = &http.Server{Handler: http.HandlerFunc(h.handle), ReadHeaderTimeout: 10 * time.Second}
	return h, nil
}

func (h *HTTPProxy) Path() string { return h.path }

// Serve 接受连接直到 ctx 结束。
func (h *HTTPProxy) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		h.Close()
	}()
	if err := h.srv.Serve(h.ln); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}

func (h *HTTPProxy) Close() error {
	select {
	case <-h.closeOne:
		return nil
	default:
		close(h.closeOne)
	}
	err := h.srv.Close()
	if h.path != "" {
		_ = os.Remove(h.path)
	}
	return err
}

// allows 是普通 HTTP 请求的放行判定：域名白名单之外，还认平台注入的
// 精确 host:port（宿主地址）。
//
// 两套口径必须分开：宿主地址可能是 127.0.0.1，按域名放行就等于放开本机
// 所有端口；而它又不能写进清单（清单 hosts 只收域名）。所以平台把这一个
// 端口单独注进来，插件既不需要声明，也无从伪造。
func (h *HTTPProxy) allows(host string, port int) bool {
	if h.policy != nil && h.policy.Allow(host) {
		return true
	}
	return h.hostPorts.Allow(host, port)
}

// clientFor 按目标选拨号策略：只有精确白名单里的目标（平台地址）可以落在
// 内网 / 回环网段，其余目标仍由 dialChecked 拒绝私网。
func (h *HTTPProxy) clientFor(host string, port int) *http.Client {
	if h.platformClient != nil && h.hostPorts.Allow(host, port) {
		return h.platformClient
	}
	return h.client
}

func (h *HTTPProxy) handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r, start)
		return
	}
	u := r.URL
	if u == nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "请求行必须是 absolute-form，例如 POST https://api.example.com/x HTTP/1.1", http.StatusBadRequest)
		return
	}
	host := u.Hostname()
	port := 0
	if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	if p := u.Port(); p != "" {
		var err error
		if port, err = parsePort(p); err != nil {
			http.Error(w, "端口非法", http.StatusBadRequest)
			return
		}
	}

	// WebSocket 必须先分流：它的 Upgrade 头不能被当成逐跳头删掉，
	// 而且授权口径是 ws.connect，不是 http.request 的 verbs。
	if isWebSocketUpgrade(r) {
		h.handleUpgrade(w, r, u, host, port, start)
		return
	}

	ev := Event{Host: host, Port: port, Duration: 0}
	// 动词限制只能在代理这层做：清单里写了 verbs: ["GET"]，
	// 结果插件 POST 出去，等于声明白写。
	if len(h.verbs) > 0 && !h.verbs[strings.ToUpper(r.Method)] {
		ev.Reason = "method_not_allowed"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "方法不在清单 verbs 里："+r.Method, http.StatusForbidden)
		return
	}
	if !h.allows(host, port) {
		ev.Allowed = false
		ev.Reason = "not_allowlisted"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "目标不在白名单内："+host, http.StatusForbidden)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = u
	out.Host = u.Host
	// 逐跳头不能透传，否则会破坏这条连接本身。
	for _, k := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Upgrade"} {
		out.Header.Del(k)
	}

	resp, err := h.clientFor(host, port).Do(out)
	if err != nil {
		ev.Reason = "dial_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "出网失败："+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	ev.Allowed = true
	ev.Bytes = n
	ev.Duration = time.Since(start)
	ev.RemoteIP = ""
	h.emit(ev)
}

// handleConnect 处理 CONNECT，这是 wss:// 的唯一实现路径。
//
// 隧道里是插件与目标之间的 TLS：宿主只能看到域名与端口，
// 看不到路径、动词、内容，也做不了响应大小限制。这是支持 WebSocket
// 必须付的代价，所以只有清单里显式声明 ws.connect 的插件才有隧道白名单。
func (h *HTTPProxy) handleConnect(w http.ResponseWriter, r *http.Request, start time.Time) {
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" {
		http.Error(w, "CONNECT 目标必须是 host:port", http.StatusBadRequest)
		return
	}
	ev := Event{Host: host}
	// 审计里要能看见真实端口，别把被拒的 22 记成 0。
	if p, err := parsePort(portStr); err == nil {
		ev.Port = p
	}
	// 端口先定死 443：这不是通用 TCP 隧道，只给 wss 用。
	if portStr != "443" {
		ev.Reason = "port_not_allowed"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "隧道只允许 443（wss）", http.StatusForbidden)
		return
	}
	ev.Port = 443
	// 唯一来源是清单里显式声明的 ws.connect：隧道里看不见明文，
	// 不能拿主域名白名单放宽。
	if h.connect == nil || !h.connect.Allow(host) {
		ev.Reason = "not_allowlisted"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "隧道目标不在白名单内："+host, http.StatusForbidden)
		return
	}

	dial := h.dial
	if dial == nil {
		dial = dialChecked
	}
	up, err := dial(r.Context(), host, 443)
	if err != nil {
		ev.Reason = "dial_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "隧道建立失败："+err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "当前连接不支持隧道", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		h.log.Warn("接管连接失败", "err", err)
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = conn.Close()
		_ = up.Close()
		return
	}

	// 双向拷贝：任一端关闭就把另一侧也关掉，避免半开连接挂在代理里。
	var total int64
	done := make(chan struct{})
	go func() {
		n, _ := io.Copy(up, brw)
		atomic.AddInt64(&total, n)
		_ = up.Close()
		_ = conn.Close()
		close(done)
	}()
	n, _ := io.Copy(conn, up)
	atomic.AddInt64(&total, n)
	_ = conn.Close()
	_ = up.Close()
	<-done

	ev.Allowed = true
	ev.Bytes = atomic.LoadInt64(&total)
	ev.Duration = time.Since(start)
	h.emit(ev)
}

// connectList 把 ws.connect 的域名包成 Allowlist；空列表返回 nil 表示不放行任何隧道。
func connectList(hosts []string) *Allowlist {
	if len(hosts) == 0 {
		return nil
	}
	return NewAllowlist(hosts)
}

// isWebSocketUpgrade 判断这是一次 WS 握手。
//
// Deno 用 unix: 代理时不发 CONNECT，而是直接发 absolute-form 的
// GET https://... 加 Upgrade: websocket，让代理自己连上游做 TLS——
// 所以这条路径才是实际会走到的，CONNECT 只是兜底。
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// handleUpgrade 把 WS 握手原样转给上游，握手成功后就变成一条双向隧道。
//
// 授权口径是 ws.connect：宿主能看到域名与路径，但 101 之后的帧是加密的，
// 既管不了内容，也做不了响应大小限制。
func (h *HTTPProxy) handleUpgrade(w http.ResponseWriter, r *http.Request, u *url.URL, host string, port int, start time.Time) {
	ev := Event{Host: host, Port: port}
	if h.connect == nil || !h.connect.Allow(host) {
		ev.Reason = "ws_not_allowed"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "WebSocket 目标不在 ws.connect 白名单内："+host, http.StatusForbidden)
		return
	}

	dial := h.dial
	if dial == nil {
		dial = dialChecked
	}
	raw, err := dial(r.Context(), host, port)
	if err != nil {
		ev.Reason = "dial_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "连接上游失败："+err.Error(), http.StatusBadGateway)
		return
	}
	var up net.Conn = raw
	if u.Scheme == "https" {
		tc := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(r.Context()); err != nil {
			_ = raw.Close()
			ev.Reason = "tls_failed"
			ev.Err = err
			ev.Duration = time.Since(start)
			h.emit(ev)
			http.Error(w, "上游 TLS 握手失败："+err.Error(), http.StatusBadGateway)
			return
		}
		up = tc
	}
	defer func() { _ = up.Close() }()

	// 握手请求手动拼：Upgrade / Connection / Sec-WebSocket-* 必须原样保留，
	// 用 Request.Write 会被它自己改写 Connection 头。
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", r.Method, u.RequestURI())
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Proxy-Connection") || strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(up, b.String()); err != nil {
		ev.Reason = "write_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "转发握手失败", http.StatusBadGateway)
		return
	}

	br := bufio.NewReader(up)
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		ev.Reason = "read_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "上游握手响应非法", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// 握手被上游拒绝：把它的原话回给插件，别自己编一个状态码。
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		n, _ := io.Copy(w, resp.Body)
		ev.Reason = "upgrade_rejected"
		ev.Bytes = n
		ev.Duration = time.Since(start)
		h.emit(ev)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		ev.Reason = "hijack_unsupported"
		ev.Duration = time.Since(start)
		h.emit(ev)
		http.Error(w, "当前连接不支持隧道", http.StatusInternalServerError)
		return
	}
	conn, clientRW, err := hj.Hijack()
	if err != nil {
		ev.Reason = "hijack_failed"
		ev.Err = err
		ev.Duration = time.Since(start)
		h.emit(ev)
		h.log.Warn("接管连接失败", "err", err)
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := resp.Header.Write(conn); err != nil {
		_ = conn.Close()
		return
	}
	if _, err := io.WriteString(conn, "\r\n"); err != nil {
		_ = conn.Close()
		return
	}

	var total int64
	done := make(chan struct{})
	go func() {
		n, _ := io.Copy(up, clientRW)
		atomic.AddInt64(&total, n)
		_ = up.Close()
		_ = conn.Close()
		close(done)
	}()
	n, _ := io.Copy(conn, br)
	atomic.AddInt64(&total, n)
	_ = conn.Close()
	_ = up.Close()
	<-done

	ev.Allowed = true
	ev.Bytes = atomic.LoadInt64(&total)
	ev.Duration = time.Since(start)
	h.emit(ev)
}

func (h *HTTPProxy) emit(ev Event) {
	if h.onEvent != nil {
		h.onEvent(ev)
	}
	if !ev.Allowed {
		h.log.Warn("出网被拒", "host", ev.Host, "port", ev.Port, "reason", ev.Reason, "err", ev.Err)
	}
}

// listenUnix 是 SOCKS5 与 HTTP 代理共用的 socket 准备逻辑。
func listenUnix(p string) (net.Listener, error) {
	if p == "" {
		return nil, errors.New("egress: 未指定 socket 路径")
	}
	const maxUnixPathLen = 100
	if len(p) > maxUnixPathLen {
		return nil, fmt.Errorf("代理 socket 路径过长（%d 字节，上限 %d）：请改用更短的目录，例如 /tmp/wa-egress.sock", len(p), maxUnixPathLen)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("创建代理目录失败: %w", err)
	}
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
	return ln, nil
}

// dialChecked 与 SOCKS5 侧同一套判定：解析 → 全部 IP 校验 → 直连校验过的 IP。
func dialChecked(ctx context.Context, host string, port int) (net.Conn, error) {
	return dialHost(ctx, host, port, true)
}

// dialResolved 解析后直连，不拒绝私网。只有一种调用方：精确白名单里的
// 平台地址——它本身就在内网 / 回环上，按私网拒绝等于把平台自己拒掉。
// 目标由平台注入，插件只能拿去连那一个 host:port，不能借它扫内网。
func dialResolved(ctx context.Context, host string, port int) (net.Conn, error) {
	return dialHost(ctx, host, port, false)
}

func dialHost(ctx context.Context, host string, port int, checkPrivate bool) (net.Conn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("端口非法 %d", port)
	}
	dctx, cancel := context.WithTimeout(ctx, DefaultDialTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(dctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("解析 %s 无结果", host)
	}
	if checkPrivate {
		for _, ip := range ips {
			if blockedIP(ip.IP) {
				return nil, fmt.Errorf("目标 %s 解析到受限地址 %s", host, ip.IP)
			}
		}
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := (&net.Dialer{Timeout: DefaultDialTimeout}).DialContext(dctx, "tcp", net.JoinHostPort(ip.IP.String(), fmt.Sprint(port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("无可用地址")
	}
	return nil, lastErr
}

func parsePort(s string) (int, error) {
	var p int
	if _, err := fmt.Sscanf(s, "%d", &p); err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("端口非法 %q", s)
	}
	return p, nil
}
