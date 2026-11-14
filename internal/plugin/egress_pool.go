package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/workagent/auto-agent/internal/egress"
)

// egressPool 给每个插件实例一条独立出网通道。
//
// 为什么不共享一个代理：白名单是按插件清单算的，而 SOCKS5 握手本身不带身份，
// 共享 socket 无法区分是哪个插件在连，只能把所有插件的域名并成一张大名单——
// 那样 A 插件就能连 B 插件声明的域名，白名单等于没有。
//
// socket 由宿主创建、以单文件 bind 的方式挂进沙箱（见 sandbox.Spec.Proxy），
// 沙箱仍然 --unshare-net，这是唯一的对外路径。
type egressPool struct {
	mu      sync.Mutex
	entries map[string]*egressEntry
	logger  *slog.Logger
	// onEvent 在连接结束时回调，调用方可以在那里落审计。
	onEvent func(spaceID, slug string, ev egress.Event)
}

type egressEntry struct {
	server *egress.HTTPProxy
	// digest 是当次白名单的指纹：清单改了要重建，而不是继续用旧名单。
	digest string
	cancel context.CancelFunc
}

func newEgressPool(logger *slog.Logger) *egressPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &egressPool{entries: map[string]*egressEntry{}, logger: logger}
}

// proxyFor 返回该插件可用的代理 socket 路径；两张名单都空时返回空串（不给代理）。
//
// hosts 是 absolute-form 的 HTTP 白名单，wsHosts 是 CONNECT 隧道的白名单。
// 两张表分开：能发 HTTP 请求不等于能开隧道，隧道白名单只来自 ws.connect。
func (p *egressPool) proxyFor(spaceID, slug string, hosts, wsHosts []string, verbs map[string]bool) (string, error) {
	hosts = cleanHosts(hosts)
	wsHosts = cleanHosts(wsHosts)
	if len(hosts) == 0 && len(wsHosts) == 0 {
		return "", nil
	}
	key := spaceID + "|" + slug
	digest := hostsDigest(hosts) + "|" + hostsDigest(wsHosts)

	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[key]; e != nil {
		if e.digest == digest {
			return e.server.Path(), nil
		}
		// 白名单变了：旧 server 还按旧名单放行，必须换掉。
		e.cancel()
		delete(p.entries, key)
	}

	sock, err := egressSocketPath(key)
	if err != nil {
		return "", err
	}
	srv, err := egress.ListenHTTP(sock, egress.Options{
		Policy:       egress.NewAllowlist(hosts),
		ConnectHosts: wsHosts,
		Verbs:        verbsList(verbs),
		Logger:       p.logger,
		OnEvent: func(ev egress.Event) {
			if p.onEvent != nil {
				p.onEvent(spaceID, slug, ev)
			}
			if ev.Allowed {
				p.logger.Debug("插件出网", "space", spaceID, "plugin", slug,
					"host", ev.Host, "port", ev.Port, "bytes", ev.Bytes, "ms", ev.Duration.Milliseconds())
			}
		},
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	p.entries[key] = &egressEntry{server: srv, digest: digest, cancel: cancel}
	p.logger.Info("插件出网通道已建立", "space", spaceID, "plugin", slug,
		"socket", srv.Path(), "hosts", strings.Join(hosts, ","), "tunnel", strings.Join(wsHosts, ","))
	return srv.Path(), nil
}

// Close 关闭所有代理并删除 socket。进程退出前调用，避免 /tmp 里堆残留。
func (p *egressPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, e := range p.entries {
		e.cancel()
		delete(p.entries, key)
	}
}

// verbsList 把 verbs 集合摊平成稳定顺序，重建判定才不会被 map 顺序影响。
func verbsList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, ok := range set {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func hostsDigest(hosts []string) string {
	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:8])
}

// cleanHosts 只留合法域名：带协议、端口、路径的一律丢掉。
// 校验在 Validate 已经做过，这里是防止运行时被内部构造的半成品名单带偏。
func cleanHosts(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.Trim(h, ".")
		if h == "" || strings.ContainsAny(h, "/: ") || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// egressSocketPath 给出一条足够短的 unix socket 路径。
//
// getaddrinfo 之外还有个更烦的硬限制：unix socket 路径上限 100 字节左右，
// 而 os.TempDir() 在 macOS 上通常是 /var/folders/xx/.../T（60+ 字节），
// 直接拼就会得到 bind: invalid argument。优先用 /tmp。
func egressSocketPath(key string) (string, error) {
	sum := sha256.Sum256([]byte(key))
	name := "wa-egress-" + hex.EncodeToString(sum[:6]) + ".sock"
	for _, base := range []string{"/tmp", os.TempDir()} {
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		p := filepath.Join(base, name)
		if len(p) <= 100 {
			return p, nil
		}
	}
	return "", errors.New("找不到足够短的临时目录来放代理 socket")
}
