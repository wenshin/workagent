package session

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// 会话挂在 plugins/<slug> 下时，模型必须知道自己在哪个插件里，
// 否则会当成全局会话去聊别的。
func TestDirPluginContext(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	manifest := []byte(`{"protocol":"wa-plugin/1","kind":"tool","slug":"order_lookup","name":"订单查询","description":"按订单号查询状态与金额"}`)
	if _, err := fs.Put(ctx, "spaces/sp1/files/plugins/order_lookup/plugin.json", bytes.NewReader(manifest), int64(len(manifest))); err != nil {
		t.Fatalf("写入清单失败: %v", err)
	}

	s := &Service{storage: fs, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	cases := []struct {
		name     string
		dir      string
		wantNil  bool
		wantName string
		wantKind string
	}{
		{name: "插件目录", dir: "plugins/order_lookup", wantName: "订单查询", wantKind: "工具插件"},
		{name: "插件子目录", dir: "plugins/order_lookup/references", wantName: "订单查询", wantKind: "工具插件"},
		{name: "清单缺失仍识别为插件目录", dir: "plugins/not_created", wantName: ""},
		{name: "普通目录", dir: "docs/合同", wantNil: true},
		{name: "插件根目录本身", dir: "plugins", wantNil: true},
		{name: "空间根", dir: "", wantNil: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.dirPluginContext(ctx, "sp1", tc.dir)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("期望无插件上下文，得到 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("期望识别为插件目录，得到 nil")
			}
			if got.Name != tc.wantName {
				t.Fatalf("名称不符: 期望 %q 得到 %q", tc.wantName, got.Name)
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("类型不符: 期望 %q 得到 %q", tc.wantKind, got.Kind)
			}
			if got.Slug == "" {
				t.Fatal("slug 不应为空（清单读不到时用于兜底）")
			}
		})
	}
}
