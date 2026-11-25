package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// unix socket 路径上限 100 字节，t.TempDir() 在 macOS 上就已经超了。
func shortSockPath(t *testing.T) string {
	t.Helper()
	p := fmt.Sprintf("/tmp/wa-egress-test-%d.sock", time.Now().UnixNano()%1e6)
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// 代理只认 absolute-form：普通 origin-form 一律拒绝，
// 否则「宿主代理」就成了一个能连本机端口的转发器。
func TestHTTPProxyRejectsOriginForm(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{Policy: NewAllowlist([]string{"allowed.example"})})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	status := rawRequest(t, sock, "GET /path HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	if !strings.Contains(status, "400") {
		t.Fatalf("origin-form 应被拒绝，实际: %s", status)
	}
}

// 白名单之外的域名在建立连接之前就被拒，不会真的去连目标。
func TestHTTPProxyRejectsUnlistedHost(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{Policy: NewAllowlist([]string{"allowed.example"})})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	status := rawRequest(t, sock, "GET https://blocked.example/x HTTP/1.1\r\nHost: blocked.example\r\n\r\n")
	if !strings.Contains(status, "403") {
		t.Fatalf("白名单外应返回 403，实际: %s", status)
	}
}

// 平台地址由部署注入、精确到端口：同一个入口放行它，别的端口仍然拒绝。
// 它还落在回环 / 私网上，所以不能走「私网一律拒绝」那条拨号路径。
func TestHTTPProxyAllowsPlatformHostPort(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{HostPorts: []string{"127.0.0.1:1"}})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	// 放行的那一条不该是 403：端口 1 没人监听，得到的是转发失败。
	status := rawRequest(t, sock, "GET http://127.0.0.1:1/x HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")
	if strings.Contains(status, "403") {
		t.Fatalf("精确白名单里的 host:port 不应被拒，实际: %s", status)
	}
	// 同一主机别的端口没在名单里，必须在连目标之前就拒掉。
	status = rawRequest(t, sock, "GET http://127.0.0.1:2/x HTTP/1.1\r\nHost: 127.0.0.1:2\r\n\r\n")
	if !strings.Contains(status, "403") {
		t.Fatalf("同主机的其他端口应返回 403，实际: %s", status)
	}
}

func waitSock(t *testing.T, sock string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("代理 socket 没起来: %s", sock)
}

func rawRequest(t *testing.T, sock, req string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return line
}

// 清单里只声明了 GET，代理就不能放过 POST。
func TestHTTPProxyRejectsDisallowedMethod(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{
		Policy: NewAllowlist([]string{"allowed.example"}),
		Verbs:  []string{"GET"},
	})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	status := rawRequest(t, sock, "POST https://allowed.example/x HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	if !strings.Contains(status, "403") {
		t.Fatalf("未声明的方法应返回 403，实际: %s", status)
	}
}

// 没有 ws.connect 就没有隧道：CONNECT 一律 403。
func TestHTTPProxyRejectsConnectWithoutDecl(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{Policy: NewAllowlist([]string{"allowed.example"})})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	line := rawRequest(t, sock, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	if !strings.Contains(line, "403") {
		t.Fatalf("未声明 ws.connect 时 CONNECT 应 403，实际: %s", line)
	}
}

// 声明了 ws.connect 之后隧道要真的能双向跑：注入 Dial 连本地 echo，
// 模拟「代理把字节流原样桥接给上游」。
func TestHTTPProxyConnectTunnel(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起 echo 失败: %v", err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{
		ConnectHosts: []string{"wss.example"},
		Dial: func(context.Context, string, int) (net.Conn, error) {
			return net.Dial("tcp", echo.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("CONNECT wss.example:443 HTTP/1.1\r\nHost: wss.example:443\r\n\r\n")); err != nil {
		t.Fatalf("写 CONNECT 失败: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		t.Fatalf("隧道未建立: %q %v", line, err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("读空行失败: %v", err)
	}

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写隧道数据失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("隧道回显 = %q，期望 ping", buf)
	}
}

// 端口写死 443：否则这就是一条通用 TCP 隧道。
func TestHTTPProxyConnectRejectsOtherPort(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{ConnectHosts: []string{"wss.example"}})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	line := rawRequest(t, sock, "CONNECT wss.example:22 HTTP/1.1\r\nHost: wss.example:22\r\n\r\n")
	if !strings.Contains(line, "403") {
		t.Fatalf("非 443 隧道应 403，实际: %s", line)
	}
}

// ws:// 的升级透传：本地起一个只会「回 101 然后原样回显」的假 WS 服务，
// 验证代理把 Upgrade 头原样转给上游、把 101 回给客户端、之后双向通。
func TestHTTPProxyWebSocketUpgrade(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起上游失败: %v", err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
				_, _ = io.Copy(conn, br)
			}(c)
		}
	}()

	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{
		ConnectHosts: []string{"ws.example"},
		Dial: func(context.Context, string, int) (net.Conn, error) {
			return net.Dial("tcp", up.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET http://ws.example/echo HTTP/1.1\r\nHost: ws.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("写握手失败: %v", err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("握手未通过: %q %v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读响应头失败: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显 = %q", buf)
	}
}

// 没声明 ws.connect 的插件，upgrade 请求必须 403，而不是被当成普通 GET 转发出去。
func TestHTTPProxyWebSocketRequiresDecl(t *testing.T) {
	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{Policy: NewAllowlist([]string{"allowed.example"})})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	line := rawRequest(t, sock, "GET http://allowed.example/ws HTTP/1.1\r\nHost: allowed.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	if !strings.Contains(line, "403") {
		t.Fatalf("未声明 ws.connect 应 403，实际: %s", line)
	}
}

// 上游先说话：101 之后不等客户端写，上游立刻推帧，客户端必须收到。
//
// 现有用例只验「客户端写进去、回显取回来」，那是 client→upstream 方向。
// 钉钉长连接的 CONNECTED / REGISTERED / ping / CALLBACK 全是**服务端先发**，
// 这个方向断了用例照样绿，而现场表现是「连接建起来了，但一帧都收不到」——
// 最容易归因到「钉钉没推」的形态。
func TestHTTPProxyWebSocketForwardsUpstreamFrames(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起上游失败: %v", err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		// 101 之后立刻推两段，完全不等客户端说话
		_, _ = c.Write([]byte("FRAME-1"))
		time.Sleep(50 * time.Millisecond)
		_, _ = c.Write([]byte("FRAME-2"))
		time.Sleep(300 * time.Millisecond)
	}()

	sock := shortSockPath(t)
	srv, err := ListenHTTP(sock, Options{
		ConnectHosts: []string{"ws.example"},
		Dial: func(context.Context, string, int) (net.Conn, error) {
			return net.Dial("tcp", up.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("起代理失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	waitSock(t, sock)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("连接代理失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET http://ws.example/echo HTTP/1.1\r\nHost: ws.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("写握手失败: %v", err)
	}
	br := bufio.NewReader(c)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "101") {
		t.Fatalf("握手未通过: %q %v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读响应头失败: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	buf := make([]byte, 14)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("上游帧没转发到客户端: %v", err)
	}
	if string(buf) != "FRAME-1FRAME-2" {
		t.Fatalf("上游帧 = %q，期望 FRAME-1FRAME-2", buf)
	}
}
