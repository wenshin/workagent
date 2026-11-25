package space

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// treeRevTTL 是单个空间「树版本号」的进程内缓存时长。
//
// 版本号要递归读目录，而前端为了及时看到别人写出来的文件会周期性来问一次
// （其他会话里的 Agent、另一个标签页、shell 命令都会动盘）。按 TTL 复用一份结果，
// 同一空间在一个窗口内只走一次磁盘，前端轮询频率与磁盘读取次数解耦。
// 代价是最多滞后一个 TTL 才发现变化，所以 TTL 要比前端间隔小一个量级。
const treeRevTTL = time.Second

// treeRevEntry 是缓存里的一条：版本号与计算时间。
type treeRevEntry struct {
	rev string
	at  time.Time
}

// treeRevCache 是「空间 → 树版本号」的进程内缓存，零值可用。
type treeRevCache struct {
	mu sync.Mutex
	m  map[string]treeRevEntry
}

func (c *treeRevCache) get(spaceID string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[spaceID]
	if !ok || now.Sub(e.at) >= treeRevTTL {
		return "", false
	}
	return e.rev, true
}

func (c *treeRevCache) put(spaceID, rev string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]treeRevEntry)
	}
	// 空间被删掉时没有主动清理的时机，顺手摘掉过期的，
	// 否则长期运行的进程里这张表只会往上涨。
	if len(c.m) > 64 {
		for id, e := range c.m {
			if now.Sub(e.at) >= treeRevTTL {
				delete(c.m, id)
			}
		}
	}
	c.m[spaceID] = treeRevEntry{rev: rev, at: now}
}

// TreeRev 返回「该用户可见的整棵文件树」的版本号。
//
// 左树的内容是磁盘现状，没有写入事件可订阅：文件可能被其他会话里的 Agent、
// 另一个标签页、沙箱里的 shell 命令改出来。前端因此周期性拉一次这个端点，
// 只有版本号变了才重新取 /spaces/dirs 与 /spaces/files——全量清单一次可能有
// 几百条，没变化时不该跟着版本号一起传。
//
// 版本号覆盖树上会变的东西：空间的 id 与名称、每个空间 files/ 下的相对路径
// 集合与各自修改时间（改写 plugin.json / SKILL.md 会改展示名与描述，路径集合
// 发现不了）。插件的启停状态不进来——那只改数据库不落盘，由各自的写操作负责
// 让前端失效，多端同步这点滞后不影响「文件树及时更新」。
func (s *Service) TreeRev(ctx context.Context, userID string) (string, error) {
	spaces, err := s.store.ListSpaces(ctx, userID)
	if err != nil {
		return "", err
	}

	// 顺序必须稳定：同一个现状要算出同一个版本号，否则每轮都会误判成「变了」
	list := make([]store.Space, len(spaces))
	copy(list, spaces)
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	h := sha1.New()
	for _, sp := range list {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", sp.ID, sp.Name, s.spaceTreeRev(ctx, sp.ID))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// spaceTreeRev 返回单个空间文件树的版本号，带 TTL 缓存。
func (s *Service) spaceTreeRev(ctx context.Context, spaceID string) string {
	now := time.Now()
	if rev, ok := s.revCache.get(spaceID, now); ok {
		return rev
	}

	// 不加锁：读盘可能慢，持锁会把同一空间的其他请求全排在这一步上。
	// 并发的重复计算只是多读一次目录，不会算错。
	paths := make([]string, 0, 64)
	paths = s.walkTreeRev(ctx, spaceID, "", 1, paths)
	sort.Strings(paths)

	h := sha1.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	rev := hex.EncodeToString(h.Sum(nil))
	s.revCache.put(spaceID, rev, now)
	return rev
}

// walkTreeRev 按树的口径收集条目指纹：跳过平台保留项与超深目录，
// 与 walkDirs / walkFiles 的可见范围保持一致，否则「树上看不到的变化」也会触发重取。
func (s *Service) walkTreeRev(ctx context.Context, spaceID, parent string, depth int, out []string) []string {
	if depth > maxDirDepth {
		return out
	}

	key := s.filesKey(spaceID)
	if parent != "" {
		key = path.Join(key, parent)
	}
	entries, err := s.storage.List(ctx, key)
	if err != nil {
		// files/ 还没建过属于正常空态，与 ListFiles 同口径
		if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("读取空间文件失败", "space_id", spaceID, "dir", parent, "err", err)
		}
		return out
	}

	for _, e := range entries {
		name := path.Base(e.Key)
		// 平台保留的隐藏项（.meta.json / .install.json / .history）不在树上，
		// 它们的增删不该让前端重取清单
		if spacefs.Reserved(name) {
			continue
		}
		rel := name
		if parent != "" {
			rel = parent + "/" + name
		}
		// 修改时间要一起进指纹：改写文件内容不改变路径，但插件展示名、描述、
		// 文件大小都可能在变，只看路径集合发现不了
		out = append(out, fmt.Sprintf("%s|%t|%d", rel, e.IsDir, e.ModTime.UnixNano()))
		if e.IsDir {
			out = s.walkTreeRev(ctx, spaceID, rel, depth+1, out)
		}
	}
	return out
}
