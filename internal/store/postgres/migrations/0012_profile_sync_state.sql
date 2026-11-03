-- 企业名称与通讯录同步状态（技术方案 §5.1.1）
-- corp_name 用于顶栏展示企业名称；contact_error 记录最近一次通讯录同步失败原因，
-- 由 /auth/me 的 profile_warning 暴露，避免权限缺失时用户只看到字段长期为空。

alter table app_user add column if not exists corp_name text;
alter table user_identity add column if not exists contact_error text;
