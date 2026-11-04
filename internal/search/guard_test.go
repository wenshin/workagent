package search

import (
	"net"
	"net/http"
	"testing"
)

func TestGuardAllowedUsesDomainBoundary(t *testing.T) {
	g := NewGuard([]string{"bocha.com", "api.example.com"})

	cases := []struct {
		host string
		want bool
	}{
		{"bocha.com", true},
		{"api.bocha.com", true},
		{"API.BOCHA.COM", true},
		{"api.example.com", true},
		// 后缀伪装：不能因为包含 bocha.com 就放行
		{"bocha.com.evil.com", false},
		{"evilbocha.com", false},
		{"evil.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := g.Allowed(c.host); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestNewGuardNormalizesRules(t *testing.T) {
	g := NewGuard([]string{" https://API.Bocha.com/v1 ", "127.0.0.1", "http://example.com:8443/path"})

	if !g.Allowed("api.bocha.com") {
		t.Error("协议前缀与端口应被规整掉")
	}
	if g.Allowed("127.0.0.1") {
		t.Error("IP 字面量不应成为白名单规则")
	}
	if !g.Allowed("example.com") {
		t.Error("带端口的规则应规整为域名")
	}
}

func TestGuardRejectsNonHTTPS(t *testing.T) {
	g := NewGuard([]string{"bocha.com"})
	req, err := http.NewRequest(http.MethodGet, "http://api.bocha.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Do(req); err == nil {
		t.Fatal("明文 http 必须被拒绝")
	}
}

func TestGuardRejectsHostOutsideAllowlist(t *testing.T) {
	g := NewGuard([]string{"bocha.com"})
	req, err := http.NewRequest(http.MethodGet, "https://evil.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Do(req); err == nil {
		t.Fatal("白名单外的域名必须被拒绝")
	}
}

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // 回环
		"10.0.0.5",        // 私网
		"172.16.3.4",      // 私网
		"192.168.1.1",     // 私网
		"169.254.169.254", // 云元数据
		"0.0.0.0",         // 未指定
		"::1",             // IPv6 回环
		"fd00::1",         // IPv6 ULA
	}
	for _, s := range blocked {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s 应被判定为非公网", s)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 应被放行")
	}
}
