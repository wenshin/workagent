package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// CreateSpace 创建空间，并把创建者写成 manage 成员。
func (d *DB) CreateSpace(ctx context.Context, in store.SpaceInput) (store.Space, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return store.Space{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `
insert into space (id, owner_id, name, type, parent_id, path, depth, is_system_reserved)
values ($1::uuid, $2::uuid, $3, $4, nullif($5, '')::uuid, $6, $7, $8)
returning id::text, owner_id::text, name, type, coalesce(parent_id::text, ''),
          path, depth, is_system_reserved, status, created_at`

	var s store.Space
	if err := tx.QueryRow(ctx, insert,
		in.ID, in.OwnerID, in.Name, in.Type, in.ParentID, in.Path, in.Depth, in.IsSystemReserved,
	).Scan(&s.ID, &s.OwnerID, &s.Name, &s.Type, &s.ParentID,
		&s.Path, &s.Depth, &s.IsSystemReserved, &s.Status, &s.CreatedAt); err != nil {
		return store.Space{}, err
	}

	// 每个空间预置一个「新会话」，作为进入该空间后的默认对话入口
	if _, err := tx.Exec(ctx, `
insert into session (space_id, title, created_by)
values ($1::uuid, '新会话', $2::uuid)`, s.ID, in.OwnerID); err != nil {
		return store.Space{}, err
	}

	// 创建者对新空间为 manage
	if _, err := tx.Exec(ctx, `
insert into space_member (space_id, user_id, perm, source)
values ($1::uuid, $2::uuid, 'manage', 'system')
on conflict (space_id, user_id) do nothing`, s.ID, in.OwnerID); err != nil {
		return store.Space{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.Space{}, err
	}
	s.Perm = "manage"
	return s, nil
}

// GetSpace 按 ID 查询未删除的空间。
func (d *DB) GetSpace(ctx context.Context, id string) (store.Space, error) {
	const q = `
select id::text, owner_id::text, name, type, coalesce(parent_id::text, ''),
       path, depth, is_system_reserved, status, created_at
from space
where id::text = $1 and deleted_at is null`

	var s store.Space
	err := d.pool.QueryRow(ctx, q, id).Scan(&s.ID, &s.OwnerID, &s.Name, &s.Type, &s.ParentID,
		&s.Path, &s.Depth, &s.IsSystemReserved, &s.Status, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Space{}, store.ErrNotFound
	}
	if err != nil {
		return store.Space{}, err
	}
	return s, nil
}

// GetSpaceByType 按类型取第一个未删除空间；系统空间全局唯一。
func (d *DB) GetSpaceByType(ctx context.Context, spaceType string) (store.Space, error) {
	const q = `
select id::text, owner_id::text, name, type, coalesce(parent_id::text, ''),
       path, depth, is_system_reserved, status, created_at
from space
where type = $1 and deleted_at is null and status = 'active'
order by created_at
limit 1`

	var s store.Space
	err := d.pool.QueryRow(ctx, q, spaceType).Scan(&s.ID, &s.OwnerID, &s.Name, &s.Type, &s.ParentID,
		&s.Path, &s.Depth, &s.IsSystemReserved, &s.Status, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Space{}, store.ErrNotFound
	}
	if err != nil {
		return store.Space{}, err
	}
	return s, nil
}

// EnsureSpaceMember 幂等添加成员；已存在时不降级已有权限。
func (d *DB) EnsureSpaceMember(ctx context.Context, spaceID, userID, perm string) error {
	_, err := d.pool.Exec(ctx, `
insert into space_member (space_id, user_id, perm, source)
values ($1::uuid, $2::uuid, $3, 'system')
on conflict (space_id, user_id) do nothing`, spaceID, userID, perm)
	return err
}

// SpacePerm 返回用户在该空间的权限，无权限返回空字符串。
func (d *DB) SpacePerm(ctx context.Context, spaceID, userID string) (string, error) {
	const q = `
select perm
from space_member
where space_id::text = $1
  and user_id::text = $2
  and (expire_at is null or expire_at > now())
limit 1`

	var perm string
	err := d.pool.QueryRow(ctx, q, spaceID, userID).Scan(&perm)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return perm, nil
}

// RenameSpace 修改空间名称，路径与存储目录不随之变化。
func (d *DB) RenameSpace(ctx context.Context, id, name string) error {
	tag, err := d.pool.Exec(ctx, `
update space set name = $2, updated_at = now()
where id::text = $1 and deleted_at is null`, id, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteSpaceTree 软删整棵子树（递归 CTE），返回被删除的空间。
func (d *DB) DeleteSpaceTree(ctx context.Context, id string) ([]store.Space, error) {
	const q = `
with recursive tree as (
    select id, path
    from space
    where id::text = $1 and deleted_at is null
    union all
    select s.id, s.path
    from space s
    join tree t on s.parent_id = t.id
    where s.deleted_at is null
)
update space
set deleted_at = now(), status = 'deleted', updated_at = now()
where id in (select id from tree)
returning id::text, path`

	rows, err := d.pool.Query(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Space
	for rows.Next() {
		var s store.Space
		if err := rows.Scan(&s.ID, &s.Path); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
	return out, nil
}
