-- 恢复机制的补齐（§5.7.5）：让「等着人点」也成为一种可恢复的 checkpoint。
--
-- 三个缺口：
--   1. 审批等待挂在进程内存里，进程一死这一轮就没了；
--   2. 决策打到别的实例时，只有落库没有送达，等待方永远等不到；
--   3. 续跑时模型会重新生成调用，已执行过的副作用会被再跑一遍。
--
-- 依次对应：approval.args_hash（按动作 + 参数复用结论）、
-- run.status 的 waiting_approval（等人的 run 不参与清扫）、
-- run_resume.updated_at 的 TTL（断点不能永久堆积）。

-- 一次批准只该对「同一次调用」生效。续跑时模型重新生成的 tool_call id 是新的，
-- 唯一靠得住的同一性判据是「工具 + 规范化参数」。
alter table approval add column if not exists args_hash text not null default '';

-- 恢复时按 (run_id, args_hash) 找已决结论
create index if not exists approval_run_args_idx
    on approval (run_id, args_hash) where args_hash <> '';

-- 断点按更新时间清理：老断点接上去，模型面对的是一份早就过期的世界
create index if not exists run_resume_updated_idx on run_resume (updated_at);

-- 工具结果是恢复的燃料：续跑时按「工具 + 参数」认出已经做过的那次调用，
-- 直接把结果回放给模型，而不是再执行一遍（副作用不能重来）。
-- 只在结果真的产生时写，失败/拒绝也写——它们同样是「已经发生过」的事实。
alter table tool_call add column if not exists result text;

create index if not exists tool_call_run_idx on tool_call (run_id) where run_id is not null;
