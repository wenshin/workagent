package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// TouchRun 刷新心跳：只要执行还在产生事件，这一轮就不算死。
func (d *DB) TouchRun(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	const q = `update run set heartbeat_at = now() where id = $1::uuid`
	_, err := d.pool.Exec(ctx, q, runID)
	return err
}

// SaveRunResume 覆盖断点快照。每轮一次、只保留最新值：
// 历史轨迹在 run_step 里，这里只负责「从哪接着走」。
func (d *DB) SaveRunResume(ctx context.Context, runID string, payload []byte) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	const q = `
insert into run_resume (run_id, payload, updated_at)
values ($1::uuid, $2::jsonb, now())
on conflict (run_id) do update set payload = excluded.payload, updated_at = now()`

	_, err := d.pool.Exec(ctx, q, runID, payload)
	return err
}

// GetRunResume 取断点快照。
func (d *DB) GetRunResume(ctx context.Context, runID string) ([]byte, bool, error) {
	const q = `select payload from run_resume where run_id = $1::uuid`

	var payload []byte
	err := d.pool.QueryRow(ctx, q, runID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

// DeleteRunResume 丢弃断点。
func (d *DB) DeleteRunResume(ctx context.Context, runID string) error {
	const q = `delete from run_resume where run_id = $1::uuid`
	_, err := d.pool.Exec(ctx, q, runID)
	return err
}

// ResumableRun 找会话里最近一次留着断点的执行。
//
// 不按状态过滤：断点只在「系统侧中断」时保留，用户主动停止的已经当场删掉了，
// 所以「有断点」本身就是可继续的判据。
func (d *DB) ResumableRun(ctx context.Context, sessionID string) (store.Run, bool, error) {
	const q = `
select r.id::text, r.session_id::text, r.status, r.budget, r.usage,
       r.provider, r.model, r.error, r.started_at, r.ended_at
from run r
join run_resume s on s.run_id = r.id
where r.session_id = $1::uuid
order by r.started_at desc
limit 1`

	var r store.Run
	err := d.pool.QueryRow(ctx, q, sessionID).Scan(
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

// SweepStaleRuns 收尾上一个进程留下的执行。
//
// 只动「心跳停了」的那批：长任务跑两小时也该有事件，没有事件说明进程已经不在了。
// 这些 run 的断点保留着，用户刷新后可以在会话里点「继续」接着走；
// 它们残留的流式占位消息要一起标成中断，否则会永远显示「生成中」。
func (d *DB) SweepStaleRuns(ctx context.Context, staleAfter time.Duration, reason string) (int, error) {
	if staleAfter <= 0 {
		staleAfter = 10 * time.Minute
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
update run
set status = 'failed', error = $2, ended_at = now()
where status in ('running', 'waiting_approval')
  and heartbeat_at < now() - ($1 * interval '1 second')
returning session_id::text`

	rows, err := tx.Query(ctx, q, staleAfter.Seconds(), reason)
	if err != nil {
		return 0, err
	}
	sessions := make([]string, 0, 4)
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return 0, err
		}
		sessions = append(sessions, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(sessions) == 0 {
		return 0, tx.Commit(ctx)
	}

	// 这些会话里不会再有活的 run 了，占位消息的收尾交给这里：
	if err := settleStreamingMessages(ctx, tx, sessions); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(sessions), nil
}

// AbandonRun 收尾一条「库里还在 running、进程里却没有执行体」的执行。
//
// 与清扫共用同一套占位消息结算：空占位直接删（留一条空消息在列表里只会让人
// 以为出现了空白回答），已经有正文的标成中断，让「这一轮断在哪儿」看得见。
// 断点不删：这条 run 可能是崩溃前写下了断点的，用户仍可以在会话里点「继续」。
//
// staleAfter > 0 时带一道心跳判据：心跳还在跳说明执行在别的副本上活着，
// 本进程只是拿不到它的流，不能替它决定结局。
func (d *DB) AbandonRun(ctx context.Context, runID, sessionID, reason string, staleAfter time.Duration) error {
	if runID == "" || sessionID == "" {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 状态与心跳条件保证幂等：并发的两次刷新只有一个能真正收尾
	const q = `
update run
set status = 'failed', error = $3, ended_at = now()
where id = $1::uuid and session_id = $2::uuid
  and status in ('running', 'waiting_approval')
  and ($4 <= 0 or heartbeat_at < now() - ($4 * interval '1 second'))`
	tag, err := tx.Exec(ctx, q, runID, sessionID, reason, staleAfter.Seconds())
	if err != nil {
		return err
	}
	// 没改到行说明这一轮已经被别人收了尾（用户点了继续、或另一个请求先动手）：
	// 此时会话里可能已经有新一轮的占位消息，绝不能再当成这一轮的一起结算。
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if err := settleStreamingMessages(ctx, tx, []string{sessionID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// settleStreamingMessages 结算这些会话里的流式占位消息：
// 空占位删掉，已经有正文的标成中断（正文保留，别让人以为出现了空白回答）。
func settleStreamingMessages(ctx context.Context, tx pgx.Tx, sessionIDs []string) error {
	const qdel = `
delete from message
where streaming and coalesce(content, '') = '' and session_id = any($1::uuid[])`
	if _, err := tx.Exec(ctx, qdel, sessionIDs); err != nil {
		return err
	}
	const qfin = `
update message
set streaming = false, interrupted = true
where streaming and session_id = any($1::uuid[])`
	_, err := tx.Exec(ctx, qfin, sessionIDs)
	return err
}
