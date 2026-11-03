-- Tool 三层模型（技术方案 §5.6.1）：Capability → Tool → ToolBinding
-- 以及 MCP 注册（§5.3.1）、审计（§8.2）、工具调用记录（§6 tool_call）
--
-- 说明：MVP 单租户，未引入 tenant_id；资源边界用 space_id 表达，
-- 与既有 space_member 权限模型对齐。

create table if not exists capability (
    id            uuid primary key default gen_random_uuid(),
    name          text not null unique,
    description   text not null default '',
    input_schema  jsonb,
    output_schema jsonb,
    risk_level    int  not null default 1 check (risk_level between 1 and 4),
    idempotent    boolean not null default true,
    side_effects  text not null default '',
    created_at    timestamptz not null default now()
);

create table if not exists tool (
    id            uuid primary key default gen_random_uuid(),
    capability_id uuid not null references capability (id) on delete cascade,
    name          text not null unique,
    runtime       text not null default 'builtin' check (runtime in ('builtin', 'mcp', 'http')),
    risk_level    int  not null default 1 check (risk_level between 1 and 4),
    status        text not null default 'published',
    version       int  not null default 1,
    mcp_server_id uuid,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);

create index if not exists tool_capability_idx on tool (capability_id);

-- 绑定：把工具限定在某个空间 / 用户，并附带约束（脱敏等级、行数上限、是否需审批）
create table if not exists tool_binding (
    id          uuid primary key default gen_random_uuid(),
    tool_id     uuid not null references tool (id) on delete cascade,
    space_id    uuid references space (id) on delete cascade,
    user_id     uuid references app_user (id) on delete cascade,
    constraints jsonb not null default '{}'::jsonb,
    status      text not null default 'active',
    created_at  timestamptz not null default now()
);

create index if not exists tool_binding_space_idx on tool_binding (space_id);
create index if not exists tool_binding_user_idx on tool_binding (user_id);

-- MCP Server 注册与工具清单缓存（§5.3.1 / §5.4）
create table if not exists mcp_server (
    id           uuid primary key default gen_random_uuid(),
    name         text not null unique,
    transport    text not null default 'streamable_http'
                 check (transport in ('streamable_http', 'sse')),
    endpoint     text not null,
    auth_header  text not null default '',
    secret_ref   text not null default '',
    visibility   text not null default 'tenant',
    health       text not null default 'unknown',
    tools_cache  jsonb,
    last_sync_at timestamptz,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now()
);

create table if not exists audit_log (
    id         bigserial primary key,
    actor      uuid,
    action     text not null,
    object     text not null default '',
    result     text not null default 'allow',
    detail     jsonb,
    trace_id   text,
    created_at timestamptz not null default now()
);

create index if not exists audit_log_created_idx on audit_log (created_at desc);

create table if not exists tool_call (
    id          uuid primary key default gen_random_uuid(),
    session_id  uuid references session (id) on delete set null,
    run_id      uuid,
    tool_name   text not null,
    args        jsonb,
    result_ref  text,
    status      text not null default 'ok',
    latency_ms  int  not null default 0,
    risk_level  int  not null default 1,
    created_at  timestamptz not null default now()
);

create index if not exists tool_call_session_idx on tool_call (session_id, created_at desc);
