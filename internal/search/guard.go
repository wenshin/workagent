package search

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrHostNotAllowed 目标域名不在出网白名单内
	ErrHostNotAllowed = errors.New("目标域名不在出网白名单内")
	// ErrPrivateAddr 目标解析到内网地址，已拒绝
	ErrPrivateAddr = errors.New("目标地址属于内网，已拒绝")
	// ErrRedirect 目标返回 30x，拒绝跟随
	ErrRedirect = errors.New("目标发生跳转，已拒绝跟随")
)

// Guard 是搜索出网的唯一通道（技术方案 §8 安全约束）。
//
// 四道约束缺一不可：
//   - 只允许 https，避免明文外发；
//   - 域名白名单按「域名边界」匹配，`example.com.evil.com` 不算命中；
//   - 在 dial 阶段校验解析后的 IP，拒绝回环/私网/链路本地，防 DNS rebinding 与内网探测；
//   - 不跟随 30x，防止被白名单外的域名接管后续请求。
type Guard struct {
	allow   []string
	client  *http.Client
	maxBody int64
}

// NewGuard 构造出网通道。allow 里只接受域名（IPv4/IPv6 字面量会被忽略并告警由调用方处理），
// 这样白名单文件无法变成「直接连内网 IP」的后门。
func NewGuard(allow []string) *Guard {
	g := &Guard{allow: normalizeHosts(allow), maxBody: 4 << 20}

	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		// Control 在真正建立连接前拿到最终地址：DNS 解析发生在这里之后，
		// 因此这里是唯一能拦住「白名单域名解析到 10.x」的位置。
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !isPublicIP(ip) {
				return fmt.Errorf("%w: %s", ErrPrivateAddr, host)
			}
			return nil
		},
	}

	g.client = &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConns:          16,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		// 不跟随跳转：30x 由 Do 转成显式错误，交给模型告知用户。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return g
}

// Allowed 判断域名是否命中白名单（域名边界匹配，不含子串匹配）。
func (g *Guard) Allowed(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return false
	}
	for _, rule := range g.allow {
		if h == rule || strings.HasSuffix(h, "."+rule) {
			return true
		}
	}
	return false
}

// Do 发送请求并做全部校验；返回的 Body 已按上限截断。
func (g *Guard) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("请求为空")
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("只允许 https 出网，收到 %q", req.URL.Scheme)
	}
	if !g.Allowed(req.URL.Hostname()) {
		return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, req.URL.Hostname())
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w（HTTP %d）", ErrRedirect, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	resp.Body = readCloser{Reader: io.LimitReader(resp.Body, g.maxBody), Closer: resp.Body}
	return resp, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// normalizeHosts 去空白、小写、丢掉协议前缀与端口，并忽略 IP 字面量。
func normalizeHosts(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, raw := range rules {
		v := strings.ToLower(strings.TrimSpace(raw))
		v = strings.TrimPrefix(v, "https://")
		v = strings.TrimPrefix(v, "http://")
		if i := strings.IndexAny(v, "/?#"); i >= 0 {
			v = v[:i]
		}
		if host, _, err := net.SplitHostPort(v); err == nil {
			v = host
		}
		v = strings.Trim(v, ".*")
		v = strings.TrimSuffix(v, ".")
		if v == "" || net.ParseIP(v) != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// isPublicIP 只放行全局单播地址：回环、私网、链路本地（含云元数据 169.254.169.254）
// 以及组播/未指定地址一律拒绝。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return ip.IsGlobalUnicast()
}
