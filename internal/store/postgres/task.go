package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// ---- 任务派发（§15.12.4）----

const taskColumns = `id::text, space_id::text, parent_session_id::text,
       coalesce(child_session_id::text, ''), title, brief, assignee::text, status,
       waiting_since, dispatch_count, result, created_by::text, created_at, updated_at`

func scanTask(row pgx.Row) (store.Task, error) {
	var t store.Task
	var status string
	if err := row.Scan(&t.ID, &t.SpaceID, &t.ParentSessionID, &t.ChildSessionID,
		&t.Title, &t.Brief, &t.Assignee, &status,
		&t.WaitingSince, &t.DispatchCount, &t.Result, &t.CreatedBy,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		return store.Task{}, err
	}
	t.Status = store.TaskStatus(status)
	return t, nil
}

// CreateTask 落一条派发记录。
func (d *DB) CreateTask(ctx context.Context, t store.Task) (store.Task, error) {
	const q = `
insert into session_task (space_id, parent_session_id, child_session_id, title, brief,
                          assignee, status, waiting_since, dispatch_count, created_by)
values ($1::uuid, $2::uuid, nullif($3, '')::uuid, $4, $5, $6::uuid, $7, $8, $9, $10::uuid)
returning ` + taskColumns

	return scanTask(d.pool.QueryRow(ctx, q, t.SpaceID, t.ParentSessionID, t.ChildSessionID,
		t.Title, t.Brief, t.Assignee, string(t.Status), t.WaitingSince, t.DispatchCount, t.CreatedBy))
}

// GetTask 按 id 取任务。
func (d *DB) GetTask(ctx context.Context, id string) (store.Task, error) {
	const q = `select ` + taskColumns + ` from session_task where id = $1::uuid`
	task, err := scanTask(d.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Task{}, store.ErrNotFound
	}
	return task, err
}

// GetTaskByChild 按子会话反查任务。
func (d *DB) GetTaskByChild(ctx context.Context, childSessionID string) (store.Task, error) {
	const q = `
select ` + taskColumns + `
from session_task
where child_session_id = $1::uuid
order by created_at desc
limit 1`

	task, err := scanTask(d.pool.QueryRow(ctx, q, childSessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Task{}, store.ErrNotFound
	}
	return task, err
}

// ListTasks 返回父会话派发出的任务，最新的在前。
func (d *DB) ListTasks(ctx context.Context, parentSessionID string) ([]store.Task, error) {
	const q = `
select ` + taskColumns + `
from session_task
where parent_session_id = $1::uuid
order by created_at desc`

	rows, err := d.pool.Query(ctx, q, parentSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Task, 0, 8)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTask 覆盖可变字段。
func (d *DB) UpdateTask(ctx context.Context, t store.Task) error {
	const q = `
update session_task
set child_session_id = nullif($2, '')::uuid,
    status           = $3,
    waiting_since    = $4,
    dispatch_count   = $5,
    result           = $6,
    updated_at       = now()
where id = $1::uuid`

	_, err := d.pool.Exec(ctx, q, t.ID, t.ChildSessionID, string(t.Status),
		t.WaitingSince, t.DispatchCount, t.Result)
	return err
}

// ---- 排障只读链接（§15.12.3）----

const shareColumns = `id::text, session_id::text, token_hash, expires_at, revoked_at,
       view_count, created_by::text, created_at`

// CreateShare 落一条分享链接，库里只存 token 的 sha256。
func (d *DB) CreateShare(ctx context.Context, s store.Share) (store.Share, error) {
	const q = `
insert into session_share (session_id, token_hash, expires_at, created_by)
values ($1::uuid, $2, $3, $4::uuid)
returning ` + shareColumns

	var out store.Share
	if err := d.pool.QueryRow(ctx, q, s.SessionID, s.TokenHash, s.ExpiresAt, s.CreatedBy).Scan(
		&out.ID, &out.SessionID, &out.TokenHash, &out.ExpiresAt, &out.RevokedAt,
		&out.ViewCount, &out.CreatedBy, &out.CreatedAt,
	); err != nil {
		return store.Share{}, err
	}
	return out, nil
}

// GetShareByHash 按 token 摘要取链接；公开访问只有这一条查询，必须走唯一索引。
func (d *DB) GetShareByHash(ctx context.Context, hash string) (store.Share, error) {
	const q = `select ` + shareColumns + ` from session_share where token_hash = $1`

	var out store.Share
	err := d.pool.QueryRow(ctx, q, hash).Scan(
		&out.ID, &out.SessionID, &out.TokenHash, &out.ExpiresAt, &out.RevokedAt,
		&out.ViewCount, &out.CreatedBy, &out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Share{}, store.ErrNotFound
	}
	if err != nil {
		return store.Share{}, err
	}
	return out, nil
}

// RevokeShare 吊销一条链接；已吊销的不再改动。
func (d *DB) RevokeShare(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx,
		`update session_share set revoked_at = now() where id = $1::uuid and revoked_at is null`, id)
	return err
}

// ListShares 返回会话的分享记录。
func (d *DB) ListShares(ctx context.Context, sessionID string) ([]store.Share, error) {
	const q = `select ` + shareColumns + ` from session_share where session_id = $1::uuid order by created_at desc`

	rows, err := d.pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Share, 0, 4)
	for rows.Next() {
		var s store.Share
		if err := rows.Scan(&s.ID, &s.SessionID, &s.TokenHash, &s.ExpiresAt, &s.RevokedAt,
			&s.ViewCount, &s.CreatedBy, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TouchShareView 计数一次访问。
func (d *DB) TouchShareView(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx,
		`update session_share set view_count = view_count + 1 where id = $1::uuid`, id)
	return err
}

// GetActiveRun 返回会话当前活跃的 run：刷新后前端据此决定要不要续流。
//
// staleAfter > 0 时按心跳判活：心跳停了的 run 属于上一个进程，回答「没在跑」
// 比回答「在跑」诚实——后者会让前端一直等一条不会再来的流。
func (d *DB) GetActiveRun(ctx context.Context, sessionID string, staleAfter time.Duration) (store.Run, bool, error) {
	const q = `
select id::text, session_id::text, status, budget, usage, provider, model, error, started_at, ended_at
from run
where session_id = $1::uuid
  and status in ('running', 'waiting_approval')
  -- 停在审批上的那一轮带着 waiting_approval 落终态（ended_at 非空）：
  -- 它是「已暂停」，不是「还在跑」，不能当成活跃执行让前端挂上去
  and ended_at is null
  and ($2 <= 0 or heartbeat_at > now() - ($2 * interval '1 second'))
order by started_at desc
limit 1`

	var r store.Run
	err := d.pool.QueryRow(ctx, q, sessionID, staleAfter.Seconds()).Scan(
		&r.ID, &r.SessionID, &r.Status, &r.Budget, &r.Usage,
		&r.Provider, &r.Model, &r.Error, &r.StartedAt, &r.EndedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, false, nil
	}
	if err != nil {
		return store.Run{}, false, err
	}
	return r, true, nil
}

// SearchUsers 按姓名/工号/邮箱模糊匹配，供派发任务时把人名解析成具体用户。
func (d *DB) SearchUsers(ctx context.Context, keyword string, limit int) ([]store.User, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	const q = `
select id::text
from app_user
where status = 'active'
  and ($1 = '' or name ilike '%' || $1 || '%'
       or coalesce(job_number, '') ilike '%' || $1 || '%'
       or coalesce(email, '') ilike '%' || $1 || '%')
order by name
limit $2`

	rows, err := d.pool.Query(ctx, q, keyword, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]store.User, 0, len(ids))
	for _, id := range ids {
		u, err := d.getUserByID(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}
