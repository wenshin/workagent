-- 统一身份接入（技术方案 §5.1.1 / §6）：IdP 身份映射与资料补全

-- 展示资料：邮箱与工号来自企业通讯录（钉钉 job_number / email）
alter table app_user add column if not exists email text;
alter table app_user add column if not exists job_number text;

-- 历史列仅作兼容保留；身份唯一键迁移到 user_identity(provider, provider_uid)
alter table app_user alter column dingtalk_union_id drop not null;

create table if not exists user_identity (
    provider     text not null,
    provider_uid text not null,
    user_id      uuid not null references app_user (id) on delete cascade,
    union_id     text,
    open_id      text,
    employee_id  text,
    job_number   text,
    email        text,
    title        text,
    raw          jsonb not null default '{}'::jsonb,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    primary key (provider, provider_uid)
);

create index if not exists user_identity_user_idx on user_identity (user_id);

-- 回填：历史用户都是钉钉身份
insert into user_identity (provider, provider_uid, user_id, union_id)
select 'dingtalk', dingtalk_union_id, id, dingtalk_union_id
from app_user
where dingtalk_union_id is not null
on conflict (provider, provider_uid) do nothing;
