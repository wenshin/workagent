package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// UpsertUserByIdentity 以 (provider, provider_uid) 为唯一键写入用户与身份映射。
//
// 资料字段「有值才覆盖」：通讯录权限临时缺失时不会把已存的工号/邮箱清空。
func (d *DB) UpsertUserByIdentity(ctx context.Context, p store.IdentityProfile) (store.User, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return store.User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	raw := p.Raw
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}

	var userID string
	err = tx.QueryRow(ctx, `
select user_id::text
from user_identity
where provider = $1 and provider_uid = $2`, p.Provider, p.ProviderUID).Scan(&userID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = tx.QueryRow(ctx, `
insert into app_user (dingtalk_union_id, name, avatar, mobile, email, job_number, corp_name)
values (nullif($1, ''), $2, nullif($3, ''), nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''))
returning id::text`,
			legacyUnionID(p), p.Name, p.Avatar, p.Mobile, p.Email, p.JobNumber, p.CorpName).Scan(&userID); err != nil {
			return store.User{}, err
		}
		if _, err = tx.Exec(ctx, `
insert into user_identity (provider, provider_uid, user_id, union_id, open_id, employee_id, job_number, email, title, contact_error, raw)
values ($1, $2, $3::uuid, nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''), $11::jsonb)
on conflict (provider, provider_uid) do update
set user_id       = excluded.user_id,
    union_id      = coalesce(excluded.union_id, user_identity.union_id),
    open_id       = coalesce(excluded.open_id, user_identity.open_id),
    employee_id   = coalesce(excluded.employee_id, user_identity.employee_id),
    job_number    = coalesce(excluded.job_number, user_identity.job_number),
    email         = coalesce(excluded.email, user_identity.email),
    title         = coalesce(excluded.title, user_identity.title),
    contact_error = excluded.contact_error,
    raw           = excluded.raw,
    updated_at    = now()`,
			p.Provider, p.ProviderUID, userID, p.UnionID, p.OpenID, p.EmployeeID,
			p.JobNumber, p.Email, p.Title, p.ContactError, raw); err != nil {
			return store.User{}, err
		}
	case err != nil:
		return store.User{}, err
	default:
		if _, err = tx.Exec(ctx, `
update app_user
set name       = coalesce(nullif($2, ''), name),
    avatar     = coalesce(nullif($3, ''), avatar),
    mobile     = coalesce(nullif($4, ''), mobile),
    email      = coalesce(nullif($5, ''), email),
    job_number = coalesce(nullif($6, ''), job_number),
    corp_name  = coalesce(nullif($7, ''), corp_name),
    updated_at = now()
where id = $1::uuid`, userID, p.Name, p.Avatar, p.Mobile, p.Email, p.JobNumber, p.CorpName); err != nil {
			return store.User{}, err
		}
		if _, err = tx.Exec(ctx, `
update user_identity
set union_id      = coalesce(nullif($3, ''), union_id),
    open_id       = coalesce(nullif($4, ''), open_id),
    employee_id   = coalesce(nullif($5, ''), employee_id),
    job_number    = coalesce(nullif($6, ''), job_number),
    email         = coalesce(nullif($7, ''), email),
    title         = coalesce(nullif($8, ''), title),
    contact_error = nullif($9, ''),
    raw           = $10::jsonb,
    updated_at    = now()
where provider = $1 and provider_uid = $2`,
			p.Provider, p.ProviderUID, p.UnionID, p.OpenID, p.EmployeeID,
			p.JobNumber, p.Email, p.Title, p.ContactError, raw); err != nil {
			return store.User{}, err
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return store.User{}, err
	}
	return d.getUserByID(ctx, userID)
}

// GetUser 按 ID 查询用户，并带出身份来源。
func (d *DB) GetUser(ctx context.Context, id string) (store.User, error) {
	return d.getUserByID(ctx, id)
}

// FindUserByJobNumber 按工号查用户：系统管理员名单来自配置，需要用工号反查用户。
func (d *DB) FindUserByJobNumber(ctx context.Context, jobNumber string) (store.User, error) {
	const q = `
select id::text
from app_user
where job_number = $1 and status = 'active'
order by created_at
limit 1`

	var id string
	err := d.pool.QueryRow(ctx, q, jobNumber).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	return d.getUserByID(ctx, id)
}

// FindUserByProviderUID 按 (provider, provider_uid) 反查用户。
//
// 与登录时写入 user_identity 的方向相反：IM 回调只带渠道自己的 id，
// 需要一个「渠道 id → 平台用户」的查找。不筛 status 以外的东西——停用用户在
// 这里返回 ErrNotFound，投递侧据此明确回「你不是平台用户」，而不是静默丢弃。
func (d *DB) FindUserByProviderUID(ctx context.Context, provider, providerUID string) (store.User, error) {
	const q = `
select i.user_id::text
from user_identity i
join app_user u on u.id = i.user_id
where i.provider = $1 and i.provider_uid = $2 and u.status = 'active'
limit 1`

	var id string
	err := d.pool.QueryRow(ctx, q, provider, providerUID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	return d.getUserByID(ctx, id)
}

// FindUserIDByStaff 按渠道员工 id（钉钉 senderStaffId）反查平台用户。
//
// 匹配 employee_id 而不是 provider_uid：后者是 unionId（登录唯一键），
// IM 消息里带的是通讯录 userid，两个值不同，拿它查必然一条也查不到。
// 同一个 staffId 理论上唯一，limit 1 只为防脏数据。
func (d *DB) FindUserIDByStaff(ctx context.Context, provider, staffID string) (string, error) {
	const q = `
select i.user_id::text
from user_identity i
join app_user u on u.id = i.user_id
where i.provider = $1 and i.employee_id = $2 and u.status = 'active'
limit 1`

	var id string
	err := d.pool.QueryRow(ctx, q, provider, staffID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (d *DB) getUserByID(ctx context.Context, id string) (store.User, error) {
	const q = `
select u.id::text, coalesce(i.provider, ''), coalesce(i.provider_uid, ''),
       u.name, coalesce(u.avatar, ''), coalesce(u.mobile, ''),
       coalesce(u.email, ''), coalesce(u.job_number, ''), coalesce(u.corp_name, ''),
       coalesce(i.contact_error, ''), u.status, u.created_at
from app_user u
left join lateral (
    select provider, provider_uid, contact_error
    from user_identity
    where user_id = u.id
    order by created_at
    limit 1
) i on true
where u.id::text = $1`

	var u store.User
	err := d.pool.QueryRow(ctx, q, id).Scan(
		&u.ID, &u.Provider, &u.ProviderUID, &u.Name, &u.Avatar, &u.Mobile,
		&u.Email, &u.JobNumber, &u.CorpName, &u.ProfileWarning, &u.Status, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}

// legacyUnionID 仅用于兼容历史的 app_user.dingtalk_union_id 列。
func legacyUnionID(p store.IdentityProfile) string {
	if p.Provider == "dingtalk" {
		return p.ProviderUID
	}
	return ""
}
