-- 助手消息的用量与耗时：产生这条回答的那一轮 run 的 token 消耗与总墙钟耗时。
-- 前端在消息卡片上展示「token 消耗 + 总耗时」；0 表示未知（该列之前的历史消息）。

alter table message
    add column if not exists in_tokens   int not null default 0,
    add column if not exists out_tokens  int not null default 0,
    add column if not exists duration_ms int not null default 0;
