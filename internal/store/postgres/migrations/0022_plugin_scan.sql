-- 插件发布扫描（docs/插件安全基线.md §1.5）
--
-- 扫描结果是元数据：不改发布件，规则升级后可以重扫，结论可变、产物不变。
-- 唯一键带上 rules_version，所以「同一版本被不同规则扫过」天然留痕。

create table if not exists plugin_scan (
    id              uuid primary key default gen_random_uuid(),
    plugin_id       uuid not null references plugin(id) on delete cascade,
    version         text not null,
    artifact_sha256 text not null default '',
    rules_version   text not null default '',
    scanner         text not null default 'builtin',
    status          text not null,
    max_severity    text not null default '',
    deps            int not null default 0,
    lockfile        text not null default '',
    findings        jsonb not null default '[]'::jsonb,
    blocked_reason  text not null default '',
    dep_error       text not null default '',
    scanned_by      uuid,
    scanned_at      timestamptz not null default now(),
    unique (plugin_id, version, rules_version)
);

create index if not exists idx_plugin_scan_plugin on plugin_scan (plugin_id, scanned_at desc);
