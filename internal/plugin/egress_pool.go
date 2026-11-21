package plugin

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/egress"
)

// egressPool 给每个插件实例一条独立出网通道。
//
// 为什么不共享一个代理：白名单是按插件清单算的，而 SOCKS5 握手本身不带身份，
// 共享 socket 无法区分是哪个插件在连，只能把所有插件的域名并成一张大名单——
// 那样 A 插件就能连 B 插件声明的域名，白名单等于没有。
//
// socket 以单文件 bind 的方式挂进沙箱（见 sandbox.Spec.Proxy），沙箱仍然
// --unshare-net，这是唯一的对外路径。容器化部署下这个 socket 由执行体
// 自己创建，控制面只把白名单过线（sandbox.Spec.Egress）。
type egressPool struct {
	reg    *egress.Registry
	logger *slog.Logger
	// onEvent 在连接结束时回调，调用方可以在那里落审计。
	onEvent func(spaceID, slug string, ev egress.Event)
}

func newEgressPool(logger *slog.Logger) *egressPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &egressPool{reg: egress.NewRegistry(logger), logger: logger}
}

// proxyFor 返回该插件可用的代理 socket 路径；两张名单都空时返回空串（不给代理）。
//
// hosts 是 absolute-form 的 HTTP 白名单，wsHosts 是 CONNECT 隧道的白名单。
// 两张表分开：能发 HTTP 请求不等于能开隧道，隧道白名单只来自 ws.connect。
//
// 路径是确定性的（空间 + slug 哈希），执行体会在同一个字符串上建 socket：
// 这样控制面写进 Deno 权限参数与沙箱挂载表的路径，和执行体实际监听的位置
// 永远一致，不需要第二次往返确认。
func (p *egressPool) proxyFor(spaceID, slug string, hosts, wsHosts []string, verbs map[string]bool) (string, error) {
	hosts = cleanHosts(hosts)
	wsHosts = cleanHosts(wsHosts)
	if len(hosts) == 0 && len(wsHosts) == 0 {
		return "", nil
	}
	path, err := egress.SocketPath(spaceID + "|" + slug)
	if err != nil {
		return "", err
	}
	v := verbsList(verbs)
	digest := egress.Digest(hosts, wsHosts, v)
	return p.reg.Ensure(path, egress.Options{
		Policy:       egress.NewAllowlist(hosts),
		ConnectHosts: wsHosts,
		Verbs:        v,
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
	}, digest)
}

// Close 关闭所有代理并删除 socket。进程退出前调用，避免 /tmp 里堆残留。
func (p *egressPool) Close() {
	p.reg.Close()
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
