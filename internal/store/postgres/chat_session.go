package postgres

import (
	"context"
	"errors"

	"github.com/autohome/auto-agent/internal/store"
	"github.com/jackc/pgx/v5"
)

const sessionColumns = `id::text, space_id::text, title, title_auto, created_by::text,
       last_message_at, status, created_at, updated_at`

func scanSession(row pgx.Row) (store.Session, error) {
	var s store.Session
	if err := row.Scan(&s.ID, &s.SpaceID, &s.Title, &s.TitleAuto, &s.CreatedBy,
		&s.LastMessageAt, &s.Status, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Session{}, store.ErrNotFound
		}
		return store.Session{}, err
	}
	return s, nil
}

// CreateSession 在指定空间下创建聊天会话。
func (d *DB) CreateSession(ctx context.Context, spaceID, userID, title string) (store.Session, error) {
	const q = `
insert into session (space_id, title, created_by)
values ($1::uuid, $2, $3::uuid)
returning ` + sessionColumns

	return scanSession(d.pool.QueryRow(ctx, q, spaceID, title, userID))
}

// ListSessions 返回空间下的会话，最近活动的在前。
func (d *DB) ListSessions(ctx context.Context, spaceID string) ([]store.Session, error) {
	const q = `
select ` + sessionColumns + `
from session
where space_id = $1::uuid
  and status = 'active'
order by coalesce(last_message_at, created_at) desc`

	rows, err := d.pool.Query(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSessions(rows)
}

// ListUserSessions 返回用户在可见空间下的全部会话。
func (d *DB) ListUserSessions(ctx context.Context, userID string) ([]store.Session, error) {
	const q = `
select ` + "s.id::text, s.space_id::text, s.title, s.title_auto, s.created_by::text, " +
		`s.last_message_at, s.status, s.created_at, s.updated_at` + `
from session s
join space sp on sp.id = s.space_id and sp.deleted_at is null
join space_member m on m.space_id = sp.id and m.user_id = $1::uuid
where s.status = 'active'
  and (m.expire_at is null or m.expire_at > now())
order by coalesce(s.last_message_at, s.created_at) desc`

	rows, err := d.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSessions(rows)
}

func collectSessions(rows pgx.Rows) ([]store.Session, error) {
	out := make([]store.Session, 0, 16)
	for rows.Next() {
		var s store.Session
		if err := rows.Scan(&s.ID, &s.SpaceID, &s.Title, &s.TitleAuto, &s.CreatedBy,
			&s.LastMessageAt, &s.Status, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSession 按 ID 查询会话。
func (d *DB) GetSession(ctx context.Context, id string) (store.Session, error) {
	const q = `
select ` + sessionColumns + `
from session
where id::text = $1
  and status = 'active'`

	return scanSession(d.pool.QueryRow(ctx, q, id))
}

// RenameSession 修改会话标题。
func (d *DB) RenameSession(ctx context.Context, id, title string, auto bool) error {
	tag, err := d.pool.Exec(ctx, `
update session
set title = $2, title_auto = $3, updated_at = now()
where id::text = $1 and status = 'active'`, id, title, auto)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteSession 软删会话，消息记录保留以便追溯。
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `
update session
set status = 'deleted', updated_at = now()
where id::text = $1 and status = 'active'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// TouchSession 更新最近活动时间。
func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `
update session set last_message_at = now(), updated_at = now()
where id::text = $1`, id)
	return err
}

// CreateMessage 写入一条消息。
func (d *DB) CreateMessage(ctx context.Context, sessionID, role, content string) (store.Message, error) {
	const q = `
insert into message (session_id, role, content)
values ($1::uuid, $2, $3)
returning id::text, session_id::text, role, content, created_at`

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, sessionID, role, content).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// ListMessages 按时间顺序返回会话消息。
func (d *DB) ListMessages(ctx context.Context, sessionID string) ([]store.Message, error) {
	const q = `
select id::text, session_id::text, role, content, created_at
from message
where session_id = $1::uuid
order by created_at asc, id asc`

	rows, err := d.pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Message, 0, 16)
	for rows.Next() {
		var m store.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMessages 返回会话的消息条数。
func (d *DB) CountMessages(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `select count(*) from message where session_id = $1::uuid`, sessionID).Scan(&n)
	return n, err
}
