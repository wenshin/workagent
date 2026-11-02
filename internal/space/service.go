package space

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/autohome/auto-agent/internal/platform"
	"github.com/autohome/auto-agent/internal/storage"
	"github.com/autohome/auto-agent/internal/store"
)

var (
	ErrNameInvalid     = errors.New("空间名称需为 1-64 个字符，且不能包含 / 或 \\")
	ErrForbidden       = errors.New("没有该空间的权限")
	ErrReserved        = errors.New("系统预置空间不支持修改或删除")
	ErrConfirmMismatch = errors.New("输入的空间名与当前空间不一致")
	ErrDepthExceeded   = errors.New("空间最多支持 8 层")
)

const maxDepth = 8

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

	if err := s.storage.EnsureDir(ctx, s.storage.SpaceKey(sp.ID)); err != nil {
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

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}

// isLocked 判断是否为系统预置空间（个人空间根、「会话」子空间）。
func isLocked(sp store.Space) bool {
	return sp.IsSystemReserved || (sp.Type == "personal" && sp.ParentID == "")
}
