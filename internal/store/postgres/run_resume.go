package postgres

import (
	"context"
	"encoding/json"
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

// SetRunStatus 只改状态。用于 running ↔ waiting_approval：
// 等人不是终态，不该写 ended_at，也不该被当成「跑完了」。
func (d *DB) SetRunStatus(ctx context.Context, runID, status string) error {
	if runID == "" || status == "" {
		return nil
	}
	// 只允许在「还在这一轮里」的状态之间切换：终态由 FinishRun 独占，
	// 别让等待结束时的一次状态回写把 failed 改成 running。
	const q = `
update run set status = $2, heartbeat_at = now()
where id = $1::uuid and status in ('running', 'waiting_approval')`
	_, err := d.pool.Exec(ctx, q, runID, status)
	return err
}

// PurgeStaleResumes 清掉过期断点，返回清理条数。
func (d *DB) PurgeStaleResumes(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		olderThan = 7 * 24 * time.Hour
	}
	const q = `delete from run_resume where updated_at < now() - ($1 * interval '1 second')`
	tag, err := d.pool.Exec(ctx, q, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
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

	// waiting_approval 一起收：进程没了就没有执行体在等，这一轮必须落终态，
	// 否则 active-run 会一直说「还在跑」。审批记录不动——它有自己的有效期，
	// 用户在窗口内点批准，会从断点把这一轮续起来（approval → resume）。
	const q = `
update run
set status = 'failed', error = $2, ended_at = now()
where status in ('running', 'waiting_approval')
  and heartbeat_at < now() - ($1 * interval '1 second')
returning id::text, session_id::text`

	rows, err := tx.Query(ctx, q, staleAfter.Seconds(), reason)
	if err != nil {
		return 0, err
	}
	type staleRun struct{ runID, sessionID string }
	stale := make([]staleRun, 0, 4)
	for rows.Next() {
		var r staleRun
		if err := rows.Scan(&r.runID, &r.sessionID); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// 这些会话里不会再有活的 run 了，占位消息的收尾交给这里。
	// 按 run 逐条结算而不是只按会话：要拿 run_step 还原中断前的工具轨迹。
	for _, r := range stale {
		if err := settleRunStreamingMessage(ctx, tx, r.runID, r.sessionID); err != nil {
			return 0, err
		}
	}

	// 还有一类占位没人结算：上一轮已经终态（被续跑接替、或被更早的清扫收掉），
	// 但消息还挂着 streaming=true。它们不会再被任何路径碰到，只能在这里自愈，
	// 否则界面永远看不到那一轮的工具轨迹。判据是「会话里没有任何活 run」，
	// 有活的说明执行还在别处，不能替它结算。
	if err := settleOrphanStreamingMessages(ctx, tx); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(stale), nil
}

// settleOrphanStreamingMessages 结算「没有活 run」的会话里残留的流式占位消息。
//
// 用「占位创建之前最近启动的那条 run」的 run_step 还原轨迹：占位是那一轮在首个
// 正文增量时建的，之后的 run（例如用户点了继续）不属于它。续跑那一轮如果没有
// 工具步骤，这里就只会删掉空占位，不会把别人的轨迹写进来。
func settleOrphanStreamingMessages(ctx context.Context, tx pgx.Tx) error {
	const q = `
select m.session_id::text, r.id::text
from message m
join lateral (
	select id from run
	where session_id = m.session_id
	  and started_at <= m.created_at
	order by started_at desc
	limit 1
) r on true
where m.streaming
  and not exists (
	select 1 from run a
	where a.session_id = m.session_id
	  and a.status in ('running', 'waiting_approval')
  )`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return err
	}
	type orphan struct{ runID, sessionID string }
	orphans := make([]orphan, 0, 4)
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.sessionID, &o.runID); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	done := make(map[string]bool, len(orphans))
	for _, o := range orphans {
		if done[o.sessionID] {
			continue
		}
		done[o.sessionID] = true
		if err := settleRunStreamingMessage(ctx, tx, o.runID, o.sessionID); err != nil {
			return err
		}
	}
	return nil
}

// AbandonRun 收尾一条「库里还在 running、进程里却没有执行体」的执行。
//
// 与清扫共用同一套占位消息结算：run_step 里已经发生的工具调用还原进 parts，
// 空占位（连轨迹都没有）直接删，已经有正文的标成中断，让「这一轮断在哪儿」看得见。
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
	if err := settleRunStreamingMessage(ctx, tx, runID, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InterruptStreamingMessage 结算一次执行留下的流式占位消息。
//
// 续跑（用户点「继续」）接手一条中断的 run 时用它：那条 run 已经被置为终态，
// 不会再走 AbandonRun，占位消息也就没人结算——它会永远挂在 streaming=true 上，
// 而中断前调过的工具（run_step 里有记录）在界面上一条都看不到。
func (d *DB) InterruptStreamingMessage(ctx context.Context, runID, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := settleRunStreamingMessage(ctx, tx, runID, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// messagePart 是 message.parts 的元素形状，与前端 MessagePart 一一对应。
type messagePart struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Args       string `json:"args"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status"`
	Risk       int    `json:"risk,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// settleRunStreamingMessage 结算一次执行残留的流式占位消息。
//
// 进程被杀 / 重启之后，这一轮的正文与工具轨迹只剩在 run_step 里，占位消息还是
// streaming=true、正文为空。直接删掉它，用户「切页回来」或点「继续」之后就只能
// 看到答案、看不到中断前做过什么——这就是「历史记录没了」。所以先把已经发生的
// 工具调用还原成 parts 写回去：有轨迹就留一条 interrupted 消息（正文可以为空，
// 工具卡还在），连轨迹都没有的空占位才删掉，那种留在列表里只是一条空白回答。
func settleRunStreamingMessage(ctx context.Context, tx pgx.Tx, runID, sessionID string) error {
	parts, err := toolPartsFromSteps(ctx, tx, runID)
	if err != nil {
		return err
	}

	if len(parts) > 0 {
		data, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		const qfin = `
update message
set parts = $2::jsonb, streaming = false, interrupted = true
where session_id = $1::uuid and streaming`
		_, err = tx.Exec(ctx, qfin, sessionID, data)
		return err
	}

	// 没有可还原的轨迹：空占位删掉，有正文的仍标中断（正文保留）
	const qdel = `
delete from message
where session_id = $1::uuid and streaming and coalesce(content, '') = ''`
	if _, err := tx.Exec(ctx, qdel, sessionID); err != nil {
		return err
	}
	const qfin = `
update message
set streaming = false, interrupted = true
where session_id = $1::uuid and streaming`
	_, err = tx.Exec(ctx, qfin, sessionID)
	return err
}

// toolPartsFromSteps 用 run_step 还原中断前的工具调用轨迹。
//
// 只取 tool.call / tool.result：正文与思考没有逐 token 落库（§15.12.2 明确不做），
// 能还原的只有「调过哪些工具、拿到什么回执」。没有回执的调用标成失败并写明原因——
// 停在「执行中」会让人以为这一轮还在跑，而这已经是终态了。
func toolPartsFromSteps(ctx context.Context, tx pgx.Tx, runID string) ([]messagePart, error) {
	if runID == "" {
		return nil, nil
	}
	const q = `
select payload from run_step
where run_id = $1::uuid and type in ('tool.call', 'tool.result')
order by seq asc`
	rows, err := tx.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type toolPayload struct {
		CallID     string `json:"call_id"`
		Tool       string `json:"tool"`
		Args       string `json:"args"`
		Summary    string `json:"summary"`
		Status     string `json:"status"`
		Risk       int    `json:"risk"`
		DurationMS int64  `json:"duration_ms"`
	}

	parts := make([]messagePart, 0, 8)
	index := make(map[string]int, 8)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p toolPayload
		if err := json.Unmarshal(raw, &p); err != nil || p.CallID == "" {
			continue
		}
		// 同一个调用先来 call、再来 result：后者补结果，不新增一条
		if i, ok := index[p.CallID]; ok {
			if p.Summary != "" {
				parts[i].Result = p.Summary
			}
			if p.Status != "" {
				parts[i].Status = p.Status
			}
			if p.DurationMS > 0 {
				parts[i].DurationMS = p.DurationMS
			}
			continue
		}
		index[p.CallID] = len(parts)
		parts = append(parts, messagePart{
			Type: "tool", ID: p.CallID, Name: p.Tool, Args: p.Args,
			Result: p.Summary, Status: p.Status, Risk: p.Risk, DurationMS: p.DurationMS,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range parts {
		if parts[i].Status == "" || parts[i].Status == "running" {
			parts[i].Status = "error"
			if parts[i].Result == "" {
				parts[i].Result = "执行被中断，未拿到回执"
			}
		}
	}
	return parts, nil
}
