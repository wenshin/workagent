-- 用户与空间基础模型（MVP）
-- 注意：物理表名用 app_user，避免与 SQL 保留字 user 冲突

create table if not exists app_user (
    id                 uuid primary key default gen_random_uuid(),
    dingtalk_union_id  text not null unique,
    name               text not null default '',
    avatar             text,
    mobile             text,
    status             text not null default 'active',
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now()
);

create table if not exists space (
    id                  uuid primary key default gen_random_uuid(),
    owner_id            uuid not null references app_user (id) on delete cascade,
    name                text not null,
    type                text not null check (type in ('personal', 'shared')),
    parent_id           uuid references space (id) on delete cascade,
    path                text not null,
    depth               int not null default 0,
    is_system_reserved  boolean not null default false,
    status              text not null default 'active',
    deleted_at          timestamptz,
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now()
);

-- 同一路径只允许一条未删除记录，作为并发初始化的兜底
create unique index if not exists space_path_uniq on space (path) where deleted_at is null;
create index if not exists space_owner_idx on space (owner_id);

create table if not exists space_member (
    space_id    uuid not null references space (id) on delete cascade,
    user_id     uuid not null references app_user (id) on delete cascade,
    perm        text not null check (perm in ('view', 'read', 'write', 'manage')),
    source      text not null default 'manual' check (source in ('manual', 'approved', 'system')),
    expire_at   timestamptz,
    granted_by  uuid references app_user (id),
    created_at  timestamptz not null default now(),
    primary key (space_id, user_id)
);

create table if not exists auth_session (
    token_hash    text primary key,
    user_id       uuid not null references app_user (id) on delete cascade,
    created_at    timestamptz not null default now(),
    last_seen_at  timestamptz not null default now(),
    expires_at    timestamptz not null
);

create index if not exists auth_session_user_idx on auth_session (user_id);
create index if not exists auth_session_expires_idx on auth_session (expires_at);
