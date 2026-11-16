-- 长时间任务的两个前提：区分「还在跑」与「进程没了」，以及中断后能接着跑（§5.7.5）。
--
--   run.heartbeat_at  跑它的进程每收到一个事件就刷新一次。总时长不再是成败判据，
--                     「心跳停了」才是：启动清扫与 active-run 都按它判定死run。
--   run_resume        每轮结束覆盖一次完整消息快照。中断后从这里接着走，
--                     而不是让模型凭半截正文重新规划、把工具再执行一遍。

alter table run add column if not exists heartbeat_at timestamptz not null default now();

-- 清扫只扫「状态还在跑、心跳却停了」的那批；部分索引让这条更新不必全表扫描
create index if not exists run_running_idx on run (heartbeat_at) where status = 'running';

create table if not exists run_resume (
    -- 一次 run 一份，每轮覆盖：只有最新快照有意义，历史由 run_step 承担
    run_id     uuid primary key references run (id) on delete cascade,
    payload    jsonb not null,
    updated_at timestamptz not null default now()
);
