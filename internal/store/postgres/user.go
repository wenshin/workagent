package postgres

import (
	"context"
	"errors"

	"github.com/autohome/auto-agent/internal/store"
	"github.com/jackc/pgx/v5"
)

// UpsertUser 以 union_id 为唯一键写入用户，返回最新资料。
func (d *DB) UpsertUser(ctx context.Context, p store.DingTalkProfile) (store.User, error) {
	const q = `
insert into app_user (dingtalk_union_id, name, avatar, mobile)
values ($1, $2, nullif($3, ''), nullif($4, ''))
on conflict (dingtalk_union_id) do update
set name       = excluded.name,
    avatar     = coalesce(excluded.avatar, app_user.avatar),
    mobile     = coalesce(excluded.mobile, app_user.mobile),
    updated_at = now()
returning id::text, dingtalk_union_id, name, coalesce(avatar, ''), coalesce(mobile, ''), status, created_at`

	var u store.User
	err := d.pool.QueryRow(ctx, q, p.UnionID, p.Name, p.Avatar, p.Mobile).Scan(
		&u.ID, &u.DingTalkUnionID, &u.Name, &u.Avatar, &u.Mobile, &u.Status, &u.CreatedAt,
	)
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}

// GetUser 按 ID 查询用户。
func (d *DB) GetUser(ctx context.Context, id string) (store.User, error) {
	const q = `
select id::text, dingtalk_union_id, name, coalesce(avatar, ''), coalesce(mobile, ''), status, created_at
from app_user
where id::text = $1`

	var u store.User
	err := d.pool.QueryRow(ctx, q, id).Scan(
		&u.ID, &u.DingTalkUnionID, &u.Name, &u.Avatar, &u.Mobile, &u.Status, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}
