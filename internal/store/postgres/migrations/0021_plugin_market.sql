-- 插件市场：来源、待发布候选、空间本地插件启用状态。
--
-- 发布权在系统管理员，安装权在空间管理员：
--   plugin.origin 区分 组织内 / 平台内置 / 外部；
--   plugin_candidate 记录「空间已启用、尚未发布」的候选，市场只展示元数据；
--   plugin_install 仍是空间级安装记录（安装只在本空间生效）。

alter table plugin add column if not exists origin text not null default 'internal';

-- 历史内置件的 owner 是系统空间（早期种子可能为空），统一标记为 system。
update plugin set origin = 'system'
where owner_space_id is null
   or owner_space_id in (select id from space where type = 'system');

create table if not exists plugin_candidate (
    id            uuid primary key default gen_random_uuid(),
    space_id      uuid not null references space(id) on delete cascade,
    slug          text not null,
    kind          text not null,
    name          text not null default '',
    description   text not null default '',
    author_id     uuid,
    author_name   text not null default '',
    manifest_hash text not null default '',
    risk          int not null default 1,
    capabilities  jsonb not null default '[]'::jsonb,
    status        text not null default 'disabled', -- enabled / disabled
    plugin_id     uuid references plugin(id) on delete set null,
    enabled_at    timestamptz,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),
    unique (space_id, slug)
);

create index if not exists idx_plugin_candidate_space on plugin_candidate (space_id, status);
create index if not exists idx_plugin_candidate_status on plugin_candidate (status);
