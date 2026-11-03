package space

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

var (
	ErrNameInvalid     = errors.New("名称需为 1-64 个字符，且不能包含 / 或 \\")
	ErrForbidden       = errors.New("没有该空间的权限")
	ErrReserved        = errors.New("系统预置空间不支持修改或删除")
	ErrConfirmMismatch = errors.New("输入的名称与当前对象不一致")
	ErrDepthExceeded   = errors.New("空间最多支持 3 层")
	ErrDirExists       = errors.New("同名目录已存在")
	ErrDirNotFound     = errors.New("目录不存在")
)

const (
	// maxDepth 与 MVP 方案 §4.1 一致：空间树最多 3 层
	maxDepth = 3
	// dirsRoot 是空间内用户文件区的目录名（§4.3 存储布局）
	dirsRoot = "files"
)

// Service 承载空间业务规则：命名、层级、权限、存储目录。
type Service struct {
	store   store.Store
	storage storage.Storage
	log     *slog.Logger
}

// NewService 构造空间服务。
func NewService(st store.Store, fs storage.Storage, log *slog.Logger) *Service {
	return &Service{store: st, storage: fs, log: log}
}

// CreateInput 是创建空间入参。
type CreateInput struct {
	Name     string
	ParentID string
}

// List 返回用户可见空间（扁平，调用方用 BuildTree 组装）。
func (s *Service) List(ctx context.Context, userID string) ([]store.Space, error) {
	return s.store.ListSpaces(ctx, userID)
}

// Create 创建空间：id 先分配，path 由父路径推导，随后初始化存储目录。
func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (store.Space, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return store.Space{}, err
	}

	id := platform.NewUUID()
	parentID := strings.TrimSpace(in.ParentID)
	spaceType := "shared"
	spacePath := "/shared/" + id
	depth := 0

	if parentID != "" {
		parent, err := s.store.GetSpace(ctx, parentID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Space{}, ErrForbidden
		}
		if err != nil {
			return store.Space{}, err
		}

		if parent.IsSystemReserved {
			return store.Space{}, ErrReserved
		}

		perm, err := s.store.SpacePerm(ctx, parentID, userID)
		if err != nil {
			return store.Space{}, err
		}
		if !canWrite(perm) {
			return store.Space{}, ErrForbidden
		}
		if parent.Depth >= maxDepth {
			return store.Space{}, ErrDepthExceeded
		}

		spaceType = parent.Type
		spacePath = parent.Path + "/" + id
		depth = parent.Depth + 1
	}

	sp, err := s.store.CreateSpace(ctx, store.SpaceInput{
		ID:       id,
		OwnerID:  userID,
		Name:     name,
		Type:     spaceType,
		ParentID: parentID,
		Path:     spacePath,
		Depth:    depth,
	})
	if err != nil {
		return store.Space{}, err
	}

	// 空间物理目录用 ID；用户目录建在 files/ 下（§4.3）
	if err := s.storage.EnsureDir(ctx, s.filesKey(sp.ID)); err != nil {
		// 目录可懒创建，不阻塞建空间
		s.log.Warn("初始化空间目录失败", "space_id", sp.ID, "err", err)
	}
	return sp, nil
}

// Get 查询单个空间并校验可见性。
func (s *Service) Get(ctx context.Context, userID, spaceID string) (store.Space, error) {
	sp, err := s.store.GetSpace(ctx, spaceID)
	if err != nil {
		return store.Space{}, err
	}
	perm, err := s.store.SpacePerm(ctx, spaceID, userID)
	if err != nil {
		return store.Space{}, err
	}
	if perm == "" {
		return store.Space{}, ErrForbidden
	}
	sp.Perm = perm
	return sp, nil
}

// Rename 重命名空间（仅 manage，系统预置除外）。
func (s *Service) Rename(ctx context.Context, userID, spaceID, rawName string) (store.Space, error) {
	name, err := normalizeName(rawName)
	if err != nil {
		return store.Space{}, err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Space{}, err
	}
	if isLocked(sp) {
		return store.Space{}, ErrReserved
	}
	if sp.Perm != "manage" {
		return store.Space{}, ErrForbidden
	}

	if err := s.store.RenameSpace(ctx, spaceID, name); err != nil {
		return store.Space{}, err
	}
	sp.Name = name
	return sp, nil
}

// Delete 软删空间及子树，并把存储目录移入回收站。
func (s *Service) Delete(ctx context.Context, userID, spaceID, confirmName string) error {
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return err
	}
	if isLocked(sp) {
		return ErrReserved
	}
	if sp.Perm != "manage" {
		return ErrForbidden
	}
	if strings.TrimSpace(confirmName) != sp.Name {
		return ErrConfirmMismatch
	}

	removed, err := s.store.DeleteSpaceTree(ctx, spaceID)
	if err != nil {
		return err
	}

	for _, r := range removed {
		if err := s.storage.Rename(ctx, s.storage.SpaceKey(r.ID), storage.TrashKey(r.ID)); err != nil &&
			!errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("空间目录移入回收站失败", "space_id", r.ID, "err", err)
		}
	}
	s.log.Info("空间已删除", "space_id", spaceID, "count", len(removed), "by", userID)
	return nil
}

// Dir 是空间内的一个真实文件目录（§4.2）：只存在于文件系统，不落 space 表，
// 没有默认会话、不能单独分享，权限完全沿用所属空间。
type Dir struct {
	SpaceID string
	Name    string
}

// filesKey 返回空间内用户文件区的 key，目录都建在它下面。
func (s *Service) filesKey(spaceID string) string {
	return path.Join(s.storage.SpaceKey(spaceID), dirsRoot)
}

// ListDirs 返回用户可见空间下的全部目录，一次取全供左侧树使用。
func (s *Service) ListDirs(ctx context.Context, userID string) ([]Dir, error) {
	spaces, err := s.store.ListSpaces(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]Dir, 0, 8)
	for _, sp := range spaces {
		entries, err := s.storage.List(ctx, s.filesKey(sp.ID))
		if err != nil {
			// 空间还没建过目录时 files/ 不存在，属于正常空态
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			s.log.Warn("读取空间目录失败", "space_id", sp.ID, "err", err)
			continue
		}

		dirs := make([]Dir, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir {
				continue
			}
			dirs = append(dirs, Dir{SpaceID: sp.ID, Name: path.Base(e.Key)})
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		out = append(out, dirs...)
	}
	return out, nil
}

// CreateDir 在空间的文件区建一个真实目录，名称即物理目录名（不是 hash）。
func (s *Service) CreateDir(ctx context.Context, userID, spaceID, rawName string) (Dir, error) {
	name, err := normalizeDirName(rawName)
	if err != nil {
		return Dir{}, err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}

	key := path.Join(s.filesKey(sp.ID), name)
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return Dir{}, ErrDirExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Dir{}, err
	}

	if err := s.storage.EnsureDir(ctx, key); err != nil {
		return Dir{}, err
	}
	return Dir{SpaceID: sp.ID, Name: name}, nil
}

// RenameDir 重命名目录：目录名就是真实路径，所以这里是一次 fs 的 rename。
func (s *Service) RenameDir(ctx context.Context, userID, spaceID, rawOld, rawNew string) (Dir, error) {
	oldName, err := normalizeDirName(rawOld)
	if err != nil {
		return Dir{}, err
	}
	newName, err := normalizeDirName(rawNew)
	if err != nil {
		return Dir{}, err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}
	if oldName == newName {
		return Dir{SpaceID: sp.ID, Name: newName}, nil
	}

	from := path.Join(s.filesKey(sp.ID), oldName)
	if _, err := s.storage.Stat(ctx, from); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Dir{}, ErrDirNotFound
		}
		return Dir{}, err
	}

	to := path.Join(s.filesKey(sp.ID), newName)
	if _, err := s.storage.Stat(ctx, to); err == nil {
		return Dir{}, ErrDirExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Dir{}, err
	}

	if err := s.storage.Rename(ctx, from, to); err != nil {
		return Dir{}, err
	}
	return Dir{SpaceID: sp.ID, Name: newName}, nil
}

// DeleteDir 删除目录及其内容；目录不进回收站（§4.2）。
func (s *Service) DeleteDir(ctx context.Context, userID, spaceID, rawName string) error {
	name, err := normalizeDirName(rawName)
	if err != nil {
		return err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}

	key := path.Join(s.filesKey(sp.ID), name)
	if _, err := s.storage.Stat(ctx, key); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrDirNotFound
		}
		return err
	}
	return s.storage.Delete(ctx, key)
}

func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrNameInvalid
	}
	if utf8.RuneCountInString(name) > 64 {
		return "", ErrNameInvalid
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsAny(name, "\r\n\t") {
		return "", ErrNameInvalid
	}
	return name, nil
}

// normalizeDirName 校验目录名：目录名即物理路径，额外禁止 . 与 ..。
func normalizeDirName(raw string) (string, error) {
	name, err := normalizeName(raw)
	if err != nil {
		return "", err
	}
	if name == "." || name == ".." {
		return "", ErrNameInvalid
	}
	return name, nil
}

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}

// isLocked 判断是否为系统预置空间（个人空间根）。
func isLocked(sp store.Space) bool {
	return sp.IsSystemReserved || (sp.Type == "personal" && sp.ParentID == "")
}
