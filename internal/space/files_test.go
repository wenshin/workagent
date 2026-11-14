package space

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// 同名上传不能被覆盖：已有材料比新上传的文件更贵，冲突时给新文件加后缀。
func TestUniqueFilePath(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	s := &Service{storage: fs, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	base := s.filesKey("sp1")

	for _, key := range []string{"docs/合同.md", "docs/合同-1.md"} {
		if _, err := fs.Put(ctx, path.Join(base, key), bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("预置文件失败: %v", err)
		}
	}

	got, err := s.uniqueFilePath(ctx, base, "docs", "合同.md")
	if err != nil {
		t.Fatalf("计算路径失败: %v", err)
	}
	if want := "docs/合同-2.md"; got != want {
		t.Fatalf("期望 %q，得到 %q", want, got)
	}

	// 空间根没有冲突时保持原名
	got, err = s.uniqueFilePath(ctx, base, "", "说明.md")
	if err != nil {
		t.Fatalf("计算路径失败: %v", err)
	}
	if got != "说明.md" {
		t.Fatalf("期望原名，得到 %q", got)
	}
}
