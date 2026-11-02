package postgres

import (
	"context"
	"errors"

	"github.com/autohome/auto-agent/internal/store"
	"github.com/jackc/pgx/v5"
)

// EnsurePersonalSpace 幂等地保证用户拥有个人空间与「会话」子空间。
func (d *DB) EnsurePersonalSpace(ctx context.Context, userID string) ([]store.Space, bool, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 同一用户的并发登录串行化，避免重复创建
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return nil, false, err
	}

	var personalID string
	err = tx.QueryRow(ctx, `
select id::text
from space
where owner_id = $1::uuid and type = 'personal' and deleted_at is null
limit 1`, userID).Scan(&personalID)

	created := false
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		personalPath := "/personal/" + userID
		if err = tx.QueryRow(ctx, `
insert into space (owner_id, name, type, path, depth)
values ($1::uuid, $2, 'personal', $3, 0)
returning id::text`, userID, "个人空间", personalPath).Scan(&personalID); err != nil {
			return nil, false, err
		}
		if _, err = tx.Exec(ctx, `
insert into space (owner_id, name, type, parent_id, path, depth, is_system_reserved)
values ($1::uuid, $2, 'personal', $3::uuid, $4, 1, true)`,
			userID, "会话", personalID, personalPath+"/session"); err != nil {
			return nil, false, err
		}
		created = true
	case err != nil:
		return nil, false, err
	}

	// 所有者始终持有 manage（幂等）
	if _, err = tx.Exec(ctx, `
insert into space_member (space_id, user_id, perm, source)
select s.id, $1::uuid, 'manage', 'system'
from space s
where s.owner_id = $1::uuid and s.type = 'personal' and s.deleted_at is null
on conflict (space_id, user_id) do nothing`, userID); err != nil {
		return nil, false, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}

	spaces, err := d.ListSpaces(ctx, userID)
	return spaces, created, err
}

// ListSpaces 返回用户可见的空间，个人空间置顶、按深度排序。
func (d *DB) ListSpaces(ctx context.Context, userID string) ([]store.Space, error) {
	const q = `
select s.id::text, s.owner_id::text, s.name, s.type, coalesce(s.parent_id::text, ''), s.path, s.depth,
       s.is_system_reserved, s.status, m.perm, s.created_at
from space s
join space_member m on m.space_id = s.id and m.user_id = $1::uuid
where s.deleted_at is null
  and s.status = 'active'
  and (m.expire_at is null or m.expire_at > now())
order by (s.type = 'personal') desc, s.depth asc, s.created_at asc`

	rows, err := d.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Space
	for rows.Next() {
		var s store.Space
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Type, &s.ParentID, &s.Path, &s.Depth,
			&s.IsSystemReserved, &s.Status, &s.Perm, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
