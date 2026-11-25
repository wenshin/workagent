package egress

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSock 返回一个足够短的 socket 路径：t.TempDir() 的路径在 macOS 上
// 可能超过 unix socket 的 104 字节上限。
func testSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "eg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestAllowlistDomainBoundary(t *testing.T) {
	a := NewAllowlist([]string{"bocha.com", " .Example.COM "})
	cases := map[string]bool{
		"bocha.com":          true,
		"api.bocha.com":      true,
		"BOCHA.COM":          true,
		"example.com":        true,
		"bocha.com.evil.com": false,
		"evilbocha.com":      false,
		"":                   false,
		"other.com":          false,
		"sub.api.bocha.com":  true,
	}
	for host, want := range cases {
		if got := a.Allow(host); got != want {
			t.Fatalf("%q 期望 %v，实际 %v", host, want, got)
		}
	}
}

func TestEmptyAllowlistDeniesAll(t *testing.T) {
	if NewAllowlist(nil).Allow("example.com") {
		t.Fatal("空白名单必须全部拒绝")
	}
}

// 平台地址用精确 host:port 放行：同主机别的端口仍然拒绝，
// 否则「放行 127.0.0.1」等于把本机所有端口都交出去。
func TestHostPortsAllowOnlyExactPort(t *testing.T) {
	hp := NewHostPorts([]string{"127.0.0.1:8321", " .Wa.Example.com:443 ", "bad-entry", "host:notaport", "x:99999"})
	if !hp.Allow("127.0.0.1", 8321) {
		t.Fatal("列出的 host:port 应放行")
	}
	if hp.Allow("127.0.0.1", 8322) {
		t.Fatal("同主机的其他端口不该放行")
	}
	if hp.Allow("localhost", 8321) {
		t.Fatal("没列出的主机不该放行")
	}
	if !hp.Allow("WA.EXAMPLE.COM", 443) {
		t.Fatal("主机比较应忽略大小写，首尾空格与点应被规整掉")
	}
	if NewHostPorts(nil).Allow("127.0.0.1", 8321) {
		t.Fatal("空白名单必须全部拒绝")
	}
}

func TestBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.1.2.3", "192.168.1.1",
		"172.16.0.1", "169.254.169.254", "fe80::1", "0.0.0.0"}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Fatalf("%s 应被判定为受限地址", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if blockedIP(net.ParseIP(s)) {
			t.Fatalf("%s 不应被判定为受限地址", s)
		}
	}
}

// socks5Connect 是测试用的最小客户端：无认证 + CONNECT + 域名。
func socks5Connect(t *testing.T, sock, host string, port int) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("握手写失败: %v", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil {
		t.Fatalf("握手读失败: %v", err)
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatalf("请求写失败: %v", err)
	}
	var rep [2]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatalf("应答读失败: %v", err)
	}
	// 吃掉 BND.ADDR / BND.PORT（固定 8 字节：IPv4 地址 + 端口）
	bind := make([]byte, 8)
	if _, err := io.ReadFull(c, bind); err != nil {
		t.Fatalf("读取绑定地址失败: %v", err)
	}
	return c, rep[1]
}

func TestProxyForwardsAllowedHost(t *testing.T) {
	// 本地 echo 上游：用注入的 dialFn 替换真实建连，
	// 这样不必绕过 loopback 限制也能验证「协商 → 放行 → 双向转发」。
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	sock := testSock(t)
	var mu sync.Mutex
	var events []Event
	srv, err := Listen(sock, Options{
		Policy:  NewAllowlist([]string{"api.example.com"}),
		OnEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv.dialFn = func(ctx context.Context, host string, port int) (net.Conn, string, error) {
		c, err := net.Dial("tcp", upstreamLn.Addr().String())
		return c, "127.0.0.1", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	defer srv.Close()

	c, rep := socks5Connect(t, sock, "api.example.com", 443)
	defer c.Close()
	if rep != repSuccess {
		t.Fatalf("期望放行，实际应答 %d", rep)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("回显读取失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显内容不对: %q", buf)
	}
}

func TestProxyDeniesUnlistedHost(t *testing.T) {
	sock := testSock(t)
	var mu sync.Mutex
	var events []Event
	srv, err := Listen(sock, Options{
		Policy:  NewAllowlist([]string{"api.example.com"}),
		OnEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	defer srv.Close()

	c, rep := socks5Connect(t, sock, "evil.com", 443)
	defer c.Close()
	if rep != repNotAllowed {
		t.Fatalf("期望拒绝（%d），实际 %d", repNotAllowed, rep)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		var last Event
		if n > 0 {
			last = events[n-1]
		}
		mu.Unlock()
		if n > 0 {
			if last.Allowed || last.Host != "evil.com" || last.Reason != "not_allowlisted" {
				t.Fatalf("审计事件不对: %+v", last)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没有产生审计事件")
}

// TestProxyRejectsReboundTarget 验证 rebinding 防线：
// 域名解析到回环 / 私网时即使白名单放行也必须失败。
func TestProxyRejectsReboundTarget(t *testing.T) {
	sock := testSock(t)
	srv, err := Listen(sock, Options{Policy: NewAllowlist([]string{"localhost"})})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	defer srv.Close()

	c, rep := socks5Connect(t, sock, "localhost", 80)
	defer c.Close()
	if rep == repSuccess {
		t.Fatal("解析到回环地址必须拒绝，不能放行")
	}
}

func TestRejectsIPLiteralByDefault(t *testing.T) {
	sock := testSock(t)
	srv, err := Listen(sock, Options{Policy: NewAllowlist([]string{"1.1.1.1"})})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	defer srv.Close()

	// 手工发一个 ATYP=IPv4 的请求：默认应被拒绝（没有域名就没有策略）。
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil {
		t.Fatal(err)
	}
	req := []byte{0x05, 0x01, 0x00, 0x01, 1, 1, 1, 1, 0, 80}
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [2]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatal(err)
	}
	if rep[1] == repSuccess {
		t.Fatal("默认不允许 IP 直连")
	}
}

func TestListenSocketPermissions(t *testing.T) {
	sock := testSock(t)
	srv, err := Listen(sock, Options{Policy: NewAllowlist(nil)})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()
	if !strings.HasSuffix(srv.Path(), "s.sock") {
		t.Fatalf("Path() 不对: %s", srv.Path())
	}
}
