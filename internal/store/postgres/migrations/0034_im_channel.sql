-- IM 渠道绑定：一条外部消息进来要落到「哪个空间、哪个会话、以谁的身份」。
--
-- 需求是「钉钉固定绑一个会话，企微绑另一个」。把绑定写进代码就是每接一个渠道
-- 改一次发版，所以渠道本身是一张表：加渠道 = 插一行，不用动 schema。
--
-- 三个字段一起构成绑定键：
--   space_id    消息落进哪个空间（目前实现里固定是用户的个人空间）
--   channel     渠道标识（dingtalk / wecom / feishu ...），决定「绑哪个会话」
--   user_id     会话归属人；run 的 actor 就是这个人，他的工具权限也就是这次执行的权限
--
-- 一个渠道 + 一个用户只有一个会话：找不到就现建，见 GetOrCreateChannelSession。
create table if not exists im_channel (
    channel    text primary key,
    -- 展示名用于生成会话标题（「钉钉消息」「企微消息」）。空则用 channel 原文。
    label      text not null default '',
    enabled    boolean not null default true,
    created_at timestamptz not null default now()
);

-- 钉钉是平台首选身份源，装上就有；其它渠道由接入方按需插入。
insert into im_channel (channel, label) values ('dingtalk', '钉钉')
on conflict (channel) do nothing;

-- session.channel 非空表示这个会话被渠道钉死：不自动改写标题、不参与「最近会话」
-- 的常规排序，前端列表里单独标出。null 是普通会话，与历史数据完全一致。
alter table session add column if not exists channel text;

-- 同一个渠道 + 同一个用户只能有一个会话：上面的「找不到就建」在多进程/并发消息下
-- 必须有数据库这一层兜底，否则两条消息同时到会各建一个，消息就分裂到两条时间线上。
create unique index if not exists session_channel_user_uniq
    on session (channel, created_by) where channel is not null;
