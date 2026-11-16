-- 工具绑定（技术方案旧 §5.6.1 第三层 ToolBinding）整体下线。
--
-- 它原本要表达「这个工具能访问哪段资源、用哪份凭证、授给哪些角色」，但资源模型
-- 已在 0024_drop_resource.sql 下线，凭证走插件清单的 secret.use，授权口径统一为
-- 「插件安装/启用」：工具能出现在列表里就说明所属插件已装好（MCP 是已注册），
-- 不需要第二处授权记录。表里剩下的 constraints（max_rows / approval_required）
-- 也没有任何执行体在读，留着只会让人以为改绑定能限数据范围。
drop table if exists tool_binding;
