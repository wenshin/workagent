-- MCP 应用名归属（docs/技术方案.md §15.11）。
--
-- 一个应用名只有一把 JWT 签名密钥，密钥在凭证存储里按应用名寻址；
-- 没有归属记录时「取回自己的密钥」和「拿别人的密钥」分不开，
-- 所以应用名要在第一次生成密钥时绑到创建者，之后只有他能取回明文。
create table if not exists mcp_app (
    app            text primary key,
    owner_user_id  uuid not null,
    owner_space_id uuid,
    created_at     timestamptz not null default now()
);
