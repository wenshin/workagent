-- 资源发布：发布产物独立于工作空间存储，按版本快照保存在 {STORAGE_ROOT}/published/ 下。
-- 只有「用户范围」（public / private），没有「加载范围」：是否使用由模型按需检索决定。

create table if not exists resource (
    id              uuid primary key default gen_random_uuid(),
    kind            text not null check (kind in ('skill', 'tool', 'mcp', 'component', 'db')),
    name            text not null,
    description     text not null default '',
    -- public：全部可见；private：仅发布者可见
    visibility      text not null default 'private' check (visibility in ('public', 'private')),
    -- enabled：可被检索与使用；disabled：保留数据但不可用
    status          text not null default 'enabled' check (status in ('enabled', 'disabled')),
    owner_id        uuid not null references app_user(id),
    -- 发布来源：空间草稿所在的 space，用于追溯
    source_space_id uuid references space(id),
    latest_version  text not null default '',
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    -- 同一发布者下 kind + name 唯一：再次发布同名资源视为新增版本
    unique (kind, name, owner_id)
);

create index if not exists resource_visible_idx on resource (visibility, status, updated_at desc);

create table if not exists resource_version (
    id           uuid primary key default gen_random_uuid(),
    resource_id  uuid not null references resource(id) on delete cascade,
    version      text not null,
    notes        text not null default '',
    -- 相对存储根的发布目录：published/{kind}/{name}/{version}
    storage_path text not null,
    file_count   int not null default 0,
    total_bytes  bigint not null default 0,
    created_by   uuid not null references app_user(id),
    created_at   timestamptz not null default now(),
    unique (resource_id, version)
);
