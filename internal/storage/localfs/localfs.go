package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/storage"
)

// FS 是基于本地文件系统的 Storage 实现。
type FS struct {
	root string
}

// New 创建本地存储，根目录不存在时自动创建。
func New(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析存储根目录失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建存储根目录失败: %w", err)
	}
	return &FS{root: abs}, nil
}

// SpaceKey 返回空间根目录 key。
func (f *FS) SpaceKey(spaceID string) string { return "spaces/" + spaceID }

// LocalPath 把 key 解析成本机真实路径，供需要直接访问文件系统的调用方使用
// （命令执行、外部脚本适配器）。校验规则与其它方法一致，不会跳出根目录。
func (f *FS) LocalPath(key string) (string, error) { return f.resolve(key) }

// resolve 校验并解析 key，防止目录穿越。
func (f *FS) resolve(key string) (string, error) {
	k := strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
	if k == "" {
		return "", storage.ErrInvalidKey
	}
	if strings.HasPrefix(k, "/") || strings.Contains(k, "..") {
		return "", storage.ErrInvalidKey
	}
	full := filepath.Join(f.root, filepath.FromSlash(path.Clean(k)))
	rel, err := filepath.Rel(f.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", storage.ErrInvalidKey
	}
	return full, nil
}

// Put 先写临时文件再原子改名，避免读到半截内容。
func (f *FS) Put(ctx context.Context, key string, r io.Reader, size int64) (string, error) {
	full, err := f.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}

	tmp := full + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return key, nil
}

// Append 在文件末尾追加，文件不存在时创建；父目录自动建。
//
// 用 O_APPEND 而不是「读出来再 Put」：日志是多实例同时写的场景，
// 每次整份重写会把另一实例刚写的那几行覆盖掉。
func (f *FS) Append(ctx context.Context, key string, r io.Reader) (int64, error) {
	full, err := f.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, r)
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	return n, copyErr
}

// Get 打开文件，不存在返回 storage.ErrNotFound。
func (f *FS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := f.resolve(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, storage.ErrNotFound
	}
	return fh, err
}

// Delete 删除文件或目录，不存在视为成功。
func (f *FS) Delete(ctx context.Context, key string) error {
	full, err := f.resolve(key)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Rename 移动文件或目录，目标父目录自动创建。
func (f *FS) Rename(ctx context.Context, from, to string) error {
	src, err := f.resolve(from)
	if err != nil {
		return err
	}
	dst, err := f.resolve(to)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return storage.ErrNotFound
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// Stat 返回条目元信息。
func (f *FS) Stat(ctx context.Context, key string) (storage.Entry, error) {
	full, err := f.resolve(key)
	if err != nil {
		return storage.Entry{}, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return storage.Entry{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.Entry{}, err
	}
	return storage.Entry{
		Key:     key,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}, nil
}

// List 列出直接子项。
func (f *FS) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	full, err := f.resolve(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	out := make([]storage.Entry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, storage.Entry{
			Key:     path.Join(prefix, e.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			IsDir:   e.IsDir(),
		})
	}
	return out, nil
}

// EnsureDir 创建目录（幂等）。
func (f *FS) EnsureDir(ctx context.Context, key string) error {
	full, err := f.resolve(key)
	if err != nil {
		return err
	}
	return os.MkdirAll(full, 0o755)
}

// Root 返回存储根目录，便于运维排查。
func (f *FS) Root() string { return f.root }
