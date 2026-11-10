package space

import _ "embed"

// 搜索源子手册用嵌入文件：正文里有大量 markdown 反引号，写成 Go 原始字符串会互相打架。
//
//go:embed plugin_docs/source.md
var pluginDevSourceDoc string

// 插件开发技能的子手册。总纲在 system.go 的 pluginDevSkill()，
// 这里按插件类型拆分，避免总纲膨胀成一份几百行的规范合集。
//
// 注意：这些是 markdown 原文，代码围栏用 ~~~ 以示与 Go 字符串边界区分。

const pluginDevSkillDoc = `# 技能插件（kind=skill）

技能是「模型照着做的方法」，没有执行器，正文只在需要时被读取。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "skill",
  "slug": "weekly_report",
  "name": "周报",
  "description": "写周报时使用：按团队模板组织本周进展、风险与下周计划",
  "capability": "skill.use",
  "skill": { "entry": "SKILL.md" }
}
~~~

## 正文（SKILL.md）
- 第一段写「什么时候用 / 什么时候不用」，模型靠这句话决定是否检索与加载。
- 再写输入、步骤、产出格式、示例；不要写成界面点击指南。
- 需要模板或长表格时放独立文件（如 templates/weekly.md），正文里给相对路径，模型用 resource_read 读。

## 生效方式
- 启用后进能力索引，模型通过 resource_search 找到；
- 用户可以在会话里绑定它（PUT /sessions/{id}/skill），正文每轮注入；
- 单次注入上限 4000 tokens，超出需分段读取。

## 边界
- 技能不能调用工具：要执行动作请配套 tool / mcp 插件，并在智能体的 requires 里引用。
- 不要把密钥、内网地址写进正文：技能正文会进模型上下文与检索结果。`

const pluginDevToolDoc = `# 工具插件（kind=tool）

工具跑在 Deno 沙箱里，没有网络权限，文件与密钥都只能通过宿主 ABI 申请。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "order_lookup",
  "name": "订单查询",
  "description": "按订单号查询状态与金额，客服核对订单时使用",
  "capability": "http.request",
  "runtime": { "entry": "index.ts", "timeout_ms": 8000 },
  "input_schema": {
    "type": "object",
    "properties": { "order_no": { "type": "string", "minLength": 6 } },
    "required": ["order_no"],
    "additionalProperties": false
  },
  "capabilities": [
    { "method": "http.request", "hosts": ["api.example.com"], "verbs": ["GET"] }
  ],
  "limits": { "max_rpc": 10, "max_result_bytes": 4096 }
}
~~~

## 入口
~~~ts
import { host } from "./host_sdk.ts";

export default async function (input: { order_no: string }) {
  const res = await host.call("http.request", {
    url: "https://api.example.com/orders/" + input.order_no,
    method: "GET",
    headers: { Authorization: "secret:erp_token" },
  });
  if (res.status !== 200) throw new Error("订单服务返回 " + res.status);
  return { order_no: input.order_no, status: res.body.status };
}
~~~

- 入口必须 export default 一个函数，参数是本轮入参对象，返回值即工具结果。
- 宿主 ABI 由平台在启动时写进运行时目录，直接 import { host } from "./host_sdk.ts"；
  不要自己实现协议，也不要读 stdin。
- host.call(method, args) 只允许清单 capabilities 里声明过的方法。

## 能力与风险
- http.request 必须写 hosts（只写域名，不带协议与端口）与 verbs；只有 GET / HEAD 算只读。
- fs.read / fs.list / fs.write 的 paths 是空间内相对路径前缀。
- 密钥只在 host.call 的 header 或 url 里写 secret:名称，宿主出网前替换；脚本永远拿不到明文。
- 风险由能力推导：只读 HTTP = L2，写 = L3；secret.use 加外部写 = L4。

## 测试
- 空间插件面板的「试运行」会真实执行一次，报错带行号与原因。
- 超时、输出超限、异常都会作为失败返回，不会静默成功。
- 平台内置工具（kind=tool 且 builtin=true）没有脚本入口，只做全局安装与展示，不要照着写。`

const pluginDevMCPDoc = `# MCP 插件（kind=mcp）

把一个外部 MCP Server 接进来，远端工具经平台代理执行。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM",
  "description": "客户与商机查询、跟进记录写入",
  "capability": "http.request",
  "transport": { "type": "http", "url": "https://mcp.example.com/mcp" },
  "auth": { "in": "header", "name": "Authorization", "secret": "crm_token" },
  "tools": { "allow": ["customer_*", "lead_create"], "deny": ["customer_delete"] }
}
~~~

## 规则
- transport.url 只允许 https（本机 http 放行）。
- tools.allow 必填且非空：默认不信任远端全部工具；支持尾部 * 前缀匹配，deny 优先。
- MCP 默认按 L3 处理（远端工具可能写数据）；同时用到密钥时按 L4 走系统管理员。
- 安装后服务地址、鉴权 Header 名、凭证名可按空间覆盖（空间插件面板的「配置」），发布快照不动。
- 远端暂不可用不会让空间失去其他工具：连接失败只影响这个 MCP。`

const pluginDevComponentDoc = `# 组件插件（kind=component）

会话里渲染的界面：声明式 DSL，不执行代码。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "component",
  "slug": "sales_board",
  "name": "销售看板",
  "description": "展示本月销售明细，需要看销售数据时使用",
  "capability": "ui.render",
  "ui": { "entry": "ui.json" },
  "data_sources": [
    { "id": "orders", "plugin": "mysql", "tool": "sql_query", "mode": "read", "params": { "limit": 20 } }
  ]
}
~~~

## 要点
- data_sources[].plugin 必须是当前空间已安装的插件；数据请求由宿主转成工具调用，仍走策略与审计。
- 节点、字段、绑定、动作的白名单见技能 builtin_ui；写错会校验失败并指出位置。
- 组件不能发网络请求、不能读文件、不能执行脚本。
- 会话里用围栏引用（版本可省略，默认用已安装版本）：
  ~~~ui-block
  {"slug":"sales_board","props":{}}
  ~~~`

const pluginDevAgentDoc = `# 智能体插件（kind=agent）

智能体是「行为设定」：提示词 + 父智能体 + 依赖插件。它不是权限，
可见工具仍由空间安装与策略决定。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "agent",
  "slug": "sales_assistant",
  "name": "销售助理",
  "description": "面向销售的智能体：查客户、写跟进、按模板出周报",
  "capability": "agent.use",
  "agent": {
    "entry": "AGENT.md",
    "extends": "service_base",
    "requires": ["builtin_plugin_dev", "crm_mcp"]
  }
}
~~~

## 提示词（AGENT.md）
- 只写「这个人怎么工作」：目标、步骤、口径、禁止事项。
- 平台基础规则始终在最前面，智能体只能追加，不能覆盖或删除。
- 追加正文上限 64KB；写清输出格式比写口号有用。

## 继承
- extends 指向另一个 agent 插件的 slug：父提示词在前、自己在后拼接，最多 4 层，禁止成环。
- 父智能体必须已发布；父的 requires 不会自动继承，依赖要写在自己的 requires 里。
- 留空表示只追加到平台基础规则。

## 依赖
- requires 里的插件会在安装智能体时一并安装到同一空间；已全局可用的内置件自动跳过。
- 依赖只接受已发布的 slug；依赖装不上时整个安装失败，不会留下半个智能体。
- 智能体没有执行器，清单里不允许写 capabilities，也不允许写 endpoint、密钥、命令。

## 使用
- 安装到系统空间即为全局可用：所有空间的会话都能在新会话时选到它。
- 安装到普通空间只在该空间可选。
- 会话创建时把提示词快照固化在会话上，事后改智能体不影响已开会话。`
