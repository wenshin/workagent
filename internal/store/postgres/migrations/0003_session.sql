-- 聊天会话：归属于某个空间（实际创建在空间的「会话」子空间下）
create table if not exists session (
    id               uuid primary key default gen_random_uuid(),
    space_id         uuid not null references space (id) on delete cascade,
    title            text not null default '新会话',
    agent_id         uuid,
    created_by       uuid not null references app_user (id) on delete cascade,
    last_message_at  timestamptz,
    status           text not null default 'active' check (status in ('active', 'archived')),
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now()
);

create index if not exists session_space_idx on session (space_id, created_at desc);
create index if not exists session_user_idx on session (created_by);
