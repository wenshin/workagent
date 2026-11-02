-- 会话直接归属空间：取消系统预置的「会话」子空间，改为每个空间预置一个「新会话」。
-- 会话的第一条用户消息会改写标题（title_auto = true 时），因此新增标记位。

begin;

-- 1. 标题是否允许由首条消息自动生成
alter table session add column if not exists title_auto boolean not null default true;

-- 2. 原「会话」子空间下的会话上移到父空间
update session s
set space_id = p.parent_id
from space p
where s.space_id = p.id
  and p.is_system_reserved
  and p.parent_id is not null;

-- 3. 软删系统预置的「会话」子空间（历史数据保留，避免破坏外键）
update space
set deleted_at = now(), status = 'deleted', updated_at = now()
where is_system_reserved
  and deleted_at is null;

-- 4. 每个活跃空间补一个「新会话」，作为进入空间后的默认对话入口
insert into session (space_id, title, created_by, title_auto)
select s.id, '新会话', s.owner_id, true
from space s
where s.deleted_at is null
  and s.status = 'active'
  and not exists (
      select 1
      from session x
      where x.space_id = s.id
        and x.status = 'active'
  );

-- 5. 消息表：一个会话内的问答流水
create table if not exists message (
    id          uuid primary key default gen_random_uuid(),
    session_id  uuid not null references session (id) on delete cascade,
    role        text not null check (role in ('user', 'assistant', 'system')),
    content     text not null,
    created_at  timestamptz not null default now()
);

create index if not exists message_session_idx on message (session_id, created_at);

commit;
