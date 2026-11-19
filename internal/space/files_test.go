package space

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"testing"

	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/storage/localfs"
	"github.com/workagent/auto-agent/internal/store"
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

// 预览不了的文件仍要出现在树里（前端据此给下载入口），下载取原始字节且不截断；
// 正文读取那条路继续限定文本类型，模型读不到二进制。
func TestListAndDownloadNonPreviewableFile(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	svc := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Type: "shared"}, perm: "read"},
		storage: fs,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	base := svc.filesKey("sp1")

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if _, err := fs.Put(ctx, path.Join(base, "assets/logo.png"), bytes.NewReader(png), int64(len(png))); err != nil {
		t.Fatalf("预置图片失败: %v", err)
	}
	if _, err := fs.Put(ctx, path.Join(base, "notes/a.md"), bytes.NewReader([]byte("# 标题")), 8); err != nil {
		t.Fatalf("预置文档失败: %v", err)
	}

	listed := svc.walkFiles(ctx, "sp1", "", 1)
	previewableByPath := make(map[string]bool, len(listed))
	for _, f := range listed {
		previewableByPath[f.Path] = f.Previewable
	}
	if !previewableByPath["notes/a.md"] {
		t.Fatalf("md 应可预览: %+v", listed)
	}
	if ok, exists := previewableByPath["assets/logo.png"]; !exists || ok {
		t.Fatalf("png 应出现在清单里且不可预览: %+v", listed)
	}

	file, rc, err := svc.DownloadFile(ctx, "u1", "sp1", "assets/logo.png")
	if err != nil {
		t.Fatalf("下载不可预览文件应成功: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("读取下载内容失败: %v", err)
	}
	if !bytes.Equal(got, png) || file.Size != int64(len(png)) || file.Name != "logo.png" {
		t.Fatalf("下载内容不符: file=%+v got=%v", file, got)
	}

	// 路径越界与目录仍拒绝
	if _, _, err := svc.DownloadFile(ctx, "u1", "sp1", "../secret.png"); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("越界路径应返回 ErrNameInvalid，实际: %v", err)
	}
	if _, _, err := svc.DownloadFile(ctx, "u1", "sp1", "assets"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("目录不应按文件下载，实际: %v", err)
	}

	// 正文读取仍只认文本类型
	if _, err := svc.ReadFile(ctx, "u1", "sp1", "assets/logo.png"); !errors.Is(err, ErrFileUnsupported) {
		t.Fatalf("file_read 读二进制应返回 ErrFileUnsupported，实际: %v", err)
	}
}

// 隐藏文件本身可读：只有平台保留的隐藏项（.meta.json / .install.json / .history）
// 才对用户不可见、不可读。
func TestHiddenFilesVisibleExceptPlatformReserved(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	svc := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Type: "shared"}, perm: "write"},
		storage: fs,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	base := svc.filesKey("sp1")

	for rel, body := range map[string]string{
		".env":                       "TOKEN=x",
		".github/workflows/ci.yml":   "on: push",
		".meta.json":                 "{}",
		"skills/demo/.meta.json":     "{}",
		"plugins/.history/old.md":    "# old",
		"plugins/demo/.install.json": "{}",
	} {
		if _, err := fs.Put(ctx, path.Join(base, rel), bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
			t.Fatalf("预置 %s 失败: %v", rel, err)
		}
	}

	listed := svc.walkFiles(ctx, "sp1", "", 1)
	paths := make(map[string]bool, len(listed))
	for _, f := range listed {
		paths[f.Path] = true
	}
	for _, want := range []string{".env", ".github/workflows/ci.yml"} {
		if !paths[want] {
			t.Fatalf("用户隐藏文件应出现在树里: %s，实际 %+v", want, listed)
		}
	}
	for _, banned := range []string{".meta.json", "skills/demo/.meta.json", "plugins/.history/old.md", "plugins/demo/.install.json"} {
		if paths[banned] {
			t.Fatalf("平台保留项不应出现在树里: %s，实际 %+v", banned, listed)
		}
	}

	if _, err := svc.ReadFile(ctx, "u1", "sp1", ".env"); err != nil {
		t.Fatalf("用户隐藏文件应可读: %v", err)
	}
	if _, err := svc.ReadFile(ctx, "u1", "sp1", ".meta.json"); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("平台保留项应拒绝读取，实际: %v", err)
	}
}

// fakeSpaceStore 只实现删除文件会用到的三个方法；其余走嵌入接口，测试里不会调到。
type fakeSpaceStore struct {
	store.Store
	space    store.Space
	perm     string
	installs []store.PluginInstall
}

func (f *fakeSpaceStore) GetSpace(context.Context, string) (store.Space, error) {
	return f.space, nil
}

func (f *fakeSpaceStore) SpacePerm(context.Context, string, string) (string, error) {
	return f.perm, nil
}

func (f *fakeSpaceStore) ListPluginInstalls(context.Context, string) ([]store.PluginInstall, error) {
	return f.installs, nil
}

// 删文件的边界：已安装插件与系统空间内置目录只读，其余文件允许删除。
func TestDeleteFileGuardsAndDeletes(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	newSvc := func(sp store.Space, installs []store.PluginInstall) *Service {
		return &Service{
			store:   &fakeSpaceStore{space: sp, perm: "write", installs: installs},
			storage: fs,
			log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	svc := newSvc(store.Space{ID: "sp1", Type: "shared"}, nil)

	base := svc.filesKey("sp1")
	put := func(rel string) {
		t.Helper()
		if _, err := fs.Put(ctx, path.Join(base, rel), bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("预置 %s 失败: %v", rel, err)
		}
	}

	put("notes/a.md")
	put("plugins/draft/plugin.json")
	put("plugins/installed/plugin.json")
	put("docs/readme.md")

	if err := svc.DeleteFile(ctx, "u1", "sp1", "notes/a.md"); err != nil {
		t.Fatalf("普通文件应可删除: %v", err)
	}
	if _, err := fs.Stat(ctx, path.Join(base, "notes/a.md")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("文件应已删除，实际 err=%v", err)
	}

	// 目录不能按文件删：否则会顺手递归掉整棵子树
	if err := svc.DeleteFile(ctx, "u1", "sp1", "docs"); err == nil {
		t.Fatal("目录不应按文件删除")
	}

	// 已安装插件的副本只读
	installed := newSvc(store.Space{ID: "sp1", Type: "shared"}, []store.PluginInstall{{Slug: "installed"}})
	if err := installed.DeleteFile(ctx, "u1", "sp1", "plugins/installed/plugin.json"); !errors.Is(err, ErrPluginReadOnly) {
		t.Fatalf("已安装插件应返回 ErrPluginReadOnly，实际: %v", err)
	}

	// 系统空间的内置目录整棵只读
	system := newSvc(store.Space{ID: "sys", Type: SpaceTypeSystem}, nil)
	if err := system.DeleteFile(ctx, "u1", "sys", "plugins/x.md"); !errors.Is(err, ErrReserved) {
		t.Fatalf("系统空间内置目录应返回 ErrReserved，实际: %v", err)
	}

	if err := svc.DeleteFile(ctx, "u1", "sp1", "notes/missing.md"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("不存在的文件应返回 ErrFileNotFound，实际: %v", err)
	}
}
