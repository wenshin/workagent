package space

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path"
	"testing"

	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/storage/localfs"
	"github.com/workagent/auto-agent/internal/store"
)

// 树版本号是「要不要重新取清单」的唯一判据，两个方向都要成立：
// 树上看得到的变化必须让它变，看不到的变化必须让它不变——否则前端要么不刷新，
// 要么每次轮询都白重取一遍几百条清单。
func TestTreeRevTracksVisibleChanges(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	svc := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Name: "共享", Type: "shared"}, perm: "write"},
		storage: fs,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// 版本号带 1s 缓存，测试里要按磁盘现状立即重算
	rev := func() string {
		t.Helper()
		svc.revCache = treeRevCache{}
		got, err := svc.TreeRev(ctx, "u1")
		if err != nil {
			t.Fatalf("取版本号失败: %v", err)
		}
		return got
	}
	put := func(rel string, body string) {
		t.Helper()
		key := path.Join(svc.filesKey("sp1"), rel)
		if _, err := fs.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
			t.Fatalf("写入 %s 失败: %v", rel, err)
		}
	}

	empty := rev()
	if again := rev(); again != empty {
		t.Fatalf("同一个现状算出两个版本号: %q / %q", empty, again)
	}

	// 新增可见文件：树变了
	put("docs/说明.md", "# 说明")
	added := rev()
	if added == empty {
		t.Fatal("新增文件后版本号没变，前端不会重新取清单")
	}

	// 改写已有文件：路径没变但内容变了（plugin.json 的展示名就是这样改），也算变化
	put("docs/说明.md", "# 说明 v2")
	if rev() == added {
		t.Fatal("改写文件后版本号没变，展示名/大小会一直是旧的")
	}

	// 平台保留项不进树，它的增删不该让前端重取
	current := rev()
	put(spacefs.MetaFile, "{}")
	if rev() != current {
		t.Fatal("保留项的增删不该触发文件树刷新")
	}
}
