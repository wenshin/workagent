package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/workagent/auto-agent/internal/store"
)

const approvalColumns = `
id, session_id::text, run_id::text, call_id, tool, kind, action_key, args_hash,
detail, reason, risk, risk_label, status, scope, coalesce(decided_by::text, ''),
decided_at, created_at, expire_at`

// CreateApproval 登记一条待确认记录；同 id 已存在时复用（重放幂等）。
func (d *DB) CreateApproval(ctx context.Context, a store.Approval) (store.Approval, error) {
	const q = `
insert into approval (id, session_id, run_id, call_id, tool, kind, action_key, args_hash,
                      detail, reason, risk, risk_label, status, scope, expire_at)
values ($1, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
on conflict (id) do nothing`

	status := a.Status
	if status == "" {
		status = store.ApprovalPending
	}
	scope := a.Scope
	if scope == "" {
		scope = "once"
	}
	if _, err := d.pool.Exec(ctx, q,
		a.ID, a.SessionID, a.RunID, a.CallID, a.Tool, a.Kind, a.ActionKey, a.ArgsHash,
		a.Detail, a.Reason, a.Risk, a.RiskLabel, status, scope, a.ExpireAt,
	); err != nil {
		return store.Approval{}, err
	}
	out, ok, err := d.GetApproval(ctx, a.ID)
	if err != nil {
		return store.Approval{}, err
	}
	if !ok {
		return store.Approval{}, errors.New("审批记录写入后读不到")
	}
	return out, nil
}

// DecideApproval 把 pending 改成终态；已不在 pending 时 ok=false。
func (d *DB) DecideApproval(ctx context.Context, id, status, scope, decidedBy string) (store.Approval, bool, error) {
	const q = `
update approval
set status     = $2,
    scope      = coalesce(nullif($3, ''), scope),
    decided_by = nullif($4, '')::uuid,
    decided_at = now()
where id = $1 and status = 'pending'
returning ` + approvalColumns

	row, err := d.scanApproval(d.pool.QueryRow(ctx, q, id, status, scope, decidedBy))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Approval{}, false, nil
		}
		return store.Approval{}, false, err
	}
	return row, true, nil
}

// GetApproval 按 id 读一条确认记录。
func (d *DB) GetApproval(ctx context.Context, id string) (store.Approval, bool, error) {
	const q = `select ` + approvalColumns + ` from approval where id = $1`

	a, err := d.scanApproval(d.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Approval{}, false, nil
		}
		return store.Approval{}, false, err
	}
	return a, true, nil
}

// FindApprovalGrant 返回同一 run 内可以直接复用的结论。
//
// 两种命中：
//   - scope='turn' + action_key：本轮都同意 / 都拒绝；
//   - args_hash 相同：同一次调用的重放。续跑时模型重新生成的 tool_call id 是新的，
//     但参数一字不差，这是唯一能把「重放」与「用户真的又要跑一遍」分开的判据。
func (d *DB) FindApprovalGrant(ctx context.Context, runID, actionKey, argsHash string) (store.Approval, bool, error) {
	const q = `
select ` + approvalColumns + `
from approval
where run_id = $1::uuid
  and status in ('approved', 'denied')
  and (
        (scope = 'turn' and action_key = $2)
     or (coalesce(nullif($3, ''), '') <> '' and args_hash = $3)
  )
order by created_at desc
limit 1`

	a, err := d.scanApproval(d.pool.QueryRow(ctx, q, runID, actionKey, argsHash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Approval{}, false, nil
		}
		return store.Approval{}, false, err
	}
	return a, true, nil
}

// ListPendingApprovals 返回会话里所有还没失效的确认。
//
// 判据是「pending 且没过期」，不看 run 状态：进程崩过之后 run 已经被收尾，
// 但那条待办还在有效期内——它仍然可以被批准，批准会顺带把这一轮续起来。
func (d *DB) ListPendingApprovals(ctx context.Context, sessionID string) ([]store.Approval, error) {
	const q = `
select ` + approvalColumns + `
from approval
where session_id = $1::uuid
  and status = 'pending'
  and expire_at > now()
order by created_at`

	rows, err := d.pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Approval, 0, 4)
	for rows.Next() {
		a, err := d.scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkApprovalExpired 把一条已决记录收回 expired：决策没能送达执行体，
// 保持 approved 会让人误以为「批了但没执行」是平台的问题。
func (d *DB) MarkApprovalExpired(ctx context.Context, id string) error {
	const q = `
update approval set status = 'expired'
where id = $1 and status in ('approved', 'denied')`
	_, err := d.pool.Exec(ctx, q, id)
	return err
}

// ExpireRunApprovals 把一次执行留下的 pending 全部置 expired。
func (d *DB) ExpireRunApprovals(ctx context.Context, runID string) error {
	const q = `
update approval
set status = 'expired'
where run_id = $1::uuid and status = 'pending'`
	_, err := d.pool.Exec(ctx, q, runID)
	return err
}

// scanApproval 同时适配 QueryRow 与 Rows：两者都满足 distiller/row 的共同接口。
func (d *DB) scanApproval(row interface {
	Scan(dest ...any) error
}) (store.Approval, error) {
	var a store.Approval
	err := row.Scan(
		&a.ID, &a.SessionID, &a.RunID, &a.CallID, &a.Tool, &a.Kind, &a.ActionKey, &a.ArgsHash,
		&a.Detail, &a.Reason, &a.Risk, &a.RiskLabel, &a.Status, &a.Scope, &a.DecidedBy,
		&a.DecidedAt, &a.CreatedAt, &a.ExpireAt,
	)
	return a, err
}
