package postgres

import (
	"context"

	"github.com/autohome/auto-agent/internal/store"
)

// CreateSession 在指定空间下创建聊天会话。
func (d *DB) CreateSession(ctx context.Context, spaceID, userID, title string) (store.Session, error) {
	const q = `
insert into session (space_id, title, created_by)
values ($1::uuid, $2, $3::uuid)
returning id::text, space_id::text, title, created_by::text, last_message_at, status, created_at`

	var s store.Session
	if err := d.pool.QueryRow(ctx, q, spaceID, title, userID).Scan(
		&s.ID, &s.SpaceID, &s.Title, &s.CreatedBy, &s.LastMessageAt, &s.Status, &s.CreatedAt,
	); err != nil {
		return store.Session{}, err
	}
	return s, nil
}

// ListSessions 返回空间下的聊天会话，最近更新的在前。
func (d *DB) ListSessions(ctx context.Context, spaceID string) ([]store.Session, error) {
	const q = `
select id::text, space_id::text, title, created_by::text, last_message_at, status, created_at
from session
where space_id = $1::uuid
  and status = 'active'
order by coalesce(last_message_at, created_at) desc`

	rows, err := d.pool.Query(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Session, 0, 8)
	for rows.Next() {
		var s store.Session
		if err := rows.Scan(&s.ID, &s.SpaceID, &s.Title, &s.CreatedBy,
			&s.LastMessageAt, &s.Status, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
