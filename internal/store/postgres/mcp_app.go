package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// GetMCPApp 按应用名取归属记录。
func (d *DB) GetMCPApp(ctx context.Context, app string) (store.MCPApp, error) {
	const q = `
select app, owner_user_id::text, coalesce(owner_space_id::text, ''), created_at
from mcp_app where app = $1`

	var out store.MCPApp
	err := d.pool.QueryRow(ctx, q, app).Scan(
		&out.App, &out.OwnerUserID, &out.OwnerSpaceID, &out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MCPApp{}, store.ErrNotFound
	}
	return out, err
}

// ClaimMCPApp 幂等抢占应用名：靠主键冲突决出唯一创建者，不覆盖已有归属。
//
// 这里必须用「插入成功即我建的」而不是「先查再写」：两个人同时生成同名应用时，
// 后写的那一个不能把创建者改掉——密钥只有一把，归属也只能有一个。
func (d *DB) ClaimMCPApp(ctx context.Context, app, userID, spaceID string) (store.MCPApp, bool, error) {
	const q = `
insert into mcp_app (app, owner_user_id, owner_space_id)
values ($1, $2::uuid, nullif($3, '')::uuid)
on conflict (app) do nothing
returning app, owner_user_id::text, coalesce(owner_space_id::text, ''), created_at`

	var out store.MCPApp
	err := d.pool.QueryRow(ctx, q, app, userID, spaceID).Scan(
		&out.App, &out.OwnerUserID, &out.OwnerSpaceID, &out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// 已经有人占着：返回现有归属，由调用方判断是不是同一个人
		cur, gerr := d.GetMCPApp(ctx, app)
		return cur, false, gerr
	}
	if err != nil {
		return store.MCPApp{}, false, err
	}
	return out, true, nil
}
