-- Run 与 checkpoint（技术方案 §5.7.5 / §6 数据模型）
--
-- 一次 run 对应会话里的一轮执行：模型调用、工具调用、审批挂起都记在 run_step，
-- 进程重启或审批回调后可依据 run_step 续跑。
-- 说明：MVP 单租户，未引入 tenant_id / RLS；主键沿用既有 gen_random_uuid()
-- （PG16 无 uuidv7()，后续升级 PG18 或改用应用侧生成时可平滑替换）。

create table if not exists run (
    id         uuid primary key default gen_random_uuid(),
    session_id uuid not null references session (id) on delete cascade,
    -- running 执行中 / waiting_approval 等待审批 / succeeded / failed / cancelled
    status     text not null default 'running'
               check (status in ('running', 'waiting_approval', 'succeeded', 'failed', 'cancelled')),
    budget     jsonb not null default '{}'::jsonb,
    usage      jsonb not null default '{}'::jsonb,
    provider   text  not null default '',
    model      text  not null default '',
    error      text  not null default '',
    started_at timestamptz not null default now(),
    ended_at   timestamptz
);

create index if not exists run_session_idx on run (session_id, started_at desc);

-- checkpoint：每步一条，seq 递增；type 取 run.started / tool.call / tool.result /
-- approval.req / message 等，payload 保存该步的完整事件载荷。
create table if not exists run_step (
    id         bigserial primary key,
    run_id     uuid not null references run (id) on delete cascade,
    seq        int  not null,
    type       text not null,
    payload    jsonb not null default '{}'::jsonb,
    created_at timestamptz not null default now(),
    unique (run_id, seq)
);

create index if not exists run_step_run_idx on run_step (run_id, seq);
