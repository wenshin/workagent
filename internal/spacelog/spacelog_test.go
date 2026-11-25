package spacelog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/storage/localfs"
)

func newTestLogger(t *testing.T, opt Options) (*Logger, *localfs.FS) {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	return New(fs, opt), fs
}

func readKey(t *testing.T, st storage.Storage, key string) string {
	t.Helper()
	rc, err := st.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", key, err)
	}
	return string(b)
}

// 日志必须一行一条：事后是按行 parse 的，缩进或换行会把一条拆成两条。
func TestWriteAppendsOneLinePerEntry(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	l, fs := newTestLogger(t, Options{Now: func() time.Time { return now }})

	l.Write(context.Background(), Entry{
		SpaceID: "s1", Kind: "shell", Tool: "shell_exec", Status: "exit",
		Latency: 1200 * time.Millisecond, Command: "ls -la",
		ExitCode: 2, Output: "boom",
	})
	l.Write(context.Background(), Entry{
		SpaceID: "s1", Kind: "plugin", Tool: "dingtalk_bot", Status: "ok",
		Latency: 300 * time.Millisecond,
	})

	key := "spaces/s1/files/.logs/2026-11-24.log"
	lines := strings.Split(strings.TrimSpace(readKey(t, fs, key)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d: %q", len(lines), lines)
	}

	var first record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("第一行不是合法 JSON: %v", err)
	}
	if first.Kind != "shell" || first.Command != "ls -la" || first.Status != "exit" {
		t.Fatalf("shell 记录字段不对: %+v", first)
	}
	if first.ExitCode == nil || *first.ExitCode != 2 {
		t.Fatalf("退出码应落库为 2，实际 %v", first.ExitCode)
	}
	if first.LatencyMS != 1200 {
		t.Fatalf("耗时应为 1200ms，实际 %d", first.LatencyMS)
	}

	var second record
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("第二行不是合法 JSON: %v", err)
	}
	// 非 shell 不带退出码：指针为空才不会写成「退出码 0」误导排查。
	if second.ExitCode != nil {
		t.Fatalf("插件记录不该有 exit_code，实际 %d", *second.ExitCode)
	}
}

// 跨天后历史文件被压成 .log.gz，内容必须可完整还原。
func TestCompactArchivesPreviousDay(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.Local)
	l, fs := newTestLogger(t, Options{Now: func() time.Time { return now }})
	l.Write(context.Background(), Entry{SpaceID: "s1", Kind: "shell", Status: "ok", Command: "echo hi"})

	old := readKey(t, fs, "spaces/s1/files/.logs/2026-11-23.log")
	if !strings.Contains(old, "echo hi") {
		t.Fatalf("写入内容不对: %q", old)
	}

	// 第二天：同步压缩一次（Write 内部也会异步触发，两侧幂等）。
	if err := l.Compact(context.Background(), "s1", "2026-11-24"); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}

	if _, err := fs.Stat(context.Background(), "spaces/s1/files/.logs/2026-11-23.log"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("压缩后应删除明文文件，实际 err=%v", err)
	}
	gz := readKey(t, fs, "spaces/s1/files/.logs/2026-11-23.log.gz")
	zr, err := gzip.NewReader(strings.NewReader(gz))
	if err != nil {
		t.Fatalf("归档不是合法 gzip: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if string(raw) != old {
		t.Fatalf("归档内容与原文不一致:\n原文 %q\n归档 %q", old, raw)
	}
}

// 压缩后按保留期清理：完整保留 RetainDays 天，再往前一天才删。
func TestCompactDropsExpiredArchives(t *testing.T) {
	l, fs := newTestLogger(t, Options{Now: time.Now, RetainDays: 30})
	ctx := context.Background()

	for _, day := range []string{"2026-10-24", "2026-10-25", "2026-11-24"} {
		key := "spaces/s1/files/.logs/" + day + ".log.gz"
		if _, err := fs.Put(ctx, key, bytes.NewReader(nil), 0); err != nil {
			t.Fatalf("预置 %s 失败: %v", key, err)
		}
	}

	if err := l.Compact(ctx, "s1", "2026-11-24"); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}

	gone := "spaces/s1/files/.logs/2026-10-24.log.gz" // 30 天前：过期
	if _, err := fs.Stat(ctx, gone); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("过期归档应被删除，实际 err=%v", err)
	}
	keep := "spaces/s1/files/.logs/2026-10-25.log.gz" // 第 30 天：仍在保留期内
	if _, err := fs.Stat(ctx, keep); err != nil {
		t.Fatalf("保留期内的归档不该删: %v", err)
	}
}

// 单日文件有上限：到顶后不再追加，避免日志长成配额与备份的主要来源。
func TestMaxFileBytesStopsWriting(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	l, fs := newTestLogger(t, Options{
		Now: func() time.Time { return now },
		// 一条记录 100 字节上下，300 字节只能放两三条。
		MaxFileBytes: 300,
	})

	for i := 0; i < 20; i++ {
		l.Write(context.Background(), Entry{
			SpaceID: "s1", Kind: "shell", Status: "ok",
			Command: "echo 0123456789", Output: strings.Repeat("x", 40),
		})
	}

	e, err := fs.Stat(context.Background(), "spaces/s1/files/.logs/2026-11-24.log")
	if err != nil {
		t.Fatalf("读文件信息失败: %v", err)
	}
	if e.Size > 300 {
		t.Fatalf("文件不该超过上限，实际 %d 字节", e.Size)
	}
	if e.Size == 0 {
		t.Fatal("至少应写入第一条")
	}
}

// 空间之间必须隔离：filenames 相同，但落在各自的目录下。
func TestSpacesAreIsolated(t *testing.T) {
	l, fs := newTestLogger(t, Options{Now: time.Now})
	l.Write(context.Background(), Entry{SpaceID: "s1", Kind: "shell", Status: "ok", Command: "one"})
	l.Write(context.Background(), Entry{SpaceID: "s2", Kind: "shell", Status: "ok", Command: "two"})

	day := time.Now().Format(dateLayout)
	if s := readKey(t, fs, "spaces/s1/files/.logs/"+day+".log"); !strings.Contains(s, "one") || strings.Contains(s, "two") {
		t.Fatalf("s1 内容串了: %q", s)
	}
	if s := readKey(t, fs, "spaces/s2/files/.logs/"+day+".log"); !strings.Contains(s, "two") || strings.Contains(s, "one") {
		t.Fatalf("s2 内容串了: %q", s)
	}
}

// 日志目录必须被 spacefs 标为平台保留项，否则用户在树里能看到、也能改写。
func TestLogDirIsReserved(t *testing.T) {
	if !spacefs.Reserved(Dir) {
		t.Fatalf("%s 必须是平台保留项", Dir)
	}
	if !spacefs.ReservedPath("plugins/x/" + Dir + "/2026-11-24.log") {
		t.Fatal("保留标记要能拦住任意深度的路径")
	}
}
