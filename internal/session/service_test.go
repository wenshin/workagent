package session

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// 引用文件清单必须带稳定标记：前端靠它切分正文与文件标签，
// 模型靠它识别「用户要求参考这些文件」——文本要求先读，非文本只作附件。
func TestComposeContentWithReferences(t *testing.T) {
	content := ComposeContent("帮我总结", []string{"docs/规范.md", "docs/模板.md"})

	if !strings.HasPrefix(content, "帮我总结") {
		t.Fatalf("正文应保持在前: %q", content)
	}
	if !strings.Contains(content, ReferenceMarker) {
		t.Fatalf("缺少引用标记: %q", content)
	}
	for _, want := range []string{"- docs/规范.md", "- docs/模板.md", "file_read"} {
		if !strings.Contains(content, want) {
			t.Fatalf("引用块缺少 %q", want)
		}
	}

	if got := ComposeContent("没有引用", nil); got != "没有引用" {
		t.Fatalf("没有引用时不应改变正文: %q", got)
	}
}

// 引用不限类型：非文本（图片等）作为附件写进清单，但不能要求模型 file_read——
// 二进制读不到正文，照着指令读只会换来一次失败的回执。
func TestComposeContentWithAttachmentReferences(t *testing.T) {
	content := ComposeContent("看看这张图", []string{"docs/规范.md", "shots/login.png"})

	// 清单行必须是纯路径：前端按 "- " 行切出标签，路径里夹说明会让标签与跳转都错
	for _, want := range []string{"\n- docs/规范.md\n", "\n- shots/login.png\n"} {
		if !strings.Contains(content, want) {
			t.Fatalf("引用块缺少纯路径行 %q: %q", want, content)
		}
	}
	if !strings.Contains(content, "非文本附件") || !strings.Contains(content, "不要对它们调用 file_read") {
		t.Fatalf("非文本引用应标注为附件且不可读: %q", content)
	}
	if !strings.Contains(content, "file_read 读取上面的文本文件") {
		t.Fatalf("文本引用仍应要求读取正文: %q", content)
	}

	// 全部都是附件时不能再发「逐个 file_read」的指令
	only := ComposeContent("看看这张图", []string{"shots/login.png"})
	if strings.Contains(only, "先逐个 file_read") {
		t.Fatalf("只有附件时不应要求 file_read: %q", only)
	}
	if !strings.Contains(only, "不要对它们调用 file_read") {
		t.Fatalf("只有附件时仍要说明不可读: %q", only)
	}
}

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
