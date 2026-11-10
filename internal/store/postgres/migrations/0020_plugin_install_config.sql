-- 已安装插件的空间级配置（连接地址、鉴权头、凭证名）。
--
-- 配置放在安装记录上而不是 plugin 上：同一个插件装到不同空间可能连不同的
-- 服务/用不同的凭证；发布快照保持不可变，不被空间配置污染。
-- 只存非敏感值：凭证本身仍在凭证存储里，这里只存它的名字。
alter table plugin_install add column if not exists config jsonb not null default '{}'::jsonb;
