-- 插件体系（docs/插件体系设计.md）
--
-- 内容以文件为准、状态以 DB 为准，两边不互相镜像：
--   plugin         已发布资产（草稿只存在于空间文件，不落库）
--   plugin_version 不可变发布快照
--   plugin_install 空间级安装记录（安装即授权）

create table if not exists plugin (
    id             uuid primary key default gen_random_uuid(),
    slug           text not null unique,
    kind           text not null,
    name           text not null,
    description    text not null default '',
    visibility     text not null default 'public',
    status         text not null default 'enabled',
    latest_version text not null default '',
    owner_space_id uuid,
    extensible     boolean not null default false,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now()
);

create table if not exists plugin_version (
    id            uuid primary key default gen_random_uuid(),
    plugin_id     uuid not null references plugin(id) on delete cascade,
    version       text not null,
    manifest      jsonb not null,
    artifact_path text not null,
    sha256        text not null,
    capabilities  jsonb not null default '[]'::jsonb,
    risk          int not null default 1,
    notes         text not null default '',
    published_by  uuid,
    published_at  timestamptz not null default now(),
    unique (plugin_id, version)
);

create table if not exists plugin_install (
    id                uuid primary key default gen_random_uuid(),
    space_id          uuid not null,
    plugin_id         uuid not null references plugin(id) on delete cascade,
    version           text not null,
    status            text not null default 'enabled',
    risk              int not null default 1,
    installed_by      uuid,
    installed_at      timestamptz not null default now(),
    dismissed_version text not null default '',
    unique (space_id, plugin_id)
);

create index if not exists idx_plugin_install_space on plugin_install (space_id, status);

-- 默认安装集只对空间执行一次：用户卸载后不再追溯安装。
alter table space add column if not exists plugin_bootstrapped_at timestamptz;
