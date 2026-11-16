-- 人工确认记录（§5.2）。一次命中审批规则的调用一行，从 pending 走到终态。
--
-- 为什么落库而不是只放内存：
--   1. 会话里刷新 / 换标签页后，未处理的确认还在（按 session 查 pending 即可补卡片）；
--   2. 「本轮已同意」在进程重启后仍成立，不会因为重启把同一 run 的动作再问一遍；
--   3. 拒绝与超时同样是事实，审计要能答「这次为什么没执行」。
--
-- run 结束会把该 run 的 pending 全部置 expired：没有人再等，留着只会变成幽灵待办。
-- 授权范围仍然只到 run（scope='turn'），不跨轮；表里记 run_id 就是为了这条边界。

create table if not exists approval (
    id          text primary key,
    session_id  uuid not null references session(id) on delete cascade,
    run_id      uuid not null references run (id) on delete cascade,
    call_id     text not null default '',
    tool        text not null,
    kind        text not null,             -- net.write | shell
    action_key  text not null,             -- 记忆键：net / shell:rm
    detail      text not null default '',
    reason      text not null default '',
    risk        int  not null default 0,
    status      text not null default 'pending',  -- pending | approved | denied | expired
    scope       text not null default 'once',     -- once | turn
    decided_by  uuid references app_user (id),
    decided_at  timestamptz,
    created_at  timestamptz not null default now(),
    expire_at   timestamptz not null
);

-- 补卡片：一个会话里所有还没处理的确认
create index if not exists approval_session_pending_idx
    on approval (session_id, created_at) where status = 'pending';

-- 本轮记忆：同一 run 内同一动作只看已决的那条
create index if not exists approval_run_key_idx
    on approval (run_id, action_key) where status in ('approved', 'denied');
