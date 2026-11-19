-- 登录态改由 Redis 独存（§5.1.1）：会话不再落库，Postgres 只保留用户资料。
--
-- 删表而不是留一张空表：留着就会被后来的人当成「真相」，
-- 于是又出现「库里有一套、Redis 有一套」的两份会话来源。
drop table if exists auth_session;
