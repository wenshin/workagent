package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// resourceColumns 是资源行的统一列顺序，改动时必须同步 scanResource。
const resourceColumns = `r.id::text, r.kind, r.name, r.description, r.visibility, r.status,
       r.owner_id::text, coalesce(u.name, ''), coalesce(r.source_space_id::text, ''),
       coalesce(s.name, ''), r.latest_version, r.created_at, r.updated_at`

// CreateResource 在同一事务里写入资源与首个版本。
func (d *DB) CreateResource(ctx context.Context, r store.Resource, v store.ResourceVersion) (store.Resource, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return store.Resource{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// id 允许由业务层指定：发布目录 published/{kind}/{id}/{version} 需要先拿到 id
	var id string
	if err := tx.QueryRow(ctx, `
insert into resource (id, kind, name, description, visibility, status, owner_id, source_space_id, latest_version)
values (coalesce(nullif($1, '')::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7::uuid, nullif($8, '')::uuid, $9)
returning id::text`,
		r.ID, r.Kind, r.Name, r.Description, r.Visibility, r.Status, r.OwnerID, r.SourceSpaceID, v.Version).Scan(&id); err != nil {
		return store.Resource{}, err
	}

	if _, err := tx.Exec(ctx, `
insert into resource_version (resource_id, version, notes, storage_path, file_count, total_bytes, created_by)
values ($1::uuid, $2, $3, $4, $5, $6, $7::uuid)`,
		id, v.Version, v.Notes, v.StoragePath, v.FileCount, v.TotalBytes, v.CreatedBy); err != nil {
		return store.Resource{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.Resource{}, err
	}
	return d.GetResource(ctx, id)
}

// FindResourceByName 按「发布者 + kind + name」查资源。
func (d *DB) FindResourceByName(ctx context.Context, ownerID, kind, name string) (store.Resource, error) {
	row := d.pool.QueryRow(ctx, `
select `+resourceColumns+`
from resource r
left join app_user u on u.id = r.owner_id
left join space s on s.id = r.source_space_id
where r.owner_id = $1::uuid and r.kind = $2 and r.name = $3`,
		ownerID, kind, name)
	return scanResource(row)
}

// GetResource 按 id 取资源，同时带上版本数。
func (d *DB) GetResource(ctx context.Context, id string) (store.Resource, error) {
	row := d.pool.QueryRow(ctx, `
select `+resourceColumns+`,
       (select count(*) from resource_version v where v.resource_id = r.id)
from resource r
left join app_user u on u.id = r.owner_id
left join space s on s.id = r.source_space_id
where r.id = $1::uuid`, id)
	return scanResourceWithCount(row)
}

// ListResources 返回对该用户可见的资源：全部 public + 自己的 private。
func (d *DB) ListResources(ctx context.Context, userID string) ([]store.Resource, error) {
	rows, err := d.pool.Query(ctx, `
select `+resourceColumns+`,
       (select count(*) from resource_version v where v.resource_id = r.id)
from resource r
left join app_user u on u.id = r.owner_id
left join space s on s.id = r.source_space_id
where r.visibility = 'public' or r.owner_id = $1::uuid
order by r.updated_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Resource, 0, 8)
	for rows.Next() {
		r, err := scanResourceWithCount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddResourceVersion 追加一个不可变版本，并把 latest_version 指向最高版本。
func (d *DB) AddResourceVersion(ctx context.Context, resourceID string, v store.ResourceVersion, latest string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
insert into resource_version (resource_id, version, notes, storage_path, file_count, total_bytes, created_by)
values ($1::uuid, $2, $3, $4, $5, $6, $7::uuid)`,
		resourceID, v.Version, v.Notes, v.StoragePath, v.FileCount, v.TotalBytes, v.CreatedBy); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
update resource set latest_version = $2, updated_at = now() where id = $1::uuid`, resourceID, latest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListResourceVersions 按发布时间倒序返回版本。
func (d *DB) ListResourceVersions(ctx context.Context, resourceID string) ([]store.ResourceVersion, error) {
	rows, err := d.pool.Query(ctx, `
select version, notes, storage_path, file_count, total_bytes, created_by::text, created_at
from resource_version
where resource_id = $1::uuid
order by created_at desc`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.ResourceVersion, 0, 4)
	for rows.Next() {
		var v store.ResourceVersion
		if err := rows.Scan(&v.Version, &v.Notes, &v.StoragePath, &v.FileCount, &v.TotalBytes, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetResourceVersion 取指定版本；version 为空时取 latest_version。
func (d *DB) GetResourceVersion(ctx context.Context, resourceID, version string) (store.ResourceVersion, error) {
	row := d.pool.QueryRow(ctx, `
select v.version, v.notes, v.storage_path, v.file_count, v.total_bytes, v.created_by::text, v.created_at
from resource_version v
join resource r on r.id = v.resource_id
where v.resource_id = $1::uuid
  and ($2 = '' or v.version = $2)
order by (v.version = r.latest_version) desc, v.created_at desc
limit 1`, resourceID, version)

	var v store.ResourceVersion
	err := row.Scan(&v.Version, &v.Notes, &v.StoragePath, &v.FileCount, &v.TotalBytes, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ResourceVersion{}, store.ErrNotFound
	}
	return v, err
}

// SetResourceStatus 启用 / 停用资源。
func (d *DB) SetResourceStatus(ctx context.Context, id, status string) error {
	tag, err := d.pool.Exec(ctx, `
update resource set status = $2, updated_at = now() where id = $1::uuid`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpdateResourceMeta 在再次发布时同步描述与可见性。
func (d *DB) UpdateResourceMeta(ctx context.Context, id, description, visibility string) error {
	tag, err := d.pool.Exec(ctx, `
update resource set description = $2, visibility = $3, updated_at = now() where id = $1::uuid`,
		id, description, visibility)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteResource 删除资源及其全部版本记录（文件由业务层清理）。
func (d *DB) DeleteResource(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `delete from resource where id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteResourceVersion 删除一个版本，并把 latest_version 修正为剩余的最近版本。
func (d *DB) DeleteResourceVersion(ctx context.Context, resourceID, version string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if version == "" {
		if err := tx.QueryRow(ctx,
			`select latest_version from resource where id = $1::uuid`, resourceID).Scan(&version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
	}

	tag, err := tx.Exec(ctx,
		`delete from resource_version where resource_id = $1::uuid and version = $2`, resourceID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}

	// 最新版本被删掉后回退到剩下的最近一次发布
	if _, err := tx.Exec(ctx, `
update resource set latest_version = coalesce((
    select v.version from resource_version v
    where v.resource_id = resource.id
    order by v.created_at desc limit 1
), ''), updated_at = now()
where id = $1::uuid`, resourceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type resourceScanner interface {
	Scan(dest ...any) error
}

func scanResource(row resourceScanner) (store.Resource, error) {
	var r store.Resource
	err := row.Scan(&r.ID, &r.Kind, &r.Name, &r.Description, &r.Visibility, &r.Status,
		&r.OwnerID, &r.OwnerName, &r.SourceSpaceID, &r.SourceSpaceName, &r.LatestVersion, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Resource{}, store.ErrNotFound
	}
	return r, err
}

func scanResourceWithCount(row resourceScanner) (store.Resource, error) {
	var r store.Resource
	err := row.Scan(&r.ID, &r.Kind, &r.Name, &r.Description, &r.Visibility, &r.Status,
		&r.OwnerID, &r.OwnerName, &r.SourceSpaceID, &r.SourceSpaceName, &r.LatestVersion, &r.CreatedAt, &r.UpdatedAt,
		&r.VersionCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Resource{}, store.ErrNotFound
	}
	return r, err
}
