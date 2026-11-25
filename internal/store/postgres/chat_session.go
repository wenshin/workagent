package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

const sessionColumns = `id::text, space_id::text, coalesce(dir, ''), title, title_auto, coalesce(skill_path, ''),
       created_by::text, last_message_at, status, created_at, updated_at,
       coalesce(agent_id, ''), coalesce(agent_space_id::text, ''), agent_version, agent_prompt,
       coalesce(channel, '')`

// sessionColumnsWithAlias 是带表别名的版本，列顺序必须与 scanSession 一致。
const sessionColumnsWithAlias = `s.id::text, s.space_id::text, coalesce(s.dir, ''), s.title, s.title_auto, coalesce(s.skill_path, ''),
       s.created_by::text, s.last_message_at, s.status, s.created_at, s.updated_at,
       coalesce(s.agent_id, ''), coalesce(s.agent_space_id::text, ''), s.agent_version, s.agent_prompt,
       coalesce(s.channel, '')`

func scanSession(row pgx.Row) (store.Session, error) {
	var s store.Session
	if err := row.Scan(&s.ID, &s.SpaceID, &s.Dir, &s.Title, &s.TitleAuto, &s.SkillPath, &s.CreatedBy,
		&s.LastMessageAt, &s.Status, &s.CreatedAt, &s.UpdatedAt,
		&s.AgentID, &s.AgentSpaceID, &s.AgentVersion, &s.AgentPrompt, &s.Channel); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Session{}, store.ErrNotFound
		}
		return store.Session{}, err
	}
	return s, nil
}

// CreateSession 在指定空间（或空间内某个目录）下创建聊天会话。
func (d *DB) CreateSession(ctx context.Context, spaceID, userID, title, dir, agentID, agentVersion, agentPrompt string) (store.Session, error) {
	return d.CreateSessionTitled(ctx, spaceID, userID, title, dir, "", agentID, agentVersion, agentPrompt, true)
}

// CreateSessionTitled 与 CreateSession 相同，但可以关掉「首条消息改写标题」。
func (d *DB) CreateSessionTitled(ctx context.Context, spaceID, userID, title, dir, agentSpaceID, agentID, agentVersion, agentPrompt string, titleAuto bool) (store.Session, error) {
	const q = `
insert into session (space_id, dir, title, title_auto, created_by, agent_id, agent_space_id, agent_version, agent_prompt)
values ($1::uuid, nullif($2, ''), $3, $4, $5::uuid, nullif($6, ''), nullif($7, '')::uuid, $8, $9)
returning ` + sessionColumns

	return scanSession(d.pool.QueryRow(ctx, q, spaceID, dir, title, titleAuto, userID, agentID, agentSpaceID, agentVersion, agentPrompt))
}

// UpdateSessionAgent 重新绑定智能体，并把当时的提示词一起固化。
func (d *DB) UpdateSessionAgent(ctx context.Context, id, agentSpaceID, agentID, agentVersion, agentPrompt string) error {
	const q = `
update session
set agent_id = nullif($2, ''), agent_space_id = nullif($3, '')::uuid,
    agent_version = $4, agent_prompt = $5, updated_at = now()
where id::text = $1 and status = 'active'`

	tag, err := d.pool.Exec(ctx, q, id, agentID, agentSpaceID, agentVersion, agentPrompt)
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
			&s.AgentID, &s.AgentSpaceID, &s.AgentVersion, &s.AgentPrompt, &s.Channel); err != nil {
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

// GetOrCreateChannelSession 取「该渠道 + 该用户」的钉死会话，没有就现建一个。
//
// 走一条 upsert 而不是「先查再插」：(channel, created_by) 上有部分唯一索引，
// 两个渠道进程同时投递同一个人时，后到的那条会改走 do nothing，再读到已存在
// 的那一行。先查再插在并发下会有一个必然失败的插入，投递侧只能重试。
func (d *DB) GetOrCreateChannelSession(ctx context.Context, channel, spaceID, userID, title string, agentSpaceID, agentID, agentVersion, agentPrompt string) (store.Session, bool, error) {
	const q = `
insert into session (space_id, title, title_auto, created_by, channel, agent_space_id, agent_id, agent_version, agent_prompt)
values ($1::uuid, $2, false, $3::uuid, $4, nullif($5, '')::uuid, nullif($6, ''), $7, $8)
on conflict (channel, created_by) where channel is not null do nothing
returning ` + sessionColumns

	sess, err := scanSession(d.pool.QueryRow(ctx, q, spaceID, title, userID, channel, agentSpaceID, agentID, agentVersion, agentPrompt))
	if err == nil {
		return sess, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Session{}, false, err
	}

	// 冲突（已存在）或被软删（status 不是 active）时走到这里：回到读取。
	const sel = `
select ` + sessionColumns + `
from session
where channel = $1 and created_by = $2::uuid
order by created_at desc
limit 1`
	sess, err = scanSession(d.pool.QueryRow(ctx, sel, channel, userID))
	if err != nil {
		return store.Session{}, false, err
	}
	return sess, false, nil
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
       coalesce(parts, '[]'::jsonb), in_tokens, out_tokens, duration_ms,
       streaming, interrupted, source, created_at`

// CreateMessage 写入一条消息（用户/系统消息，无执行轨迹）。
func (d *DB) CreateMessage(ctx context.Context, sessionID, role, content string) (store.Message, error) {
	return d.CreateMessageFrom(ctx, sessionID, role, content, "user")
}

// CreateMessageFrom 写入一条带来源的消息：task_result / external 要能被 UI 与提示词区分。
func (d *DB) CreateMessageFrom(ctx context.Context, sessionID, role, content, source string) (store.Message, error) {
	if source == "" {
		source = "user"
	}
	const q = `
insert into message (session_id, role, content, source)
values ($1::uuid, $2, $3, $4)
returning ` + messageColumns

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, sessionID, role, content, source).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
		&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// CreateStreamingMessage 先占位一条「生成中」的助手消息。
//
// 只在首个增量到达时调用一次：占位让刷新后的列表能显示「这一轮确实开始了」，
// 正文仍然在结束时一次性覆盖，避免逐 token 写库（§15.12.2）。
func (d *DB) CreateStreamingMessage(ctx context.Context, sessionID string) (store.Message, error) {
	const q = `
insert into message (session_id, role, content, streaming)
values ($1::uuid, 'assistant', '', true)
returning ` + messageColumns

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, sessionID).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
		&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// FinishMessage 用最终正文覆盖占位消息，并把 streaming 置回 false。
func (d *DB) FinishMessage(ctx context.Context, id, content, reasoning string, parts []byte, stats store.MessageStats, interrupted bool) (store.Message, error) {
	if len(parts) == 0 {
		parts = []byte("[]")
	}
	const q = `
update message
set content = $2, reasoning = $3, parts = $4::jsonb,
    in_tokens = $5, out_tokens = $6, duration_ms = $7,
    streaming = false, interrupted = $8
where id = $1::uuid
returning ` + messageColumns

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, id, content, reasoning, parts,
		stats.InTokens, stats.OutTokens, stats.DurationMS, interrupted).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
		&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt,
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
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
		&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt,
	); err != nil {
		return store.Message{}, err
	}
	return m, nil
}

// MessageOfRun 读一次执行写下的那条助手消息。
//
// 判据是 run_step 里 type='message' 的载荷：它在落库的同一个位置写下，比按时间
// 猜「这一轮的最后一条」可靠。续跑靠它接着同一条消息写。
func (d *DB) MessageOfRun(ctx context.Context, runID string) (store.Message, bool, error) {
	if runID == "" {
		return store.Message{}, false, nil
	}
	const q = `
select m.id::text, m.session_id::text, m.role, m.content, m.reasoning,
       coalesce(m.parts, '[]'::jsonb), m.in_tokens, m.out_tokens, m.duration_ms,
       m.streaming, m.interrupted, m.source, m.created_at
from run_step s
join message m on m.id::text = s.payload->>'message_id'
where s.run_id = $1::uuid
  and s.type = 'message'
  and coalesce(s.payload->>'message_id', '') <> ''
order by s.seq desc
limit 1`

	var m store.Message
	if err := d.pool.QueryRow(ctx, q, runID).Scan(
		&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Parts,
		&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
		&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Message{}, false, nil
		}
		return store.Message{}, false, err
	}
	return m, true, nil
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
			&m.Stats.InTokens, &m.Stats.OutTokens, &m.Stats.DurationMS,
			&m.Streaming, &m.Interrupted, &m.Source, &m.CreatedAt); err != nil {
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
