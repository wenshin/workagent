package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

const sessionColumns = `id::text, space_id::text, coalesce(dir, ''), title, title_auto, coalesce(skill_path, ''),
       created_by::text, last_message_at, status, created_at, updated_at,
       coalesce(agent_id, ''), agent_version, agent_prompt`

// sessionColumnsWithAlias 是带表别名的版本，列顺序必须与 scanSession 一致。
const sessionColumnsWithAlias = `s.id::text, s.space_id::text, coalesce(s.dir, ''), s.title, s.title_auto, coalesce(s.skill_path, ''),
       s.created_by::text, s.last_message_at, s.status, s.created_at, s.updated_at,
       coalesce(s.agent_id, ''), s.agent_version, s.agent_prompt`

func scanSession(row pgx.Row) (store.Session, error) {
	var s store.Session
	if err := row.Scan(&s.ID, &s.SpaceID, &s.Dir, &s.Title, &s.TitleAuto, &s.SkillPath, &s.CreatedBy,
		&s.LastMessageAt, &s.Status, &s.CreatedAt, &s.UpdatedAt,
		&s.AgentID, &s.AgentVersion, &s.AgentPrompt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Session{}, store.ErrNotFound
		}
		return store.Session{}, err
	}
	return s, nil
}

// CreateSession 在指定空间（或空间内某个目录）下创建聊天会话。
func (d *DB) CreateSession(ctx context.Context, spaceID, userID, title, dir, agentID, agentVersion, agentPrompt string) (store.Session, error) {
	const q = `
insert into session (space_id, dir, title, created_by, agent_id, agent_version, agent_prompt)
values ($1::uuid, nullif($2, ''), $3, $4::uuid, nullif($5, ''), $6, $7)
returning ` + sessionColumns

	return scanSession(d.pool.QueryRow(ctx, q, spaceID, dir, title, userID, agentID, agentVersion, agentPrompt))
}

// UpdateSessionAgent 重新绑定智能体，并把当时的提示词一起固化。
func (d *DB) UpdateSessionAgent(ctx context.Context, id, agentID, agentVersion, agentPrompt string) error {
	const q = `
update session
set agent_id = nullif($2, ''), agent_version = $3, agent_prompt = $4, updated_at = now()
where id::text = $1 and status = 'active'`

	tag, err := d.pool.Exec(ctx, q, id, agentID, agentVersion, agentPrompt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
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
select ` + sessionColumnsWithAlias + `
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
		if err := rows.Scan(&s.ID, &s.SpaceID, &s.Dir, &s.Title, &s.TitleAuto, &s.SkillPath, &s.CreatedBy,
			&s.LastMessageAt, &s.Status, &s.CreatedAt, &s.UpdatedAt,
			&s.AgentID, &s.AgentVersion, &s.AgentPrompt); err != nil {
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

// ClearSessionsDir 把目录（含子目录）下的会话移回空间根：目录被删后会话不跟着消失。
func (d *DB) ClearSessionsDir(ctx context.Context, spaceID, dir string) error {
	_, err := d.pool.Exec(ctx, `
update session
set dir = null, updated_at = now()
where space_id = $1::uuid
  and status = 'active'
  and (dir = $2 or dir like $2 || '/%')`, spaceID, dir)
	return err
}

// MoveSessionsDir 目录改名后同步会话的 dir 前缀。
func (d *DB) MoveSessionsDir(ctx context.Context, spaceID, from, to string) error {
	if from == to {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
update session
set dir = $3 || substr(dir, length($2) + 1), updated_at = now()
where space_id = $1::uuid
  and status = 'active'
  and (dir = $2 or dir like $2 || '/%')`, spaceID, from, to)
	return err
}

// UpdateSessionSkill 绑定会话技能；空串表示清除绑定。
func (d *DB) UpdateSessionSkill(ctx context.Context, id, path string) error {
	_, err := d.pool.Exec(ctx,
		`update session set skill_path = $2, updated_at = now() where id = $1::uuid`, id, path)
	return err
}

const messageColumns = `id::text, session_id::text, role, content, reasoning,
       coalesce(parts, '[]'::jsonb), in_tokens, out_tokens, duration_ms, created_at`

// CreateMessage 写入一条消息（用户/系统消息，无执行轨迹）。
func (d *DB) CreateMessage(ctx context.Context, sessionID, role, content string) (store.Message, error) {
	const q = `
insert into message (session_id, role, content)
values ($1::uuid, $2, $3)
returning ` + messageColumns

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, sessionID, role, content).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// CreateAssistantMessage 写入助手消息，包含思考过程、工具调用轨迹与这一轮的用量耗时。
func (d *DB) CreateAssistantMessage(ctx context.Context, sessionID, content, reasoning string, parts []byte, stats store.MessageStats) (store.Message, error) {
	if len(parts) == 0 {
		parts = []byte("[]")
	}

	const q = `
insert into message (session_id, role, content, reasoning, parts, in_tokens, out_tokens, duration_ms)
values ($1::uuid, 'assistant', $2, $3, $4::jsonb, $5, $6, $7)
returning ` + messageColumns

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, sessionID, content, reasoning, parts,
		stats.InTokens, stats.OutTokens, stats.DurationMS).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// ListMessages 按时间顺序返回会话消息。
func (d *DB) ListMessages(ctx context.Context, sessionID string) ([]store.Message, error) {
	const q = `
select ` + messageColumns + `
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
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
			&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS, &m.CreatedAt); err != nil {
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
