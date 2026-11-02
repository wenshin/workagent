package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/autohome/auto-agent/internal/space"
	"github.com/autohome/auto-agent/internal/store"
)

var (
	ErrForbidden      = errors.New("没有该空间的权限")
	ErrTitleInvalid   = errors.New("会话标题需为 1-64 个字符")
	ErrMessageEmpty   = errors.New("消息内容不能为空")
	ErrMessageTooLong = errors.New("单条消息不能超过 8000 字")
)

const (
	defaultTitle    = "新会话"
	maxTitleRunes   = 64
	maxMessageRunes = 8000
	// 首条消息自动生成的标题长度（超出部分省略）
	autoTitleRunes = 20
)

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

// List 返回空间下的会话。
func (s *Service) List(ctx context.Context, userID, spaceID string) ([]store.Session, error) {
	if _, err := s.spaces.Get(ctx, userID, spaceID); err != nil {
		return nil, err
	}
	return s.store.ListSessions(ctx, spaceID)
}

// ListMine 返回当前用户可见的全部会话（左侧树一次取数）。
func (s *Service) ListMine(ctx context.Context, userID string) ([]store.Session, error) {
	return s.store.ListUserSessions(ctx, userID)
}

// Create 在空间下新建会话，标题默认为「新会话」。
func (s *Service) Create(ctx context.Context, userID, spaceID, rawTitle string) (store.Session, error) {
	sp, err := s.spaces.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
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

// Rename 手动重命名会话；命名后不再被首条消息自动改写。
func (s *Service) Rename(ctx context.Context, userID, sessionID, rawTitle string) (store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}
	if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
		return store.Session{}, err
	}
	sess.Title = title
	sess.TitleAuto = false
	return sess, nil
}

// Delete 删除会话，需要空间写权限；消息记录保留但不再展示。
func (s *Service) Delete(ctx context.Context, userID, sessionID string) error {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}
	if err := s.store.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	s.log.Info("会话已删除", "session_id", sess.ID, "space_id", sess.SpaceID, "by", userID)
	return nil
}

// Messages 返回会话消息。
func (s *Service) Messages(ctx context.Context, userID, sessionID string) ([]store.Message, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	return s.store.ListMessages(ctx, sessionID)
}

// AppendMessage 追加一条用户消息；若标题仍是自动状态，用首条消息生成标题。
func (s *Service) AppendMessage(ctx context.Context, userID, sessionID, content string) (store.Message, store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Message{}, store.Session{}, ErrForbidden
	}

	text := strings.TrimSpace(content)
	if text == "" {
		return store.Message{}, store.Session{}, ErrMessageEmpty
	}
	if utf8.RuneCountInString(text) > maxMessageRunes {
		return store.Message{}, store.Session{}, ErrMessageTooLong
	}

	msg, err := s.store.CreateMessage(ctx, sessionID, "user", text)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	now := time.Now()
	sess.LastMessageAt = &now

	// 首条消息决定会话名：命名完成后 title_auto 置为 false，后续消息不再覆盖
	if sess.TitleAuto {
		if title := summarize(text); title != "" {
			if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
				s.log.Warn("会话标题自动生成失败", "session_id", sessionID, "err", err)
			} else {
				sess.Title = title
				sess.TitleAuto = false
			}
		}
	}

	// 回读一次，保证返回的 updated_at / last_message_at 与实际一致
	if latest, err := s.store.GetSession(ctx, sessionID); err == nil {
		sess = latest
	}
	return msg, sess, nil
}

// load 读取会话并校验当前用户对所属空间的可见性。
func (s *Service) load(ctx context.Context, userID, sessionID string) (store.Session, store.Space, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return store.Session{}, store.Space{}, err
	}

	sp, err := s.spaces.Get(ctx, userID, sess.SpaceID)
	if err != nil {
		if errors.Is(err, space.ErrForbidden) {
			return store.Session{}, store.Space{}, ErrForbidden
		}
		return store.Session{}, store.Space{}, err
	}
	return sess, sp, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return defaultTitle, nil
	}
	if utf8.RuneCountInString(title) > maxTitleRunes || strings.ContainsAny(title, "\r\n\t") {
		return "", ErrTitleInvalid
	}
	return title, nil
}

// summarize 把首条消息压成标题：合并空白，截断到固定字数。
func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) <= autoTitleRunes {
		return flat
	}
	return string(runes[:autoTitleRunes]) + "…"
}

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}
