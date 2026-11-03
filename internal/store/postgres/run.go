package postgres

import (
	"context"

	"github.com/workagent/auto-agent/internal/store"
)

// CreateRun 开启一次执行，返回数据库生成的 id 与开始时间（§6 run）。
func (d *DB) CreateRun(ctx context.Context, r store.Run) (store.Run, error) {
	const q = `
insert into run (session_id, status, budget)
values ($1::uuid, $2, $3::jsonb)
returning id::text, status, started_at`

	status := r.Status
	if status == "" {
		status = store.RunRunning
	}
	budget := r.Budget
	if len(budget) == 0 {
		budget = []byte(`{}`)
	}

	var out store.Run
	if err := d.pool.QueryRow(ctx, q, r.SessionID, status, budget).Scan(
		&out.ID, &out.Status, &out.StartedAt,
	); err != nil {
		return store.Run{}, err
	}
	out.SessionID = r.SessionID
	out.Budget = budget
	return out, nil
}

// AppendRunStep 追加一条 checkpoint；同 (run_id, seq) 重复写入视为错误，避免重放乱序。
func (d *DB) AppendRunStep(ctx context.Context, s store.RunStep) (store.RunStep, error) {
	const q = `
insert into run_step (run_id, seq, type, payload)
values ($1::uuid, $2, $3, $4::jsonb)
returning id::text, created_at`

	payload := s.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	var out store.RunStep
	if err := d.pool.QueryRow(ctx, q, s.RunID, s.Seq, s.Type, payload).Scan(
		&out.ID, &out.CreatedAt,
	); err != nil {
		return store.RunStep{}, err
	}
	out.RunID = s.RunID
	out.Seq = s.Seq
	out.Type = s.Type
	out.Payload = payload
	return out, nil
}

// FinishRun 落最终状态、用量与错误信息（§5.7.5 checkpoint 的收尾）。
func (d *DB) FinishRun(ctx context.Context, r store.Run) error {
	const q = `
update run
set status   = $2,
    usage    = $3::jsonb,
    provider = $4,
    model    = $5,
    error    = $6,
    ended_at = now()
where id = $1::uuid`

	usage := r.Usage
	if len(usage) == 0 {
		usage = []byte(`{}`)
	}
	_, err := d.pool.Exec(ctx, q, r.ID, r.Status, usage, r.Provider, r.Model, r.Error)
	return err
}

// ListRunSteps 按 seq 返回一次执行的全部 checkpoint，恢复时据此重放。
func (d *DB) ListRunSteps(ctx context.Context, runID string) ([]store.RunStep, error) {
	const q = `
select id::text, run_id::text, seq, type, payload, created_at
from run_step
where run_id = $1::uuid
order by seq`

	rows, err := d.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.RunStep, 0, 8)
	for rows.Next() {
		var s store.RunStep
		if err := rows.Scan(&s.ID, &s.RunID, &s.Seq, &s.Type, &s.Payload, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
