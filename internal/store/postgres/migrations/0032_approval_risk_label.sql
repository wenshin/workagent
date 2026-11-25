-- 确认卡上的动作标签（网络请求 / 写文件 / 脚本执行…）。
--
-- 为什么要落库而不是只在事件里带：refresh 之后未处理的确认是从这张表补出来的，
-- 只带在 SSE 里的话，同一张卡刷新前写「脚本执行」、刷新后写回「写入」——
-- 用户会以为是自己记错了。
--
-- 与 risk 的关系：risk 是四档等级（决定徽标颜色与审批口径），risk_label 只是
-- 更具体的说法（决定徽标文案）。空串表示没有更具体的说法，前端退回等级文案。
alter table approval add column if not exists risk_label text not null default '';
