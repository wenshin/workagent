package tool

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cacheGCInterval = 6 * time.Hour

var (
	cacheTTLMu sync.RWMutex
	cacheTTL   = 30 * 24 * time.Hour
)

// SetCacheTTL 设置缓存的保留时长，<=0 关闭回收。
//
// 默认 30 天：足够覆盖「一段时间没碰、后来又想起来用」的场景，
// 又不会让 /cache 变成只增不减的黑洞。
func SetCacheTTL(d time.Duration) {
	cacheTTLMu.Lock()
	cacheTTL = d
	cacheTTLMu.Unlock()
}

// StartCacheGC 启动后台清理。
//
// 只清「可以重新下载」的东西，而且按最后修改时间判断：
//   - deno/<slug>@<version>：整个版本目录
//   - apt/archives/*.deb：单个包文件
//
// 不清 npm/_cacache 与 pip：它们的目录结构由工具自己管理，
// 手删容易留下悬空的索引项，交给 `npm cache clean` 更安全。
//
// 代价是：一个 30 天没用过的插件，下次调用时可能因为
// `--cached-only` 找不到依赖而失败。这比重装一个节点要便宜，
// 而且安装一次就会重新预热。
var (
	cacheGCMu     sync.Mutex
	cacheGCCancel context.CancelFunc
)

// StartCacheGC 启动后台清理，重复调用无副作用。
func StartCacheGC(log *slog.Logger) {
	cacheGCMu.Lock()
	defer cacheGCMu.Unlock()
	if cacheGCCancel != nil || CacheTTL() <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cacheGCCancel = cancel
	go func() {
		runCacheGC(log)
		t := time.NewTicker(cacheGCInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runCacheGC(log)
			}
		}
	}()
}

// StopCacheGC 停止后台清理，进程退出时调用。
func StopCacheGC() {
	cacheGCMu.Lock()
	defer cacheGCMu.Unlock()
	if cacheGCCancel != nil {
		cacheGCCancel()
		cacheGCCancel = nil
	}
}

// CacheTTL 返回当前保留时长。
func CacheTTL() time.Duration {
	cacheTTLMu.RLock()
	defer cacheTTLMu.RUnlock()
	return cacheTTL
}

func runCacheGC(log *slog.Logger) {
	ttl := CacheTTL()
	if ttl <= 0 {
		return
	}
	root := CacheRoot()
	cutoff := time.Now().Add(-ttl)
	removed := 0

	// 插件版本目录：整个删。
	denoDir := filepath.Join(root, "deno")
	if entries, err := os.ReadDir(denoDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(denoDir, e.Name())); err == nil {
				removed++
			}
		}
	}

	// apt 的 deb：按文件删，保留目录本身。
	archives := filepath.Join(root, "apt", "archives")
	if entries, err := os.ReadDir(archives); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(archives, e.Name())); err == nil {
				removed++
			}
		}
	}

	if removed > 0 {
		log.Info("清理过期缓存", "entries", removed, "ttl", ttl.String(), "root", root)
	}
}
