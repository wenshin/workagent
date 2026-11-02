package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/autohome/auto-agent/internal/space"
	"github.com/autohome/auto-agent/internal/store"
)

var (
	ErrNotSessionSpace = errors.New("只能在空间的「会话」子空间下创建会话")
	ErrForbidden       = errors.New("没有该空间的权限")
	ErrTitleInvalid    = errors.New("会话标题需为 1-64 个字符")
)

const defaultTitle = "新会话"

// Service 承载聊天会话的业务规则。
type Service struct {
	store  store.Store
	spaces *space.Service
	log    *slog.Logger
}

// NewService 构造会话服务。
func NewService(st store.Store, spaces *space.Service, log *slog.Logger) *Service {
	return &Service{store: st, spaces: spaces, log: log}
}

// List 返回空间下的会话；非「会话」子空间返回空列表。
func (s *Service) List(ctx context.Context, userID, spaceID string) ([]store.Session, error) {
	sp, err := s.spaces.Get(ctx, userID, spaceID)
	if err != nil {
		return nil, err
	}
	if !sp.IsSystemReserved {
		return []store.Session{}, nil
	}
	return s.store.ListSessions(ctx, spaceID)
}

// Create 在「会话」子空间下创建会话。
func (s *Service) Create(ctx context.Context, userID, spaceID, rawTitle string) (store.Session, error) {
	sp, err := s.spaces.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Session{}, err
	}
	if !sp.IsSystemReserved {
		return store.Session{}, ErrNotSessionSpace
	}
	if sp.Perm != "write" && sp.Perm != "manage" {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}

	sess, err := s.store.CreateSession(ctx, spaceID, userID, title)
	if err != nil {
		return store.Session{}, err
	}
	s.log.Info("会话已创建", "session_id", sess.ID, "space_id", spaceID, "by", userID)
	return sess, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return defaultTitle, nil
	}
	if utf8.RuneCountInString(title) > 64 {
		return "", ErrTitleInvalid
	}
	return title, nil
}
