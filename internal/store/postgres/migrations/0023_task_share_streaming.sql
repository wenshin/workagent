-- 异步任务派发、排障分享链接与流式消息（技术方案 §15.12）
--
-- 三件事放在同一个迁移里，因为它们同属「会话可见性与异步回执」这一批：
--   message.streaming   首个增量到达即占位，结束时整段覆盖（§15.12.2）
--   session_task        主 Agent 派发、子会话回执（§15.12.4）
--   session_share       用户授权的临时只读链接（§15.12.3）

alter table message add column if not exists streaming boolean not null default false;
-- interrupted 标记「这一轮启动了但被取消/中断」，与 streaming=false 组合使用
alter table message add column if not exists interrupted boolean not null default false;
-- source 区分消息来源：user 用户输入 / task_result 子任务回执 / external 外部回信
alter table message add column if not exists source text not null default 'user';

-- 未完成的消息要能在列表中一眼看出来，避免刷新后误以为没发出去
create index if not exists message_streaming_idx on message (session_id) where streaming;

create table if not exists session_task (
    id                uuid primary key default gen_random_uuid(),
    space_id          uuid not null references space (id) on delete cascade,
    parent_session_id uuid not null references session (id) on delete cascade,
    child_session_id  uuid references session (id) on delete set null,
    title             text not null,
    brief             text not null default '',
    assignee          uuid not null references app_user (id),
    -- pending 已派发待开始 / running 子会话在执行 / waiting_external 等待回执
    -- succeeded / failed / cancelled 终态；没有「过期」，过期只能由用户取消
    status            text not null default 'pending'
                      check (status in ('pending', 'running', 'waiting_external', 'succeeded', 'failed', 'cancelled')),
    waiting_since     timestamptz,
    dispatch_count    int not null default 0,
    result            text not null default '',
    created_by        uuid not null references app_user (id),
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);

create index if not exists session_task_parent_idx on session_task (parent_session_id, created_at desc);
create index if not exists session_task_assignee_idx on session_task (assignee, status);

create table if not exists session_share (
    id         uuid primary key default gen_random_uuid(),
    session_id uuid not null references session (id) on delete cascade,
    -- 只存 sha256：链接一旦生成，平台也拿不回原文，泄漏面只有链接本身
    token_hash text not null unique,
    expires_at timestamptz not null,
    revoked_at timestamptz,
    view_count int not null default 0,
    created_by uuid not null references app_user (id),
    created_at timestamptz not null default now()
);

create index if not exists session_share_session_idx on session_share (session_id, created_at desc);
