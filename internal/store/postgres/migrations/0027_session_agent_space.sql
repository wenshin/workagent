-- agent_space_id：会话绑定的智能体来自哪个空间。
--
-- agent_id 只存 slug，同名 slug 在两个空间可以并存；派发出来的子会话要用发起方
-- 空间的智能体，必须把来源空间一起记下来，否则运行时会按子会话空间解析到另一个
-- 同名插件（或者干脆找不到）。
--
-- 为空表示沿用会话自己的 space_id，历史数据不需要回填。
alter table session
    add column if not exists agent_space_id uuid references space (id) on delete set null;
