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
	// allowHTTP 放开明文：只给显式声明 allow_private 的内网源用
	allowHTTP bool
	// allowPrivate 放开私网/回环地址：内网搜索源必须用它，否则永远连不上
	allowPrivate bool
}

// NewGuard 构造出网通道。allow 里只接受域名（IPv4/IPv6 字面量会被忽略并告警由调用方处理），
// 这样白名单文件无法变成「直接连内网 IP」的后门。
func NewGuard(allow []string) *Guard {
	return newGuard(allow, false)
}

// NewSourceGuard 构造「搜索源专用」出网通道。
//
// allowPrivate 为 true 时放开 http 与私网地址（内网 ES / 内部检索网关），
// 但仍然拒绝链路本地与未指定地址——否则它就成了读云元数据 169.254.169.254 的后门。
func NewSourceGuard(allow []string, allowPrivate bool) *Guard {
	return newGuard(allow, allowPrivate)
}

func newGuard(allow []string, allowPrivate bool) *Guard {
	g := &Guard{
		allow:        normalizeHosts(allow),
		maxBody:      4 << 20,
		allowHTTP:    allowPrivate,
		allowPrivate: allowPrivate,
	}

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
			if !g.dialAllowed(ip) {
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
	resp, err := g.DoRaw(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// DoRaw 与 Do 共用同一套出网边界，但保留非 2xx 的响应体。
//
// 插件可能依赖 4xx 的业务错误体（例如 {"code":401,"msg":"token 过期"}），
// 把状态码和响应体一起藏起来，插件就无法区分「参数错」和「网络坏」。
func (g *Guard) DoRaw(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("请求为空")
	}
	if req.URL.Scheme != "https" && !(g.allowHTTP && req.URL.Scheme == "http") {
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
	resp.Body = readCloser{Reader: io.LimitReader(resp.Body, g.maxBody), Closer: resp.Body}
	return resp, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// normalizeHosts 去空白、小写、丢掉协议前缀与端口。
//
// IP 字面量要保留：内网搜索源常写成 https://10.1.2.3:9200，若在这里丢掉，
// 声明了 allow_private 也永远匹配不上。它不构成后门——真正拦私网的是
// 拨号期的 dialAllowed，白名单只是「允许打到哪」，不是「允许打私网」。
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
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// dialAllowed 是拨号期的地址校验。
//
// allowPrivate 只放开「私网 + 回环」，链路本地（含云元数据 169.254.169.254）、
// 未指定与组播仍然拒绝：内网源不需要读元数据服务，这条没有正当用途。
func (g *Guard) dialAllowed(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if g.allowPrivate {
		return true
	}
	return isPublicIP(ip)
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
