-- 会话级技能：会话可以绑定一个技能，每轮对话把它的 SKILL.md 注入系统提示词。
-- 路径相对空间 files/，与 files/.meta.json 索引中的 path 一致；空串表示未绑定。
alter table session add column if not exists skill_path text not null default '';
