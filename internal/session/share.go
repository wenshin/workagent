package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/workagent/auto-agent/internal/store"
)

const (
	// defaultShareTTL 是排障链接的默认有效期：够运维看一眼，又不至于长期敞着
	defaultShareTTL = 24 * time.Hour
	// maxShareTTL 是上限：再长就该重新生成，而不是把一条链接当长期凭证用
	maxShareTTL = 7 * 24 * time.Hour
)

// ErrShareGone 表示链接不存在、已过期或已吊销——三者对外一律不区分，避免探测。
var ErrShareGone = errors.New("链接无效或已过期")

// CreateShare 生成一条只读排障链接，返回原文（只此一次）与记录。
//
// 会话只对创建者可见（§15.12.3）之后，运维排障看不到任何内容；与其开一个
// 「管理员能读所有人会话」的旁路，不如让用户自己把这段对话临时授权出去。
// 平台侧只存 token 的 sha256，链接一旦丢失，平台也拿不回。
func (s *Service) CreateShare(ctx context.Context, userID, sessionID string, ttl time.Duration) (store.Share, string, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return store.Share{}, "", err
	}
	if ttl <= 0 {
		ttl = defaultShareTTL
	}
	if ttl > maxShareTTL {
		ttl = maxShareTTL
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return store.Share{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	share, err := s.store.CreateShare(ctx, store.Share{
		SessionID: sessionID,
		TokenHash: hashToken(token),
		ExpiresAt: time.Now().Add(ttl),
		CreatedBy: userID,
	})
	if err != nil {
		return store.Share{}, "", err
	}

	s.audit(ctx, userID, "session.share.create", sessionID, map[string]any{
		"share_id": share.ID,
		"expires":  share.ExpiresAt.Format(time.RFC3339),
	})
	return share, token, nil
}

// RevokeShare 吊销一条链接。
func (s *Service) RevokeShare(ctx context.Context, userID, sessionID, shareID string) error {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return err
	}
	shares, err := s.store.ListShares(ctx, sessionID)
	if err != nil {
		return err
	}
	found := false
	for _, sh := range shares {
		if sh.ID == shareID {
			found = true
			break
		}
	}
	if !found {
		return store.ErrNotFound
	}
	if err := s.store.RevokeShare(ctx, shareID); err != nil {
		return err
	}
	s.audit(ctx, userID, "session.share.revoke", sessionID, map[string]any{"share_id": shareID})
	return nil
}

// Shares 返回会话当前的链接记录（不含 token 原文，平台也没有）。
func (s *Service) Shares(ctx context.Context, userID, sessionID string) ([]store.Share, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	return s.store.ListShares(ctx, sessionID)
}

// ViewShared 用链接原文读出会话与消息。这是唯一不需要登录的读取路径。
func (s *Service) ViewShared(ctx context.Context, token, ip, ua string) (store.Session, []store.Message, error) {
	share, err := s.store.GetShareByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Session{}, nil, ErrShareGone
		}
		return store.Session{}, nil, err
	}
	if share.Expired(time.Now()) {
		return store.Session{}, nil, ErrShareGone
	}

	sess, err := s.store.GetSession(ctx, share.SessionID)
	if err != nil {
		return store.Session{}, nil, ErrShareGone
	}
	msgs, err := s.store.ListMessages(ctx, share.SessionID)
	if err != nil {
		return store.Session{}, nil, err
	}

	_ = s.store.TouchShareView(ctx, share.ID)
	s.audit(ctx, share.CreatedBy, "session.share.view", share.SessionID, map[string]any{
		"share_id": share.ID,
		"ip":       ip,
		"ua":       ua,
	})
	return sess, msgs, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) audit(ctx context.Context, actor, action, object string, detail map[string]any) {
	data, _ := json.Marshal(detail)
	if err := s.store.AppendAuditLog(ctx, store.AuditEntry{
		Actor:  actor,
		Action: action,
		Object: object,
		Result: "ok",
		Detail: data,
	}); err != nil {
		s.log.Warn("写审计失败", "action", action, "object", object, "err", err)
	}
}
