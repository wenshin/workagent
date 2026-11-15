-- 资源发布并入插件体系（docs/插件体系设计.md）：发布件只由 plugin / plugin_version /
-- plugin_install 表达，resource 两表不再读写，直接下线。
drop table if exists resource_version;
drop table if exists resource;

-- 内置工具改名：resource_search / resource_read 改为 plugin_search / plugin_read，
-- resource_list（实际列的是空间）改为 space_list。旧记录对应的实现已不存在，
-- 留着会让工具清单里出现两个同义条目；删 capability 会级联清掉 tool 与 tool_binding。
delete from capability where name in ('system.source.search', 'system.source.read', 'resource.list');
