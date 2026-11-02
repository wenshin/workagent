package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = errors.New("storage: not found")
	// ErrInvalidKey 表示 key 越界或格式非法。
	ErrInvalidKey = errors.New("storage: invalid key")
)

// Entry 是存储条目的元信息。
type Entry struct {
	Key     string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Storage 抽象文件存储。MVP 用本地 FS，部署期可换 HDFS / S3，业务代码不感知。
// key 一律使用正斜杠、相对路径，例如 spaces/{space_id}/files/a.txt。
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) (string, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Rename(ctx context.Context, from, to string) error
	Stat(ctx context.Context, key string) (Entry, error)
	List(ctx context.Context, prefix string) ([]Entry, error)
	EnsureDir(ctx context.Context, key string) error
	// SpaceKey 返回空间在存储中的根 key。
	SpaceKey(spaceID string) string
}

// TrashKey 生成回收站 key：trash/{unix_ts}/{space_id}。
func TrashKey(spaceID string) string {
	return fmt.Sprintf("trash/%d/%s", time.Now().Unix(), spaceID)
}
