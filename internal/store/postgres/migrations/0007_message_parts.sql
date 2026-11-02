-- 助手消息需要保存思考过程与工具调用轨迹，历史消息才能原样还原。
-- parts 为 JSON 数组：[{type:"tool", name, args, result, status, duration_ms}, ...]
-- reasoning 单独存一列，便于前端折叠展示与后续检索。

begin;

alter table message add column if not exists reasoning text not null default '';
alter table message add column if not exists parts jsonb;

commit;
