package egress

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
)

// Registry 按 socket 路径复用代理实例。
//
// 复用不是可选项：常驻插件进程持着一条长连接，每次调用重建代理会把它打断；
// 但白名单变了又必须换掉——旧 server 还在按旧名单放行，等于名单没改。
// digest 就是这两者的判据。
//
// 为什么放在这里而不是各调用方自己维护：控制面与执行体各有一份代理，
// 两份逻辑一旦漂移，就会出现「控制面以为放行了、执行体还在拒」这种
// 只在容器化部署里出现的故障。
type Registry struct {
	mu      sync.Mutex
	entries map[string]*registryEntry
	log     *slog.Logger
}

type registryEntry struct {
	server *Server
	digest string
	cancel context.CancelFunc
}

// NewRegistry 构造一个空的代理注册表。
func NewRegistry(log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{entries: map[string]*registryEntry{}, log: log}
}

// Ensure 在 path 上确保有一个按 opt 配置的代理，返回实际可用的 socket 路径。
//
// digest 由调用方用 Digest 算出：只有名单真的变了才重建，否则沿用旧实例，
// 长连接不会被打断。
func (r *Registry) Ensure(path string, opt Options, digest string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("egress: 未指定 socket 路径")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if e := r.entries[path]; e != nil {
		if e.digest == digest {
			return e.server.Path(), nil
		}
		// 名单变了：旧 server 还按旧名单放行，必须先关掉再重建。
		//
		// 必须同步 Close：Close 会删掉 socket 文件，如果交给 Serve 的
		// goroutine 异步做，它可能在下面 Listen 之后才执行，把新 socket
		// 删掉——表现是「代理建好了但连不上」，且只在白名单变更时出现。
		e.cancel()
		_ = e.server.Close()
		delete(r.entries, path)
	}

	srv, err := Listen(path, opt)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	r.entries[path] = &registryEntry{server: srv, digest: digest, cancel: cancel}
	logger := opt.Logger
	if logger == nil {
		logger = r.log
	}
	logger.Info("出网代理已建立", "socket", srv.Path())
	return srv.Path(), nil
}

// Close 关闭全部代理并删除 socket。
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for path, e := range r.entries {
		e.cancel()
		delete(r.entries, path)
	}
}

// Digest 是白名单指纹：排序后哈希，调用顺序不影响判定。
func Digest(hosts, wsHosts, verbs []string) string {
	h := sha256.New()
	for _, group := range [][]string{hosts, wsHosts, verbs} {
		list := append([]string{}, group...)
		sort.Strings(list)
		for _, s := range list {
			_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(s))))
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// SocketPath 给出一条足够短、且两端可复现的 unix socket 路径。
//
// 可复现是协议的一部分：容器化之后 socket 由执行体创建，但路径要写进
// 沙箱的挂载表与 Deno 的权限参数，那些都是控制面生成的。两边必须算出
// 同一个字符串，所以这里不能带随机数或进程相关信息。
//
// 长度上限是真的：unix socket 路径超过 100 字节左右会得到
// bind: invalid argument，而 os.TempDir() 在 macOS 上通常已经 60+ 字节。
func SocketPath(key string) (string, error) {
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

// IsSocketPath 判断路径是否落在 SocketPath 允许的位置上。
//
// 执行体会在控制面传来的路径上 bind 一个监听：不校验就等于允许对方
// 在任意目录里建文件。
func IsSocketPath(p string) bool {
	clean := filepath.Clean(p)
	if !strings.HasPrefix(filepath.Base(clean), "wa-egress-") || !strings.HasSuffix(clean, ".sock") {
		return false
	}
	dir := filepath.Dir(clean)
	for _, base := range []string{"/tmp", os.TempDir()} {
		if filepath.Clean(base) == dir {
			return true
		}
		if real, err := filepath.EvalSymlinks(base); err == nil && real == dir {
			return true
		}
	}
	return false
}
