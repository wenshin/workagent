package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/storage"
)

const (
	// DraftDir 是空间文件区下存放插件草稿的目录（files/plugins/）。
	DraftDir = "plugins"
	// publishedRoot 是发布产物在存储根下的目录（published/plugin/{id}/{version}/）。
	publishedRoot = "published/plugin"

	// 发布快照的体积上限，避免把空间里的超大目录复制进发布区。
	maxBundleFiles = 200
	maxBundleBytes = 5 << 20
)

var (
	// ErrBundleTooLarge 表示插件目录超过发布上限。
	ErrBundleTooLarge = errors.New("插件体积超过上限（200 个文件 / 5MB）")
	// ErrManifestMissing 表示目录里没有 plugin.json。
	ErrManifestMissing = errors.New("缺少 plugin.json（插件元数据只能在这里定义）")
)

// Bundle 是一个插件的目录内容快照。
type Bundle struct {
	// Dir 是目录在存储里的 key。
	Dir  string
	Raw  []byte
	Meta Manifest
	// Files 是相对目录的路径 → 内容，包含 plugin.json 本身。
	Files map[string][]byte
	Hash  string
}

// DraftKey 返回草稿目录 key。
func DraftKey(fs storage.Storage, spaceID, slug string) string {
	return path.Join(fs.SpaceKey(spaceID), "files", DraftDir, slug)
}

// PublishedKey 返回发布件目录 key。
func PublishedKey(pluginID, version string) string {
	return path.Join(publishedRoot, pluginID, version)
}

// LoadBundle 读取一个插件目录并计算内容指纹。
func LoadBundle(ctx context.Context, fs storage.Storage, dir string, publishing bool) (*Bundle, error) {
	f, err := fs.Get(ctx, path.Join(dir, ManifestFile))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrManifestMissing
		}
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleBytes))
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	meta, err := Parse(raw, publishing)
	if err != nil {
		return nil, err
	}

	files := map[string][]byte{ManifestFile: raw}
	var total int64
	err = walk(ctx, fs, dir, "", func(rel string, size int64, read func() ([]byte, error)) error {
		if len(files) >= maxBundleFiles {
			return ErrBundleTooLarge
		}
		total += size
		if total > maxBundleBytes {
			return ErrBundleTooLarge
		}
		body, readErr := read()
		if readErr != nil {
			return readErr
		}
		files[rel] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	// plugin.json 已在上面读过，walk 里跳过它避免重复计数
	delete(files, "") // 防御：walk 不会产出空路径
	return &Bundle{Dir: dir, Raw: raw, Meta: meta, Files: files, Hash: HashDir(files)}, nil
}

// ContentHash 只统计内容文件，不含 plugin.json。
//
// 派生副本会重写清单（清版本、写 derived_from），若把清单计入基线指纹，
// 副本一落地就会被判成「本地已改」。基线比对只看正文与脚本。
func (b *Bundle) ContentHash() string {
	files := make(map[string][]byte, len(b.Files))
	for name, body := range b.Files {
		if name == ManifestFile {
			continue
		}
		files[name] = body
	}
	return HashDir(files)
}

// walk 递归遍历目录，按相对路径回调。跳过隐藏目录（如 .history）。
func walk(ctx context.Context, fs storage.Storage, dir, rel string, fn func(rel string, size int64, read func() ([]byte, error)) error) error {
	key := dir
	if rel != "" {
		key = path.Join(dir, rel)
	}
	entries, err := fs.List(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := path.Base(e.Key)
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := name
		if rel != "" {
			child = rel + "/" + name
		}
		if e.IsDir {
			if err := walk(ctx, fs, dir, child, fn); err != nil {
				return err
			}
			continue
		}
		if child == ManifestFile {
			continue
		}
		key := e.Key
		read := func() ([]byte, error) {
			f, err := fs.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return io.ReadAll(io.LimitReader(f, maxBundleBytes))
		}
		if err := fn(child, e.Size, read); err != nil {
			return err
		}
	}
	return nil
}

// WriteTo 把快照写到目标目录。
func (b *Bundle) WriteTo(ctx context.Context, fs storage.Storage, target string) error {
	names := make([]string, 0, len(b.Files))
	for name := range b.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := fs.EnsureDir(ctx, target); err != nil {
		return err
	}
	for _, name := range names {
		body := b.Files[name]
		key := path.Join(target, name)
		if err := fs.EnsureDir(ctx, path.Dir(key)); err != nil {
			return err
		}
		if _, err := fs.Put(ctx, key, strings.NewReader(string(body)), int64(len(body))); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", name, err)
		}
	}
	return nil
}

// ReadFile 读取目录下的一个文件。
func ReadFile(ctx context.Context, fs storage.Storage, dir, rel string) (string, error) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." || strings.Contains(rel, "..") {
		return "", fmt.Errorf("非法路径")
	}
	f, err := fs.Get(ctx, path.Join(dir, rel))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleBytes))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// RemoveDir 删除整个插件目录（不存在视为成功）。
func RemoveDir(ctx context.Context, fs storage.Storage, dir string) error {
	return fs.Delete(ctx, dir)
}

// CopyDir 把目录复制一份（用于从基线恢复副本前的备份）。
func CopyDir(ctx context.Context, fs storage.Storage, from, to string) error {
	b, err := LoadBundleRaw(ctx, fs, from)
	if err != nil {
		return err
	}
	return b.WriteTo(ctx, fs, to)
}

// LoadBundleRaw 只做目录复制，不校验清单（备份 / 迁移用）。
func LoadBundleRaw(ctx context.Context, fs storage.Storage, dir string) (*Bundle, error) {
	files := map[string][]byte{}
	var total int64
	err := walk(ctx, fs, dir, "", func(rel string, size int64, read func() ([]byte, error)) error {
		if len(files) >= maxBundleFiles {
			return ErrBundleTooLarge
		}
		total += size
		if total > maxBundleBytes {
			return ErrBundleTooLarge
		}
		body, err := read()
		if err != nil {
			return err
		}
		files[rel] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	if raw, err := ReadFile(ctx, fs, dir, ManifestFile); err == nil {
		files[ManifestFile] = []byte(raw)
	}
	return &Bundle{Dir: dir, Files: files}, nil
}
