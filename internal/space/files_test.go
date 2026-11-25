package space

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
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
	kindByPath := make(map[string]PreviewKind, len(listed))
	for _, f := range listed {
		previewableByPath[f.Path] = f.Previewable
		kindByPath[f.Path] = f.PreviewKind
	}
	if !previewableByPath["notes/a.md"] {
		t.Fatalf("md 应可预览: %+v", listed)
	}
	if kindByPath["notes/a.md"] != PreviewKindText {
		t.Fatalf("md 的渲染方式应为 text: %+v", listed)
	}
	if ok, exists := previewableByPath["assets/logo.png"]; !exists || ok {
		t.Fatalf("png 应出现在清单里且不可预览正文: %+v", listed)
	}
	if kindByPath["assets/logo.png"] != PreviewKindImage {
		t.Fatalf("png 的渲染方式应为 image: %+v", listed)
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

// 浏览器能原生渲染的类型走 raw 端点：白名单决定 MIME 与渲染方式，
// 文本与可执行容器（HTML / JS）一律拒绝。
func TestPreviewFileKinds(t *testing.T) {
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
	put := func(rel, body string) {
		t.Helper()
		if _, err := fs.Put(ctx, path.Join(base, rel), bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
			t.Fatalf("预置 %s 失败: %v", rel, err)
		}
	}
	put("assets/logo.png", "png")
	put("assets/clip.mp4", "mp4")
	put("docs/manual.pdf", "pdf")
	put("notes/a.md", "# 标题")
	put("notes/page.html", "<script>alert(1)</script>")

	cases := []struct {
		path string
		kind PreviewKind
		mime string
	}{
		{"assets/logo.png", PreviewKindImage, "image/png"},
		{"assets/clip.mp4", PreviewKindVideo, "video/mp4"},
		{"docs/manual.pdf", PreviewKindPDF, "application/pdf"},
	}
	for _, c := range cases {
		file, contentType, rc, err := svc.PreviewFile(ctx, "u1", "sp1", c.path)
		if err != nil {
			t.Fatalf("%s 应可预览，实际: %v", c.path, err)
		}
		if contentType != c.mime || file.PreviewKind != c.kind {
			t.Fatalf("%s 类型不符: mime=%q kind=%q", c.path, contentType, file.PreviewKind)
		}
		if file.Previewable {
			t.Fatalf("%s 不应被当成可读正文的文本类型", c.path)
		}
		// 本地存储要能 Seek，raw 端点才能交给 http.ServeContent 出 Range
		if _, ok := rc.(io.ReadSeeker); !ok {
			t.Fatalf("%s 的读取器应支持 Seek（视频拖动进度条依赖 Range）", c.path)
		}
		_ = rc.Close()
	}

	// 文本走正文端点，可执行容器不允许内联返回
	for _, banned := range []string{"notes/a.md", "notes/page.html", "assets"} {
		if _, _, _, err := svc.PreviewFile(ctx, "u1", "sp1", banned); !errors.Is(err, ErrFileUnsupported) {
			t.Fatalf("%s 不应允许 raw 预览，实际: %v", banned, err)
		}
	}
	if _, _, _, err := svc.PreviewFile(ctx, "u1", "sp1", "../secret.png"); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("越界路径应返回 ErrNameInvalid，实际: %v", err)
	}
}

// 扩展名判定大小写无关，且文本 / 媒体 / 二进制三档互斥。
// 文本走排除法：代码、配置、无扩展名的构建文件都不需要维护白名单。
func TestPreviewKind(t *testing.T) {
	cases := map[string]PreviewKind{
		"README.md":   PreviewKindText,
		".env":        PreviewKindText,
		"src/main.ts": PreviewKindText,
		"main.go":     PreviewKindText,
		"script.js":   PreviewKindText,
		"page.html":   PreviewKindText,
		"Dockerfile":  PreviewKindText,
		"Makefile":    PreviewKindText,
		"config.toml": PreviewKindText,
		"photo.PNG":   PreviewKindImage,
		"logo.svg":    PreviewKindImage,
		"clip.webm":   PreviewKindVideo,
		"voice.mp3":   PreviewKindAudio,
		"manual.pdf":  PreviewKindPDF,
		"archive.zip": PreviewKindNone,
		"tool.exe":    PreviewKindNone,
		"font.woff2":  PreviewKindNone,
	}
	for name, want := range cases {
		if got := previewKind(name); got != want {
			t.Fatalf("%s 期望 %q，得到 %q", name, want, got)
		}
	}
}

// 代码与无扩展名配置能读正文；扩展名不认识但内容是二进制的，
// 读正文时明确报 ErrFileNotText，面板据此退回下载而不是展示乱码。
func TestReadFileTextAndBinarySniff(t *testing.T) {
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
	put := func(rel, body string) {
		t.Helper()
		if _, err := fs.Put(ctx, path.Join(base, rel), bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
			t.Fatalf("预置 %s 失败: %v", rel, err)
		}
	}
	put("src/main.ts", "export const answer: number = 42;\n")
	put("Makefile", "all:\n\tgo build ./...\n")
	put("data/blob.dat", "abc\x00def")
	put("archive.zip", "PK\x03\x04")

	fc, err := svc.ReadFile(ctx, "u1", "sp1", "src/main.ts")
	if err != nil {
		t.Fatalf("代码文件应能读正文: %v", err)
	}
	if !strings.HasPrefix(fc.Content, "export const answer") || !fc.Previewable || fc.PreviewKind != PreviewKindText {
		t.Fatalf("代码文件元信息不符: %+v", fc.File)
	}
	if _, err := svc.ReadFile(ctx, "u1", "sp1", "Makefile"); err != nil {
		t.Fatalf("无扩展名的构建文件应能读正文: %v", err)
	}

	// 扩展名不认识 + 内容含 NUL：不是文本，退回下载
	if _, err := svc.ReadFile(ctx, "u1", "sp1", "data/blob.dat"); !errors.Is(err, ErrFileNotText) {
		t.Fatalf("二进制内容应返回 ErrFileNotText，实际: %v", err)
	}
	// 已知二进制扩展名在路径校验阶段就被拦住
	if _, err := svc.ReadFile(ctx, "u1", "sp1", "archive.zip"); !errors.Is(err, ErrFileUnsupported) {
		t.Fatalf("压缩包应返回 ErrFileUnsupported，实际: %v", err)
	}
}

// raw 端点：白名单 MIME + inline + Range；非媒体类型回 400，不能当文档内联返回。
func TestPreviewFileHandlerServesRange(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Type: "shared"}, perm: "read"},
		storage: fs,
		log:     logger,
	}
	body := []byte("0123456789abcdef")
	if _, err := fs.Put(ctx, path.Join(svc.filesKey("sp1"), "assets/logo.png"), bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("预置图片失败: %v", err)
	}
	if _, err := fs.Put(ctx, path.Join(svc.filesKey("sp1"), "notes/a.md"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("预置文档失败: %v", err)
	}

	mux := http.NewServeMux()
	NewHandler(svc, func(*http.Request) (store.User, error) {
		return store.User{ID: "u1"}, nil
	}, logger).Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/spaces/sp1/file/raw?path=assets/logo.png", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求应回 206，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type 应为白名单里的 image/png，得到 %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("内联预览应回 inline，得到 %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("应禁止类型嗅探，得到 %q", got)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Fatalf("Range 内容不符: %q", got)
	}

	// 文本走正文端点；raw 端点拒绝，避免把 HTML / JS 之类的东西当文档返回
	req = httptest.NewRequest(http.MethodGet, "/spaces/sp1/file/raw?path=notes/a.md", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_file") {
		t.Fatalf("文本类型请求 raw 应回 400 unsupported_file，得到 %d：%s", rec.Code, rec.Body.String())
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

// fakeSpaceStore 只实现文件相关用例会用到的方法；其余走嵌入接口，测试里不会调到。
type fakeSpaceStore struct {
	store.Store
	space    store.Space
	perm     string
	installs []store.PluginInstall
}

func (f *fakeSpaceStore) GetSpace(context.Context, string) (store.Space, error) {
	return f.space, nil
}

// ListSpaces 只返回这一个空间：TreeRev 的版本号按用户可见空间拼，用它可以验证结构指纹。
func (f *fakeSpaceStore) ListSpaces(context.Context, string) ([]store.Space, error) {
	return []store.Space{f.space}, nil
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

// 上传不限类型：图片 / 音视频 / 压缩包都能落盘并带上对应的预览类别；
// 平台保留名与只读权限仍在写入侧拦住。
func TestSaveFileAcceptsAnyType(t *testing.T) {
	ctx := context.Background()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Type: "shared"}, perm: "write"},
		storage: fs,
		log:     logger,
	}

	cases := []struct {
		name string
		body []byte
		kind PreviewKind
	}{
		{"logo.png", []byte{0x89, 'P', 'N', 'G'}, PreviewKindImage},
		{"演示.mp4", []byte("mp4"), PreviewKindVideo},
		{"打包.zip", []byte("PK\x03\x04"), PreviewKindNone},
		// 无扩展名的文件按文本候选处理（README / Makefile / Dockerfile 都是这一类）
		{"README", []byte("no ext"), PreviewKindText},
		{"说明.md", []byte("# 标题"), PreviewKindText},
	}
	for _, c := range cases {
		f, err := svc.SaveFile(ctx, "u1", "sp1", "uploads", c.name, bytes.NewReader(c.body), int64(len(c.body)))
		if err != nil {
			t.Fatalf("%s 应可上传，实际: %v", c.name, err)
		}
		if f.Name != c.name || f.Size != int64(len(c.body)) || f.PreviewKind != c.kind {
			t.Fatalf("%s 元信息不符: %+v", c.name, f)
		}
		// 只有文本能读正文：二进制即使能传、能预览，也不进引用 / file_read
		if c.kind != PreviewKindText && f.Previewable {
			t.Fatalf("%s 不应被当成可读文本", c.name)
		}
	}

	// 平台保留的隐藏元数据不能由用户上传覆盖
	if _, err := svc.SaveFile(ctx, "u1", "sp1", "", ".install.json", strings.NewReader("{}"), 2); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("保留名应返回 ErrNameInvalid，实际: %v", err)
	}

	// 只读权限仍不能上传
	readonly := &Service{
		store:   &fakeSpaceStore{space: store.Space{ID: "sp1", Type: "shared"}, perm: "read"},
		storage: fs,
		log:     logger,
	}
	if _, err := readonly.SaveFile(ctx, "u1", "sp1", "", "a.png", bytes.NewReader([]byte("x")), 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("只读空间上传应返回 ErrForbidden，实际: %v", err)
	}
}

// 默认上限是 100MB；超限两条路都要挡住，且不能在空间里留下半截文件。
// 测试把上限调到 1KB，避免真的写 100MB。
func TestSaveFileRejectsOversize(t *testing.T) {
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
	if DefaultUploadMaxBytes != 100<<20 {
		t.Fatalf("默认上限应为 100MB，实际 %d", DefaultUploadMaxBytes)
	}
	svc.SetUploadMaxBytes(1 << 10)

	// size 已知：解析阶段就能判断，不必写进存储
	big := bytes.Repeat([]byte("x"), 2<<10)
	if _, err := svc.SaveFile(ctx, "u1", "sp1", "", "known.bin", bytes.NewReader(big), int64(len(big))); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("size 已知时超限应返回 ErrFileTooLarge，实际: %v", err)
	}
	if _, err := fs.Stat(ctx, path.Join(svc.filesKey("sp1"), "known.bin")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("超限文件不应落盘，实际 err=%v", err)
	}

	// size 未知：流式写入后靠计数兜底，刚落的文件要删掉
	if _, err := svc.SaveFile(ctx, "u1", "sp1", "", "unknown.bin", bytes.NewReader(big), -1); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("size 未知时超限应返回 ErrFileTooLarge，实际: %v", err)
	}
	if _, err := fs.Stat(ctx, path.Join(svc.filesKey("sp1"), "unknown.bin")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("超限文件不应留在空间里，实际 err=%v", err)
	}

	// 刚好等于上限可以写入
	exact := bytes.Repeat([]byte("y"), 1<<10)
	f, err := svc.SaveFile(ctx, "u1", "sp1", "", "exact.bin", bytes.NewReader(exact), int64(len(exact)))
	if err != nil || f.Size != int64(len(exact)) {
		t.Fatalf("恰好等于上限应可写入: file=%+v err=%v", f, err)
	}
}
