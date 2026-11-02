package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/autohome/auto-agent/internal/store"
	"github.com/jackc/pgx/v5"
)

// CreateAuthSession 写入登录会话，同一 token 覆盖写入。
func (d *DB) CreateAuthSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
insert into auth_session (token_hash, user_id, expires_at)
values ($1, $2::uuid, $3)
on conflict (token_hash) do update
set user_id = excluded.user_id,
    expires_at = excluded.expires_at,
    last_seen_at = now()`,
		tokenHash, userID, expiresAt)
	return err
}

// GetAuthSession 读取未过期的登录会话。
func (d *DB) GetAuthSession(ctx context.Context, tokenHash string) (store.AuthSession, error) {
	const q = `
select token_hash, user_id::text, expires_at
from auth_session
where token_hash = $1
  and expires_at > now()`

	var s store.AuthSession
	err := d.pool.QueryRow(ctx, q, tokenHash).Scan(&s.TokenHash, &s.UserID, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AuthSession{}, store.ErrNotFound
	}
	if err != nil {
		return store.AuthSession{}, err
	}
	return s, nil
}

// DeleteAuthSession 让登录会话立即失效。
func (d *DB) DeleteAuthSession(ctx context.Context, tokenHash string) error {
	_, err := d.pool.Exec(ctx, `delete from auth_session where token_hash = $1`, tokenHash)
	return err
}
