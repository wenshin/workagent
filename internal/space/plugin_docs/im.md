# IM 渠道插件（kind=tool + im.reply）

把外部 IM 里的消息接进平台：渠道插件收到消息后**报给宿主**，宿主把消息投进那个人
固定的渠道会话并跑完一轮，再把答复交回插件去回复。

渠道插件是**宿主的下游，不是控制器**：它指定不了空间或会话，也读不到会话内容。
能做的只有两件事——投递一条消息、报告这条消息回过了。

## 清单

```json
{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "dingtalk_bot",
  "name": "钉钉机器人",
  "description": "接收钉钉消息并回复（长连接）",
  "capability": "im.reply",
  "runtime": { "entry": "main.ts", "resident": true },
  "im": { "channel": "dingtalk", "label": "钉钉" },
  "capabilities": [
    { "method": "im.reply" },
    { "method": "ws.connect", "hosts": ["api.dingtalk.com"] },
    { "method": "http.request", "hosts": ["api.dingtalk.com", "oapi.dingtalk.com"], "verbs": ["GET", "POST"] }
  ]
}
```

- kind 仍是 tool：渠道插件是工具插件的一种，`im.reply` 只是它多出来的一条能力。
- `im.channel` 是渠道标识（dingtalk / wecom / feishu…），进数据库、也进会话绑定键；
  留空回落到 slug。**发布后不要改**：换它等于换一条会话时间线。
- `im` 段只描述「消息从哪来」，不参与风险推导；出网范围仍由 `capabilities` 决定。
- 长连接必须 `runtime.resident: true`：一次性进程的连接活不过一次调用，进度也会丢。
- `im.reply` 不接受额外参数（写 hosts / verbs / name 都解析失败）：
  能投给谁由宿主按「渠道 + 用户」算，清单里写什么都没用。

## 平台注入的配置（作者不用配，也没法配）

声明了 `im.reply` 的插件，`wa.config()` 里会多三项：

| 键 | 含义 |
|---|---|
| `host_base` | 宿主在沙箱里可达的地址（部署配 `PLUGIN_HOST_BASE`，留空回落 `APP_BASE_URL`） |
| `host_space_id` | 插件所在空间 id |
| `host_token` | 插件身份令牌，由 `HMAC-SHA256(PLUGIN_TOKEN_SECRET, 空间 + slug)` 派生 |

- 这三项不能用清单的 `config` 段声明（保留键），也不该让使用者手抄：沙箱里的
  `127.0.0.1` 是 Pod 自己，抄错的唯一表现是投递 401、排查方向全错。
- 宿主地址还会以「精确 host:port」自动进这次调用的出网白名单，否则代理会 403。
- `PLUGIN_TOKEN_SECRET` 没配就签发不出令牌，`/inbox` 一律 401：宁可渠道不通，
  也不开匿名写入口。换密钥即所有已签发令牌失效。
- 注入面跟着能力走：没声明 `im.reply` 的普通工具拿不到这三项——多一条「能调平台
  接口」的入口就是多一份风险。

## 投递：POST /spaces/{id}/plugins/{slug}/inbox

请求头带 `X-WA-Plugin-Token: <host_token>`（**没有、也不该有登录 Cookie**），
路径里的空间与 slug 就是宿主校验的身份，插件选不了别的空间。

```json
{
  "items": [
    { "channel_uid": "staff-xxx", "name": "张三", "text": "帮我查一下", "ref": "msg-1", "reply_to": "..." }
  ]
}
```

- `channel_uid`：发送者在渠道侧的稳定 id（钉钉是 `senderStaffId`）。宿主用它查
  `user_identity.employee_id`——**不是**登录用的 unionId，拿 unionId 查必然一条也匹配不上。
- `text` 是要交给模型的消息；`ref` 是插件自己的消息 id，去重与回执都靠它。
- `reply_to` 宿主不解析，只原样回传（那是渠道侧的格式，比如钉钉的 sessionWebhook）。

回执与 items 一一对应：

```json
{
  "results": [
    { "ref": "msg-1", "channel_uid": "staff-xxx", "ok": true, "reply": "……", "session_id": "s-1", "need_login": false, "reason": "" }
  ]
}
```

- `ok=false` 时把 `reason` **原样回给渠道用户**：它已经是可读中文
  （如「上一轮还在执行，请稍后再发」），不要包一层「系统错误」。
- `need_login=true` 表示这个人没登录过平台：回一句「先去平台登录一次」，
  不要重试、也不要报 500。
- 宿主按 `ref` 去重 10 分钟：渠道重投不会跑第二轮模型，也不会回答两遍。
  `ref` 要稳定且唯一（用渠道侧的消息 id），不要每次现生成。
- 同一会话已有 run 时宿主明确拒绝（不排队、不并发）：并发会让两轮抢同一份历史。

回完消息调 `POST .../inbox/reply` 留痕（`{"ref","reply_to","note"}`）：只进日志与审计，
发送动作本身在插件里做（各渠道发送方式差别太大，宿主不掺和）。

## 会话与权限

- 一个人一条时间线：绑定键是 `(channel, user)`，不是 `(channel, 群)`——
  他在哪个群发都进同一个会话，上下文才连续，工具权限也只按这个人算。
- 渠道会话建在**个人空间**：run 的 actor 是会话所有者；落共享空间等于把内容给别人看。
- 会话不存在就现建；插件**指定不了**空间或会话——否则被篡改的插件就能把外部内容
  塞进任意会话。投递目标永远由宿主按渠道 uid 算。
- 只有声明了 `im.reply` 的插件能投递，没声明的一律拒绝（不是返回空结果）。

## 部署

- `PLUGIN_HOST_BASE` 必须与对外地址分开：执行体容器里的 `127.0.0.1` 是 Pod 自己，
  本机 k8s 要写 `http://host.docker.internal:8321`。
- `PLUGIN_TOKEN_SECRET` 必配，否则投递 401；同一套部署里控制面与执行体必须共用它。

## 多机器人（设计定稿，未实施）

一个插件可以同时接多台机器人（不同 appKey / robotCode / 长连接）。**这是插件自己的事**：机器人列表就是清单里一条 `records` 配置，运行时 `wa.config()` 直接读到数组，一个进程内起 N 条连接——不要发布 N 个插件。

```jsonc
"config": [{
  "key": "bots", "label": "机器人", "type": "records",
  // 从没配过时的初始条目；保存过之后以存储为准，删掉的不会自己补回来
  "default": [
    { "name": "默认机器人", "app_key": "", "app_secret": "", "agent": "base" }
  ],
  "item": { "title_key": "name", "fields": [
    { "key": "name",       "label": "名称",      "type": "text", "required": true },
    { "key": "app_key",    "label": "AppKey",    "type": "text", "required": true },
    { "key": "app_secret", "label": "AppSecret", "type": "secret_ref", "required": true },
    { "key": "agent",      "label": "对应智能体", "type": "agent_ref", "default": "base" }
  ]}
}]
```

- **配置**：`const bots = await wa.config().bots` 直接是数组（平台已解析好，不用 `JSON.parse`）。使用者加一台机器人就是加一条；保存后进程重建、读到新列表。`item.fields[].default` 是「新增一条时」的预填值，`default` 是「一条都没有时」的初始条目——两者不要混。
- **关联智能体用 `agent_ref`，不要用 `text`**：`agent_ref` 存的是本空间智能体的 slug，安装页会渲染成选择器、保存时校验可用性；写成 `text` 就只能让使用者手抄 slug，写错了要到投递被拒才发现。留空即 `base`。
- **密钥**：`bot.app_secret` 只是**名字**，明文用 `await wa.secret(bot.app_secret)` 取。名字由使用者填、可以每台不同；凭证按 `(空间, 插件)` 隔离，读不到别的插件或别的空间的。
- **投递**：调 `POST {host_inbox_url}`（或 `/spaces/{host_space_id}/plugins/{slug}/inbox`）时在每条消息里带 `agent: bot.agent`——平台据此把消息投进对应智能体的会话（绑定键 `(渠道, 智能体, 人)`），不传则落默认智能体。**路径里的空间用 `host_space_id`（安装空间），不要用 `wa.context().spaceId`**（那是运行空间，跨空间借用时会投错）。
- **出站用 `wa.context()` 挑机器人**：工具被调用时要发消息，先 `const ctx = await wa.context()`（`{ spaceId, agentId, userId, runId, sessionId }`），再 `bots.find(b => (b.agent || "base") === ctx.agentId)`；同一个智能体配了多台就取**第一台**（顺序即优先级），找不到就明确报「没有为该智能体配置机器人」，不要退到第一台。**`agentId` 不能缓存到模块顶层**——它是每次调用不同的，缓存下来就是 A 的机器人给 B 发消息；预热期调用 `wa.context()` 会直接报错，这是故意的。每次 `call` 帧对应一次调用，宿主按帧回 `result`，所以并发调用天然隔离。
- **令牌**：`host_token` 仍按 `(空间, 插件)` 派生，与机器人数无关；`host_inbox_url` 照旧注入，别自己拼。
- **注意**：不要把机器人配置缓存成模块级单例再让多条连接共享，每条连接必须用自己的那份。
