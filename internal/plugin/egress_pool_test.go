package plugin

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 平台地址不进清单，由平台补成精确 host:port。这条路径必须真的通：
// 只有它能连上宿主，渠道插件（im.reply）才可能把消息投进 /inbox。
//
// 用回环上的 httptest 服务，正好覆盖「宿主地址落在私网上」这一档——
// 普通域名白名单的拨号会拒绝私网，平台地址这条不能拒。
func TestPluginEgressReachesPlatformHostPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbox" {
			t.Errorf("路径应原样透传，实际 %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务地址: %v", err)
	}

	pool := newEgressPool(slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError})))
	defer pool.reg.Close()

	// 只有宿主地址、没有任何清单域名：也必须有代理，否则插件完全没有出网。
	path, err := pool.proxyFor("space-1", "im-bot", nil, nil, []string{u.Host}, nil)
	if err != nil {
		t.Fatalf("建代理: %v", err)
	}
	if path == "" {
		t.Fatal("只有平台地址时也必须建代理")
	}

	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		t.Fatalf("连代理 socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST http://%s/inbox HTTP/1.1\r\nHost: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", u.Host, u.Host)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("平台地址应被放行并转发，实际: %s", line)
	}
}
