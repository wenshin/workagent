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
- 需要模板或长表格时放独立文件（如 templates/weekly.md），正文里给相对路径，模型用 plugin_read 读。

## 创建方式
没有「建技能」的专用工具：技能就是插件目录，用 file_write 写两个文件即可。
1. 先写 plugins/<slug>/plugin.json，照上面的清单填；slug 自己定（小写字母开头、[a-z0-9_]、2-32 位、同空间唯一）。
2. 再写 plugins/<slug>/SKILL.md 正文，不带 frontmatter。
3. 写完让用户去空间插件面板启用或发布；试运行会校验清单，字段写错会指出位置。

界面上的「新建技能」只是这两步的快捷方式，建出来的结构完全一样；目录已存在时不要重建，直接编辑里面的文件。

## 生效方式
- 启用后进能力索引，模型通过 plugin_search 找到；
- 用户可以在会话里绑定它（PUT /sessions/{id}/skill），正文每轮注入；
- 单次注入上限 4000 tokens，超出需分段读取。

## 边界
- 技能不能调用工具：要执行动作请配套 tool / mcp 插件，并在智能体的 requires 里引用。
- 不要把密钥、内网地址写进正文：技能正文会进模型上下文与检索结果。`

const pluginDevEvalsDoc = `# 发布前自测（evals.json）

插件的「怎么用」是作者最清楚的事。把期望行为写成用例放在插件目录的 ` + "`evals.json`" + `，
**发布时会执行**：没过就发布失败（409），审计记 ` + "`plugin.publish.eval_failed`" + `。
用例跟着发布快照走，别人装到的插件带着它的用例。

## 两种用例

| kind | 跑什么 | 需要模型 |
|---|---|---|
| ` + "`tool`" + ` | 用 ` + "`input`" + ` 跑一次试运行（与「试运行」按钮同一条路径），断言输出 | 不需要 |
| ` + "`agent`" + ` | 用脚本化的模型回合驱动 Agent 循环，断言停止原因、工具序列、轮次 | 不需要（回合是脚本） |

` + "`tool`" + ` 用来证明「给定输入能拿到预期输出」；` + "`agent`" + ` 用来证明「模型按说明书能被用得对」——
比如检索到空结果时不会死循环、连续重复调用会被拦住、第 8 轮一定给出结论。

## 最小示例

~~~json
{
  "version": 1,
  "cases": [
    {
      "name": "查订单号返回状态",
      "kind": "tool",
      "input": { "order_no": "A00001" },
      "expect": { "contains": ["status"], "not_contains": ["Traceback"] }
    },
    {
      "name": "检索不到时不会反复重试",
      "kind": "agent",
      "query": "按公司规范给这个订单退款",
      "tools": [{ "name": "plugin_search", "result": "" }],
      "script": [
        { "tool_calls": [{ "name": "plugin_search", "args": { "query": "退款规范" } }] },
        { "tool_calls": [{ "name": "plugin_search", "args": { "query": "退款 流程" } }] },
        { "content": "没有找到退款规范，需要你确认是哪一份。" }
      ],
      "expect": { "stop_reason": "stalled", "content_contains": ["没有找到"] }
    }
  ]
}
~~~

## 断言字段

` + "`expect`" + ` 按 kind 取用：

- ` + "`tool`" + `：` + "`contains`" + ` / ` + "`not_contains`" + ` 子串，` + "`error_expected`" + ` 期望执行失败。
- ` + "`agent`" + `：` + "`stop_reason`" + `（` + "`done`" + ` / ` + "`budget`" + ` / ` + "`stalled`" + `）、
  ` + "`content_contains`" + `、` + "`content_not_contains`" + `、` + "`tool_sequence`" + `（实际执行的工具名顺序，被拦截的重复调用不计入）、
  ` + "`max_rounds`" + `（模型调用次数上限）、` + "`max_tool_calls`" + `、` + "`blocked_at_least`" + `（至少拦截几次重复调用）。

` + "`limits`" + ` 可以给单条用例改预算（` + "`max_rounds`" + ` / ` + "`max_tool_calls`" + ` / ` + "`stall_rounds`" + ` / ` + "`max_depth`" + ` / ` + "`dispatch`" + `），
用来验证预算边界本身。

## 硬规则

- **没有断言的用例会被拒绝**：一条「什么都不校验」的用例比没有用例更糟，它会稳定通过。
- 字段名写错直接报错（未知字段不忽略），避免 ` + "`content_contain`" + ` 这种拼写让用例变成假绿。
- 用例只描述输入与期望，不要写路径、密钥、内网地址：它会随发布快照被所有人看到。
- 行为用例里的工具是**桩**（用 ` + "`tools`" + ` 声明），不连真实资源；它们验证的是循环与说明书，不是外部系统可用性。`

const pluginDevToolDoc = `# 工具插件（kind=tool）

工具跑在沙箱里：文件按 fs 段挂载，出网只能走宿主代理（域名与动词由 capabilities 决定）。默认**一次性**：每次调用起一个进程、跑完即退，不占常驻池。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "order_lookup",
  "name": "订单查询",
  "description": "按订单号查询状态与金额，客服核对订单时使用",
  "capability": "http.request",
  "runtime": { "entry": "index.ts" },
  "fs": { "read": ["data/"], "write": ["output/"] },
  "input_schema": {
    "type": "object",
    "properties": { "order_no": { "type": "string", "minLength": 6 } },
    "required": ["order_no"],
    "additionalProperties": false
  },
  "capabilities": [
    { "method": "http.request", "hosts": ["api.example.com"], "verbs": ["GET"] },
    { "method": "secret.use", "name": "erp_token" }
  ],
  "limits": { "max_rpc": 10, "timeout_ms": 8000, "max_result_bytes": 4096 }
}
~~~

- runtime 接受两个字段：entry 与 resident。默认一次性；写 "resident": true 才在调用之间复用进程，见下文「常驻与状态」。
  超时与 RPC 上限写在 limits（那是**单次调用**的上限），写进 runtime 会解析失败。
- fs 是文件权限的唯一声明处，不写进 capabilities：
  - 默认：整个空间工作区**只读**，不能写；
  - 写权限必须显式声明 fs.write，没声明就完全不能写（连自己的工作区也不给）；
  - 声明了 fs.read 就只读这些子目录；
  - read / write 的每一项都必须是空间工作区的**子目录**：绝对路径、「..」、「.」（表示整个空间）都会被拒绝；
  - 未声明的目录在沙箱里**不存在**（报 ENOENT，不是权限错误）；
  - 空间之外的路径（其它空间、published、凭证目录）永远不可见。
- 这些选择会原样出现在「安装确认」里给用户看，写宽了会劝退安装方。
- 示例里的 secret.use 不是可选项：脚本里调了 host.secret("erp_token")，清单就必须有对应的一条，
  否则运行时会被拒绝（不是返回空串）。

## 入口
~~~ts
// 不需要 import：平台把 host 与 wa 挂到全局
export default async function (input: { order_no: string }) {
  // 普通出网：走宿主代理，域名必须在 capabilities.hosts 里
  const res = await wa.fetch("https://api.example.com/orders/" + input.order_no);
  if (res.status !== 200) throw new Error("订单服务返回 " + res.status);

  // 需要凭证：按名字取明文，自己拼 header
  const token = await host.secret("erp_token");
  const detail = await wa.fetch("https://api.example.com/orders/" + input.order_no + "/detail", {
    headers: { Authorization: "Bearer " + token },
  });

  // 文件：沙箱里已经按 fs 段挂载，直接读写（写 output/ 就必须在 fs.write 里声明它）
  const cache = wa.path("output/orders.json");
  await Deno.writeTextFile(cache, JSON.stringify(res.json()));

  return { status: res.json(), detail: detail.json() };
}
~~~

- 入口必须 export default 一个函数，参数是本轮入参对象，返回值即工具结果。
- host 与 wa 由平台挂到全局，**不要 import ./host_sdk.ts**：那个文件在运行时目录里，
  插件目录下不存在，相对 import 会直接报模块找不到。
- 不要读 stdin、不要自己实现协议。

## 出网：wa.fetch 走代理
- wa.fetch(url, init) 返回 { status, ok, headers, body, json() }；body 已按
  Content-Length / chunked 解码。域名不在 capabilities.hosts 里会被代理直接拒绝（403），
  方法不在 verbs 里同样拒绝。
- 代理在宿主侧完成 TLS，插件只发 absolute-form 明文请求到本机 socket；
  沙箱仍然是 --unshare-net，没有第二条出网路径。
- **只支持 HTTP/1.1 请求-响应**：HTTP/2、gRPC 流不支持；WebSocket 单独见下一节。
- wa.path("data/x.json") 把相对路径拼成空间工作区下的绝对路径；
  没声明的目录在沙箱里不存在（ENOENT），不是权限错误。

## WebSocket：声明 ws.connect，用原生 API
- 必须先声明，**没声明就连不出去**（升级请求会被代理拒绝，403）：

  ~~~json
  "capabilities": [
    { "method": "ws.connect", "hosts": ["api.dingtalk.com"] }
  ]
  ~~~

- 声明之后直接用 Deno 的 WebSocket，不需要任何封装：

  ~~~ts
  const ws = new WebSocket("wss://api.dingtalk.com/v1.0/gateway");
  ws.onmessage = (e) => console.log(e.data);
  ~~~

- 原理：宿主把 HTTPS_PROXY 指向本机 socket，Deno 把握手请求发给代理，
  代理连上游、转发 Upgrade、收到 101 之后双向透传。
- 用 wss://。ws:// 明文也会走代理，但宿主看得见内容，别拿它传敏感数据。
- 宿主看得到域名和路径，看不到 101 之后的帧；也做不了响应大小限制。
  权限文案会明确写「建立加密隧道」，安装方看得见。
- 长连接由你维护：心跳、重连、消息去重都要自己写，宿主不会替你重连。
- 一次调用结束后进程就退了，**连接不会跨调用存活**；需要长连接必须声明 runtime.resident。

## 凭证：host.secret(名称) 取明文
- 名字必须在清单里逐个声明，**没声明的一律被宿主拒绝**（不是返回空串）：

  ~~~json
  "capabilities": [
    { "method": "secret.use", "name": "erp_token" }
  ]
  ~~~

- 一个凭证一条能力：写 name 字段就是声明能取它，不写 name 直接解析失败。
  「声明了 secret.use」不等于「能读所有凭证」，这是两件事。
- 拿到的是明文：**不要写进日志、不要拼进报错信息、不要落到空间文件里**。
  插件进程内出现明文，沙箱逃逸或一行 console.log 就等于泄露。
- 密钥由平台管理员在系统空间的「密钥配置」里录入，插件只写名字，不写值。

## 文件：直接读写，不再有宿主 ABI
- Deno.readTextFile(wa.path("data/x")) / Deno.writeTextFile(...) / Deno.readDir(...)。
- 能看见什么由 fs 段决定：默认整个空间工作区**只读**，写必须显式声明 fs.write。
- host.call("fs.read" / "fs.list" / "fs.write") 已经从宿主 ABI 移除：调用会报错并给出替代写法。
  绕开挂载去读文件等于把沙箱白名单当摆设，所以这条通道不保留。

## 常驻与状态
- 默认一次性：每次调用都是一个新进程，模块级变量不会跨调用保留。
  顶层初始化（读配置、建连接池）每次都要跑一遍，别写成「第一次才执行」的假设。
- 需要跨调用保留状态（长连接、缓存、心跳）时，在清单里显式声明：

  ~~~json
  "runtime": { "entry": "main.ts", "resident": true }
  ~~~

  声明后才复用同一个进程，代价是常占内存（约 30MB 起）并占用常驻池名额
  （默认全服务 32 个，满了淘汰最久未用的空闲进程）。
- 常驻时**不要把上一位用户的数据放在模块变量里**，那会跨调用泄漏给下一个人。
- 常驻进程在插件升级、停用、卸载、空间重载后会被立刻重启，不用自己处理热更新。
- limits.timeout_ms 是**单次调用**的墙钟；超时即杀进程，下次调用重新起（常驻也一样）。
  不要起后台任务等结果，宿主不会等它。
- 崩溃不必自己兜底：进程退出后下次调用自动重建，但**这次调用一定失败**，
  错误处理照常写，不要用 process.exit 当"取消"。
- 空间面板的「试运行」总走一次性进程，即使声明了 resident 也一样。

## 能力与风险
- http.request 必须写 hosts（只写域名，不带协议与端口）；verbs 省略时只放行 GET / HEAD，只有这两个算只读。
  hosts 同时是代理白名单：写少了就是连不上，写多了安装时会被人看见。
- 文件权限不要写进 capabilities：能读什么、能不能写只看 fs 段（写了 fs.* 直接解析失败）。
- 凭证用 host.secret(名称) 取明文，名字逐条声明在 capabilities 里；宿主不再代发任何请求。
- 风险由能力推导：只读 HTTP = L2；写文件 / 写 HTTP / shell / ws.connect = L3；secret.use 加外部写 = L4。
- ws.connect 只接受 hosts，写 verbs 或 name 都会解析失败（隧道里宿主看不到方法）。

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

## 鉴权：static 与 jwt

static 是默认模式：auth.secret 只写凭证名（如 crm_token），值由用户用 secret_request（scope=mcp）自己录，
**不要向用户索要 Token 明文**，用户贴了就提醒他作废重签，再走 secret_request。

如果 MCP Server 要认识「当前是谁在调用」，改用 jwt 模式，不要写 auth.secret：

~~~json
"auth": { "in": "header", "name": "Authorization", "mode": "jwt", "app": "crm_mcp" }
~~~

- app 是应用名（1-64 位字母数字与 _-），留空回落到 slug。平台为每个应用生成一把独立的 HS256 密钥，
  同一应用在多个空间安装时共用这一把。
- 每次 tools/call 平台按当前用户现签一个 5 分钟令牌，header 值是 Bearer <jwt>，claims 为：
  sub / user_id（用户 ID）、email、job_number、iss=workagent、aud=应用名、iat、exp。
  握手与 tools/list 没有用户上下文，用的是服务令牌（sub=app:<app>、act=platform）。
- jwt 模式不允许再写 auth.secret：两种来源并存时平台无法判断该信哪个。
- 密钥怎么给到 Server 侧：发布响应里的 jwt 字段，或 GET /spaces/{id}/plugins/{slug}/jwt-key
  （空间 manage 权限）。部署者把它配到 MCP Server 的密钥配置里，服务端用同一把密钥验签。
- 服务端验签口径：HS256，密钥原样作为 HMAC key，校验 iss=workagent、aud=应用名、exp 未过期，
  然后从 user_id / email / job_number 取用户信息。**密钥是应用级的，不能跨应用复用。**
- 凭证存储没启用（未配 SECRET_KEK）时签发不了令牌：这类插件会明确不可用，不会退回无鉴权调用。

## 规则
- transport.url 默认要求 https。内网 MCP Server 只有 http 时，显式写 transport.allow_http: true 即可，
  权限文案会标成「明文 http」，安装的人看得见代价；本机（localhost / 127.0.0.1 / ::1）用 http 不必声明。
- allow_http 是「允许」不是「必须」：清单写 https 地址同时声明 allow_http，安装方才能在「配置」里
  把服务地址覆盖成内网 http（反过来不行，发布方没声明就不能靠改地址绕成明文）。
- tools.allow 必填且非空：默认不信任远端全部工具；支持尾部 * 前缀匹配，deny 优先。
- MCP 默认按 L3 处理（远端工具可能写数据）；同时用到密钥时按 L4 走系统管理员。
- 安装后服务地址、鉴权 Header 名、凭证名可按空间覆盖（空间插件面板的「配置」），发布快照不动。
- 远端暂不可用不会让空间失去其他工具：连接失败只影响这个 MCP。`

const pluginDevComponentDoc = `# 组件插件（kind=component）

会话里渲染的界面：声明式 DSL，不执行代码。平台前端按节点原生渲染，
组件拿不到 window / cookie / 网络，也注入不了 CSS。

## 清单
~~~json
{
  "protocol": "wa-plugin/1",
  "kind": "component",
  "slug": "sales_board",
  "name": "销售看板",
  "description": "按周期展示销售明细，需要看本月/本季销售时使用",
  "capability": "ui.render",
  "ui": { "entry": "ui.json" },
  "data_sources": [
    { "id": "orders", "plugin": "mysql", "tool": "sql_query", "mode": "read", "params": { "limit": 20 } },
    { "id": "submit", "plugin": "crm", "tool": "lead_create", "mode": "action" }
  ]
}
~~~
- slug：小写字母开头、只含 [a-z0-9_]，2-32 位；同空间唯一。
- data_sources[].id：DSL 里引用的名字；plugin 必须是**当前空间已安装**的插件 slug。
- mode：read 在渲染时自动取数；action 只能由按钮触发，且建议配 confirm。
- 不要写 version（发布时生成）、不要写 risk（由能力推导）、不要写 trust / events。
- 组件不能自己写接口地址或工具名：取数一律经 data_sources，由宿主带着当前用户身份走权限、审批与审计。

## 文件结构
~~~
plugins/sales_board/
  plugin.json   # 元数据唯一入口
  ui.json       # 渲染 DSL，ui.entry 默认就是它
~~~

## ui.json 骨架
~~~json
{
  "dsl": "wa-ui/1",
  "height": 360,
  "state": {"period": "month"},
  "data": {"orders": {"params": {"period": "{{state.period}}"}}},
  "view": {"type": "Stack", "gap": "md", "children": []}
}
~~~
- height：120-2000，组件最小高度。
- state：初始状态，键必须是 a-z 开头的 [a-z0-9_]。
- data：键必须是 data_sources 里的只读数据源 id；auto: false 可关掉首屏取数；params 支持 {{state.x}}。

## 可用节点
| type | 必需字段 | 说明 |
|---|---|---|
| Stack | children | 纵向布局，gap: sm/md/lg |
| Row | children | 横向布局，align: start/center/end/between |
| Grid | columns(1-4)、children | 网格 |
| Card | children | 卡片，title 可选 |
| Heading | text | 标题，level: 1-4（默认 3） |
| Text | text | 正文，size: sm/md/lg，tone: default/muted/success/warning/danger/info |
| Badge | text | 徽标，tone 同上 |
| Alert | text | 提示块，title 可选，tone 默认 info |
| Stat | label、value | 指标卡，delta 可选 |
| KeyValue | items | [{"label":"客户","value":"{{data.orders.name}}"}] |
| List | source | source 绑定数组，元素用 title/description/badge/tone 字段 |
| Table | columns、source | columns: [{"key":"name","label":"客户","align":"left"}]；source 绑定对象数组 |
| Progress | value | 进度条，max 默认 100，label 可选 |
| Button | text、action | 触发动作 |
| Input | name | 文本输入，写入 state.<name> |
| Select | name、options | 下拉，options: [{"label":"本月","value":"month"}] |
| Tabs | name、options | 页签，选中值写入 state.<name> |
| Divider | — | 分割线 |

所有节点可选 when：{"path":"state.period","op":"eq","value":"month"}，op ∈ eq/ne/gt/lt/in/exists/truthy。
字段写错会直接校验失败（不会静默忽略），节点最多 300 个、嵌套最多 8 层。

## 绑定
- 只支持 {{state.x}}、{{props.x}}、{{data.<数据源id>.x}} 三种路径，不能写表达式或函数。
- 整个字符串就是一个绑定时保留原始类型："source": "{{data.orders.items}}" 得到数组。
- 文本里可以拼接："共 {{data.orders.total}} 条"。
- 字符串里出现无法配对的 }} 会被拒绝；纯文本不需要转义。

## 动作
| 写法 | 用途 |
|---|---|
| {"type":"reload","target":"orders","params":{…}} | 重新取只读数据 |
| {"type":"invoke","source":"submit","params":{…},"confirm":"确认提交？"} | 调写数据源；必须用户点击，成功后自动刷新只读数据 |
| {"type":"message","text":"帮我把 {{state.period}} 的销售明细导出"} | 把一句话填进输入框，由用户决定是否发送 |

## 生成流程
1. 先问清：展示什么、数据来自哪个**已安装**插件的哪个工具、有没有写操作。
2. file_write("plugins/<slug>/plugin.json", …)，再 file_write("plugins/<slug>/ui.json", …)。
3. 让用户点插件的「试运行」：节点 / 字段 / 绑定 / 数据源写错会直接报错并指出位置，按提示改文件即可。
4. 告诉用户在空间插件面板发布并安装；会话里用下面的围栏引用（版本可省略，默认用已安装版本；不要自己编版本号）：
   ~~~ui-block
   {"slug":"sales_board","props":{}}
   ~~~

## 边界
- 组件不能发网络请求、不能读本地文件、不能执行脚本：需要新数据就新增数据源并让用户安装对应插件。
- 不要伪造版本号；版本由发布生成，历史消息固定渲染当时版本。`

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
