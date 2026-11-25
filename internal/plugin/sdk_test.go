package plugin

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// bootstrap 不能等 stdin 读循环结束：ready 是 pump 的 promise，只有 stdin 关闭
// 才 resolve。一次性进程会关，常驻进程永远不会——等它就等于常驻插件永远不加载
// 入口，现场表现是「插件执行超时（15000ms）」而 stderr 一片空白。
func TestBootstrapDoesNotAwaitPump(t *testing.T) {
	if strings.Contains(bootstrapSDK, "\nawait ready;") {
		t.Fatal("bootstrap 不能 await ready：pump 只有 stdin 关闭才结束，常驻进程会永远卡在加载入口之前")
	}
	if !strings.Contains(bootstrapSDK, "await import(entry)") {
		t.Fatal("bootstrap 必须自己 import 入口：serve 之前入口没跑，谁都拿不到回帧")
	}
}

// 文档口径是 await wa.config()，SDK 只有 host.config() 时插件会抛 TypeError，
// 被自己的 try/catch 吞掉后表现为「配置为空、插件永远未就绪」。
func TestSDKExposesWaConfig(t *testing.T) {
	i := strings.Index(hostSDK, "export const wa = {")
	if i < 0 {
		t.Fatal("host_sdk 里找不到 wa：插件可用的运行时助手只剩文件路径与出网，没有配置")
	}
	if !strings.Contains(hostSDK[i:], "config(): Promise<Record<string, string>>") {
		t.Fatal("wa 必须暴露 config()：文档让插件写 wa.config()，缺了就只能读 host.config()")
	}
}

// 插件池必须建 HTTP 代理，不能建 SOCKS5：SDK 的 wa.fetch 发的是 absolute-form
// 明文请求，Deno 的 HTTPS_PROXY 也只认 HTTP 语义。接错协议时插件收到的是
// 「不支持的协议版本 71」（0x47 = 'G'，GET 的首字节），出网全废。
func TestPluginEgressSpeaksHTTP(t *testing.T) {
	pool := newEgressPool(slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError})))
	defer pool.reg.Close()

	path, err := pool.proxyFor("space-1", "demo", []string{"wa-egress-test.invalid"}, nil, nil, map[string]bool{"GET": true})
	if err != nil {
		t.Fatalf("建代理: %v", err)
	}
	if path == "" {
		t.Fatal("声明了 hosts 却没建代理")
	}

	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		t.Fatalf("连代理 socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// SDK 就是这么说话的：absolute-form + Host，TLS 交给代理。
	fmt.Fprintf(conn, "GET http://wa-egress-test.invalid/ HTTP/1.1\r\nHost: wa-egress-test.invalid\r\nConnection: close\r\n\r\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.HasPrefix(line, "HTTP/1.") {
		t.Fatalf("插件池回的必须是 HTTP 响应，收到 %q（说明建成了 SOCKS5 代理）", line)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
