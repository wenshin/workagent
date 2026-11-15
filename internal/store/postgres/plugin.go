package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// UpsertPlugin 写入或更新插件元数据（按 slug 唯一）。
//
// name / description 在这里是「最近一次发布的快照」，不是第二处可编辑定义：
// 唯一入口始终是发布件里的 plugin.json。
func (d *DB) UpsertPlugin(ctx context.Context, p store.Plugin) (store.Plugin, error) {
	const q = `
insert into plugin (slug, kind, name, description, visibility, status, latest_version, owner_space_id, origin, extensible)
values ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::uuid, $9, $10)
on conflict (slug) do update set
    kind           = excluded.kind,
    name           = excluded.name,
    description    = excluded.description,
    visibility     = excluded.visibility,
    status         = excluded.status,
    latest_version = excluded.latest_version,
    origin         = excluded.origin,
    extensible     = excluded.extensible,
    updated_at     = now()
returning id::text, slug, kind, name, description, visibility, status,
          latest_version, coalesce(owner_space_id::text, ''), origin, extensible, created_at, updated_at`

	var out store.Plugin
	err := d.pool.QueryRow(ctx, q,
		p.Slug, p.Kind, p.Name, p.Description, p.Visibility, p.Status,
		p.LatestVersion, p.OwnerSpaceID, p.Origin, p.Extensible,
	).Scan(
		&out.ID, &out.Slug, &out.Kind, &out.Name, &out.Description,
		&out.Visibility, &out.Status, &out.LatestVersion,
		&out.OwnerSpaceID, &out.Origin, &out.Extensible, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

// GetPluginBySlug 按 slug 取插件。
func (d *DB) GetPluginBySlug(ctx context.Context, slug string) (store.Plugin, error) {
	const q = `
select id::text, slug, kind, name, description, visibility, status,
       latest_version, coalesce(owner_space_id::text, ''), origin, extensible, created_at, updated_at
from plugin where slug = $1`

	var p store.Plugin
	err := d.pool.QueryRow(ctx, q, slug).Scan(
		&p.ID, &p.Slug, &p.Kind, &p.Name, &p.Description, &p.Visibility,
		&p.Status, &p.LatestVersion, &p.OwnerSpaceID, &p.Origin, &p.Extensible,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Plugin{}, store.ErrNotFound
	}
	return p, err
}

// ListPlugins 返回全部已发布插件（插件市场视图）。
func (d *DB) ListPlugins(ctx context.Context) ([]store.Plugin, error) {
	const q = `
select id::text, slug, kind, name, description, visibility, status,
       latest_version, coalesce(owner_space_id::text, ''), origin, extensible, created_at, updated_at
from plugin order by kind, name`

	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Plugin, 0, 16)
	for rows.Next() {
		var p store.Plugin
		if err := rows.Scan(
			&p.ID, &p.Slug, &p.Kind, &p.Name, &p.Description, &p.Visibility,
			&p.Status, &p.LatestVersion, &p.OwnerSpaceID, &p.Origin, &p.Extensible,
			&p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPluginVersion 在事务里追加版本并移动 latest_version。
func (d *DB) AddPluginVersion(ctx context.Context, v store.PluginVersion, latest string) (store.PluginVersion, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return store.PluginVersion{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `
insert into plugin_version (plugin_id, version, manifest, artifact_path, sha256, capabilities, risk, notes, published_by)
values ($1::uuid, $2, $3::jsonb, $4, $5, $6::jsonb, $7, $8, nullif($9, '')::uuid)
returning plugin_id::text, version, manifest, artifact_path, sha256, capabilities, risk, notes,
          coalesce(published_by::text, ''), published_at`

	caps := v.Capabilities
	if len(caps) == 0 {
		caps = []byte(`[]`)
	}
	var out store.PluginVersion
	if err := tx.QueryRow(ctx, insert,
		v.PluginID, v.Version, v.Manifest, v.ArtifactPath, v.SHA256, caps, v.Risk, v.Notes, v.PublishedBy,
	).Scan(
		&out.PluginID, &out.Version, &out.Manifest, &out.ArtifactPath, &out.SHA256,
		&out.Capabilities, &out.Risk, &out.Notes, &out.PublishedBy, &out.PublishedAt,
	); err != nil {
		return store.PluginVersion{}, err
	}
	if _, err := tx.Exec(ctx,
		`update plugin set latest_version = $2, updated_at = now() where id = $1::uuid`,
		v.PluginID, latest); err != nil {
		return store.PluginVersion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.PluginVersion{}, err
	}
	return out, nil
}

// GetPluginVersion 取指定版本；version 为空时取 latest_version。
func (d *DB) GetPluginVersion(ctx context.Context, pluginID, version string) (store.PluginVersion, error) {
	const q = `
select v.plugin_id::text, v.version, v.manifest, v.artifact_path, v.sha256, v.capabilities,
       v.risk, v.notes, coalesce(v.published_by::text, ''), v.published_at
from plugin_version v
join plugin p on p.id = v.plugin_id
where v.plugin_id = $1::uuid
  and v.version = coalesce(nullif($2, ''), p.latest_version)`

	var v store.PluginVersion
	err := d.pool.QueryRow(ctx, q, pluginID, version).Scan(
		&v.PluginID, &v.Version, &v.Manifest, &v.ArtifactPath, &v.SHA256,
		&v.Capabilities, &v.Risk, &v.Notes, &v.PublishedBy, &v.PublishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PluginVersion{}, store.ErrNotFound
	}
	return v, err
}

// ListPluginVersions 返回全部版本，按发布时间倒序。
func (d *DB) ListPluginVersions(ctx context.Context, pluginID string) ([]store.PluginVersion, error) {
	const q = `
select plugin_id::text, version, manifest, artifact_path, sha256, capabilities,
       risk, notes, coalesce(published_by::text, ''), published_at
from plugin_version where plugin_id = $1::uuid order by published_at desc`

	rows, err := d.pool.Query(ctx, q, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.PluginVersion, 0, 8)
	for rows.Next() {
		var v store.PluginVersion
		if err := rows.Scan(
			&v.PluginID, &v.Version, &v.Manifest, &v.ArtifactPath, &v.SHA256,
			&v.Capabilities, &v.Risk, &v.Notes, &v.PublishedBy, &v.PublishedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpsertPluginInstall 安装/升级：同一空间同一插件只有一条记录。
func (d *DB) UpsertPluginInstall(ctx context.Context, in store.PluginInstall) (store.PluginInstall, error) {
	const q = `
insert into plugin_install (space_id, plugin_id, version, status, risk, installed_by, dismissed_version)
values ($1::uuid, $2::uuid, $3, $4, $5, nullif($6, '')::uuid, $7)
on conflict (space_id, plugin_id) do update set
    version  = excluded.version,
    status   = excluded.status,
    risk     = excluded.risk,
    installed_by = excluded.installed_by,
    installed_at = now()
returning space_id::text, plugin_id::text, version, status, risk,
          coalesce(installed_by::text, ''), installed_at, dismissed_version, config`

	var out store.PluginInstall
	if err := d.pool.QueryRow(ctx, q,
		in.SpaceID, in.PluginID, in.Version, in.Status, in.Risk, in.InstalledBy, in.DismissedVersion,
	).Scan(
		&out.SpaceID, &out.PluginID, &out.Version, &out.Status, &out.Risk,
		&out.InstalledBy, &out.InstalledAt, &out.DismissedVersion, &out.Config,
	); err != nil {
		return store.PluginInstall{}, err
	}
	return out, nil
}

// ListPluginInstalls 返回空间内全部安装记录（含已停用）。
func (d *DB) ListPluginInstalls(ctx context.Context, spaceID string) ([]store.PluginInstall, error) {
	const q = `
select i.space_id::text, i.plugin_id::text, p.slug, i.version, i.status, i.risk,
       coalesce(i.installed_by::text, ''), i.installed_at, i.dismissed_version, i.config
from plugin_install i
join plugin p on p.id = i.plugin_id
where i.space_id = $1::uuid
order by p.slug`

	rows, err := d.pool.Query(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.PluginInstall, 0, 8)
	for rows.Next() {
		var in store.PluginInstall
		if err := rows.Scan(
			&in.SpaceID, &in.PluginID, &in.Slug, &in.Version, &in.Status,
			&in.Risk, &in.InstalledBy, &in.InstalledAt, &in.DismissedVersion, &in.Config,
		); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ListAllPluginInstalls 返回全部空间的安装记录：凭证是平台级的，
// 判断「还有没有插件在用」不能只看某一个空间。
func (d *DB) ListAllPluginInstalls(ctx context.Context) ([]store.PluginInstall, error) {
	const q = `
select i.space_id::text, i.plugin_id::text, p.slug, i.version, i.status, i.risk,
       coalesce(i.installed_by::text, ''), i.installed_at, i.dismissed_version, i.config
from plugin_install i
join plugin p on p.id = i.plugin_id
order by p.slug, i.space_id`

	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.PluginInstall, 0, 16)
	for rows.Next() {
		var in store.PluginInstall
		if err := rows.Scan(
			&in.SpaceID, &in.PluginID, &in.Slug, &in.Version, &in.Status,
			&in.Risk, &in.InstalledBy, &in.InstalledAt, &in.DismissedVersion, &in.Config,
		); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// GetPluginInstall 按「空间 + slug」取安装记录。
func (d *DB) GetPluginInstall(ctx context.Context, spaceID, slug string) (store.PluginInstall, error) {
	const q = `
select i.space_id::text, i.plugin_id::text, p.slug, i.version, i.status, i.risk,
       coalesce(i.installed_by::text, ''), i.installed_at, i.dismissed_version, i.config
from plugin_install i
join plugin p on p.id = i.plugin_id
where i.space_id = $1::uuid and p.slug = $2`

	var in store.PluginInstall
	err := d.pool.QueryRow(ctx, q, spaceID, slug).Scan(
		&in.SpaceID, &in.PluginID, &in.Slug, &in.Version, &in.Status,
		&in.Risk, &in.InstalledBy, &in.InstalledAt, &in.DismissedVersion, &in.Config,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PluginInstall{}, store.ErrNotFound
	}
	return in, err
}

// UpdatePluginInstallConfig 覆盖安装级配置；版本、状态与授权不变。
func (d *DB) UpdatePluginInstallConfig(ctx context.Context, spaceID, pluginID string, cfg json.RawMessage) error {
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	tag, err := d.pool.Exec(ctx,
		`update plugin_install set config = $3::jsonb where space_id = $1::uuid and plugin_id = $2::uuid`,
		spaceID, pluginID, cfg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeletePluginInstall 删除安装记录（审计不受影响）。
func (d *DB) DeletePluginInstall(ctx context.Context, spaceID, pluginID string) error {
	_, err := d.pool.Exec(ctx,
		`delete from plugin_install where space_id = $1::uuid and plugin_id = $2::uuid`,
		spaceID, pluginID)
	return err
}

// DeletePlugin 删除插件记录；plugin_version、plugin_install、plugin_scan 都是级联删除。
//
// 只给「代码里已经下线的内置件」用：用户自己发布的插件不能这么消失，
// 那条路是卸载安装而不是删记录（审计要留着）。
func (d *DB) DeletePlugin(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `delete from plugin where id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListAllSpaces 返回全部未删除空间（启动期迁移与默认安装集用）。
func (d *DB) ListAllSpaces(ctx context.Context) ([]store.Space, error) {
	const q = `
select id::text, coalesce(owner_id::text, ''), name, type, coalesce(parent_id::text, ''),
       path, depth, is_system_reserved, status, created_at
from space where deleted_at is null order by depth, path`

	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Space, 0, 16)
	for rows.Next() {
		var s store.Space
		if err := rows.Scan(
			&s.ID, &s.OwnerID, &s.Name, &s.Type, &s.ParentID,
			&s.Path, &s.Depth, &s.IsSystemReserved, &s.Status, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkPluginBootstrapped 原子地标记空间已完成默认安装集初始化。
// 返回 true 表示本次抢到标记（应当执行安装），false 表示已经装过。
func (d *DB) MarkPluginBootstrapped(ctx context.Context, spaceID string) (bool, error) {
	tag, err := d.pool.Exec(ctx,
		`update space set plugin_bootstrapped_at = now()
		 where id = $1::uuid and plugin_bootstrapped_at is null`, spaceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CountSessionsUsingSkill 统计绑定了某技能路径的会话，用于卸载前的引用检查。
func (d *DB) CountSessionsUsingSkill(ctx context.Context, spaceID, prefix string) (int, error) {
	const q = `
select count(*) from session
where space_id = $1::uuid and skill_path like $2 and status <> 'deleted'`

	var n int
	if err := d.pool.QueryRow(ctx, q, spaceID, prefix+"%").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------- 插件候选

// UpsertPluginCandidate 写入/更新空间本地插件的启用元数据。
// 发布关联（plugin_id）不在这里改，避免本地重新启用时把已发布关系覆盖掉。
func (d *DB) UpsertPluginCandidate(ctx context.Context, c store.PluginCandidate) (store.PluginCandidate, error) {
	caps := c.Capabilities
	if len(caps) == 0 {
		caps = []byte(`[]`)
	}
	const q = `
insert into plugin_candidate
    (space_id, slug, kind, name, description, author_id, author_name,
     manifest_hash, risk, capabilities, status, enabled_at)
values ($1::uuid, $2, $3, $4, $5, nullif($6, '')::uuid, $7, $8, $9, $10::jsonb, $11,
        case when $11 = 'enabled' then now() else null end)
on conflict (space_id, slug) do update set
    kind          = excluded.kind,
    name          = excluded.name,
    description   = excluded.description,
    author_id     = excluded.author_id,
    author_name   = excluded.author_name,
    manifest_hash = excluded.manifest_hash,
    risk          = excluded.risk,
    capabilities  = excluded.capabilities,
    status        = excluded.status,
    enabled_at    = case when excluded.status = 'enabled' then now() else null end,
    updated_at    = now()
returning id::text, space_id::text, slug, kind, name, description,
          coalesce(author_id::text, ''), author_name, manifest_hash, risk, capabilities,
          status, coalesce(plugin_id::text, ''), coalesce(enabled_at, 'epoch'::timestamptz),
          created_at, updated_at`

	var out store.PluginCandidate
	if err := d.pool.QueryRow(ctx, q,
		c.SpaceID, c.Slug, c.Kind, c.Name, c.Description, c.AuthorID, c.AuthorName,
		c.ManifestHash, c.Risk, caps, c.Status,
	).Scan(
		&out.ID, &out.SpaceID, &out.Slug, &out.Kind, &out.Name, &out.Description,
		&out.AuthorID, &out.AuthorName, &out.ManifestHash, &out.Risk, &out.Capabilities,
		&out.Status, &out.PluginID, &out.EnabledAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return store.PluginCandidate{}, err
	}
	return out, nil
}

const candidateSelect = `
select c.id::text, c.space_id::text, coalesce(s.name, ''), c.slug, c.kind, c.name, c.description,
       coalesce(c.author_id::text, ''), c.author_name, c.manifest_hash, c.risk, c.capabilities,
       c.status, coalesce(c.plugin_id::text, ''), coalesce(p.name, ''),
       coalesce(c.enabled_at, 'epoch'::timestamptz), c.created_at, c.updated_at
from plugin_candidate c
left join space s on s.id = c.space_id
left join plugin p on p.id = c.plugin_id`

func scanCandidate(row pgx.Row) (store.PluginCandidate, error) {
	var c store.PluginCandidate
	err := row.Scan(
		&c.ID, &c.SpaceID, &c.SpaceName, &c.Slug, &c.Kind, &c.Name, &c.Description,
		&c.AuthorID, &c.AuthorName, &c.ManifestHash, &c.Risk, &c.Capabilities,
		&c.Status, &c.PluginID, &c.PublishedName, &c.EnabledAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PluginCandidate{}, store.ErrNotFound
	}
	return c, err
}

// GetPluginCandidate 取某空间某 slug 的候选。
func (d *DB) GetPluginCandidate(ctx context.Context, spaceID, slug string) (store.PluginCandidate, error) {
	return scanCandidate(d.pool.QueryRow(ctx, candidateSelect+` where c.space_id = $1::uuid and c.slug = $2`, spaceID, slug))
}

// ListPluginCandidates 返回全部候选，市场按 status 过滤。
func (d *DB) ListPluginCandidates(ctx context.Context) ([]store.PluginCandidate, error) {
	rows, err := d.pool.Query(ctx, candidateSelect+` order by c.updated_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.PluginCandidate, 0, 16)
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListPluginCandidatesBySpace 返回空间内的候选，用于本地插件启用状态。
func (d *DB) ListPluginCandidatesBySpace(ctx context.Context, spaceID string) ([]store.PluginCandidate, error) {
	rows, err := d.pool.Query(ctx, candidateSelect+` where c.space_id = $1::uuid order by c.slug`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.PluginCandidate, 0, 8)
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkPluginCandidatePublished 记录候选发布到的全局插件。
func (d *DB) MarkPluginCandidatePublished(ctx context.Context, spaceID, slug, pluginID string) error {
	_, err := d.pool.Exec(ctx,
		`update plugin_candidate set plugin_id = $3::uuid, updated_at = now()
		 where space_id = $1::uuid and slug = $2`,
		spaceID, slug, pluginID)
	return err
}

// DeletePluginCandidate 删除候选（本地目录删除或装修复时）。
func (d *DB) DeletePluginCandidate(ctx context.Context, spaceID, slug string) error {
	_, err := d.pool.Exec(ctx,
		`delete from plugin_candidate where space_id = $1::uuid and slug = $2`, spaceID, slug)
	return err
}

// SavePluginScan 写入扫描结论。同一 plugin + version + rules_version 覆盖：
// 规则没换时重扫只保留最新一次，换了规则则各自留痕。
func (d *DB) SavePluginScan(ctx context.Context, s store.PluginScan) (store.PluginScan, error) {
	findings := s.Findings
	if len(findings) == 0 {
		findings = json.RawMessage("[]")
	}
	const q = `
insert into plugin_scan (plugin_id, version, artifact_sha256, rules_version, scanner,
    status, max_severity, deps, lockfile, findings, blocked_reason, dep_error, scanned_by)
values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, nullif($13, '')::uuid)
on conflict (plugin_id, version, rules_version) do update set
    artifact_sha256 = excluded.artifact_sha256,
    scanner         = excluded.scanner,
    status          = excluded.status,
    max_severity    = excluded.max_severity,
    deps            = excluded.deps,
    lockfile        = excluded.lockfile,
    findings        = excluded.findings,
    blocked_reason  = excluded.blocked_reason,
    dep_error       = excluded.dep_error,
    scanned_by      = excluded.scanned_by,
    scanned_at      = now()
returning id::text, plugin_id::text, version, artifact_sha256, rules_version, scanner,
          status, max_severity, deps, lockfile, findings, blocked_reason, dep_error,
          coalesce(scanned_by::text, ''), scanned_at`

	var out store.PluginScan
	err := d.pool.QueryRow(ctx, q,
		s.PluginID, s.Version, s.SHA256, s.RulesVersion, s.Scanner,
		s.Status, s.MaxSeverity, s.Deps, s.Lockfile, string(findings),
		s.BlockedReason, s.DepError, s.ScannedBy,
	).Scan(
		&out.ID, &out.PluginID, &out.Version, &out.SHA256, &out.RulesVersion, &out.Scanner,
		&out.Status, &out.MaxSeverity, &out.Deps, &out.Lockfile, &out.Findings,
		&out.BlockedReason, &out.DepError, &out.ScannedBy, &out.ScannedAt,
	)
	return out, err
}

// GetPluginScan 取某版本最近一次的扫描结论。
func (d *DB) GetPluginScan(ctx context.Context, pluginID, version string) (store.PluginScan, error) {
	const q = `
select id::text, plugin_id::text, version, artifact_sha256, rules_version, scanner,
       status, max_severity, deps, lockfile, findings, blocked_reason, dep_error,
       coalesce(scanned_by::text, ''), scanned_at
from plugin_scan
where plugin_id = $1::uuid and version = $2
order by scanned_at desc
limit 1`

	var s store.PluginScan
	err := d.pool.QueryRow(ctx, q, pluginID, version).Scan(
		&s.ID, &s.PluginID, &s.Version, &s.SHA256, &s.RulesVersion, &s.Scanner,
		&s.Status, &s.MaxSeverity, &s.Deps, &s.Lockfile, &s.Findings,
		&s.BlockedReason, &s.DepError, &s.ScannedBy, &s.ScannedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PluginScan{}, store.ErrNotFound
	}
	return s, err
}
