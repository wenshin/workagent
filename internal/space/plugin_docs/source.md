# 搜索源插件（kind=source）

搜索源不是「模型能调用的工具」，而是 `web_search` 背后的实现。

模型永远只看到 `web_search(query, limit, scope)` 这一个工具；内网知识库、博查、Tavily、自建检索网关都是「源」，由平台路由。所以加源要写成 source 插件，不要写成一个新工具——否则模型要在多个工具之间选，提示词里的检索规则会碎掉。

## 清单

```json
{
  "protocol": "wa-plugin/1",
  "kind": "source",
  "slug": "intranet_kb",
  "name": "内网知识库",
  "description": "公司内部文档与制度检索，回答内部流程问题前必须使用",
  "capability": "search.query",
  "source": {
    "protocol": "spec",
    "scope": "internal",
    "entry": "search.json",
    "endpoint": "https://es.corp.local/wiki/_search",
    "secret": "intranet",
    "allow_private": true,
    "priority": 10,
    "timeout_seconds": 8
  }
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `protocol` | 是 | `spec`（声明式）或 `script`（两段式脚本） |
| `scope` | 否 | `internal` / `external`，默认 external；决定模型用 `scope` 参数时选不选中它 |
| `endpoint` | 是 | 出网终点。写在这里而不是产物里：Guard 的白名单由它推导，必须在发布前可见 |
| `secret` | 否 | **凭证名**，不是密钥本身；由管理员在系统空间「密钥配置」里录入 |
| `allow_private` | 否 | 允许 http 与私网地址，内网源必须开；会推导成 L3，需要系统管理员 |
| `priority` | 否 | 同 scope 下的顺序，越小越优先，默认 0 |
| `timeout_seconds` | 否 | 单次搜索墙钟，1-25，默认 8 |

注意：**source 不允许写 `capabilities`**。它会出网，但终点已经由 `endpoint` 固定，不需要也不能声明别的能力。

## 一、spec 协议（默认首选）

`search.json` 只描述两件事：请求怎么拼、响应去哪取。**不写 endpoint**。

```json
{
  "request": {
    "method": "POST",
    "headers": { "Authorization": "Bearer {{secret}}" },
    "body": { "query": "{{query}}", "count": "{{limit}}" }
  },
  "response": {
    "items": "data.webPages.value",
    "title": "name",
    "url": "url",
    "snippet": "snippet",
    "published_at": "datePublished"
  }
}
```

### 请求侧

- `method`：`GET` / `POST`，默认 POST。
- `auth`：`{"in":"header|query|body","name":"...","prefix":"Bearer "}`；也可以在 `headers` 里直接写 `{{secret}}`。
- `body`：POST 的 JSON 模板，**可以嵌套**（ES 的 bool / multi_match 直接写）。
- `params`：GET 的查询参数模板。
- 占位符：`{{query}}` `{{limit}}` `{{freshness}}` `{{secret}}`。整个值正好是 `{{limit}}` 时保留数字类型。

### 响应侧

- `items`：结果数组路径，必填。
- `url`：必填。取不到 URL 的条目会被丢弃——回答里没有链接的引用就是编造。
- `title` / `snippet` / `published_at`：可空。
- 路径语法：`a.b[0].c`，只支持对象键与数组下标。
- 业务错误码：`error_code` + `error_ok`，如 `{"error_code":"code","error_ok":[0,"0"]}`。

### 三个模板

博查 / Tavily 这类 POST + Bearer：

```json
{ "request": { "method": "POST", "auth": {"in":"header","name":"Authorization","prefix":"Bearer "},
               "body": { "query": "{{query}}", "count": "{{limit}}" } },
  "response": { "items": "data.webPages.value", "title": "name", "url": "url", "snippet": "snippet" } }
```

自建网关 GET + query 参数：

```json
{ "request": { "method": "GET", "params": { "q": "{{query}}", "size": "{{limit}}" },
               "auth": {"in":"query","name":"apikey"} },
  "response": { "items": "items", "title": "title", "url": "link", "snippet": "summary" } }
```

Elasticsearch（嵌套 DSL，命中在 hits.hits）：

```json
{ "request": { "method": "POST", "headers": {"Authorization": "ApiKey {{secret}}"},
               "body": { "size": "{{limit}}",
                         "query": { "multi_match": { "query": "{{query}}", "fields": ["title","content"] } } } },
  "response": { "items": "hits.hits", "title": "_source.title", "url": "_source.url", "snippet": "_source.summary" } }
```

## 二、script 协议（spec 表达不了时才用）

命中任意一条再写脚本：请求要签名 / 时间戳 / 动态 token、响应不是 JSON、结果字段要计算、需要多步请求。

脚本跑在 Deno 沙箱里，**没有网络权限**，分两段：

- `plan(input)` 只输出 `{request:{method,url,headers,body}}`，不自己发请求；
- `map(response)` 收 `{status, body}`，输出 `{results:[...]}`。

输入里有 `query` / `limit` / `secret`（密钥走 stdin，不进 argv 与环境变量）与 `endpoint`。

```ts
export function plan(input: { query: string; limit: number; secret: string; endpoint: string }) {
  const body = JSON.stringify({ q: input.query, n: input.limit });
  return {
    request: {
      method: "POST",
      url: input.endpoint,
      headers: { "Content-Type": "application/json", "X-Sign": sign(body, input.secret) },
      body,
    },
  };
}

export function map(resp: { status: number; body: string }) {
  const data = JSON.parse(resp.body);
  return {
    results: (data.results || []).map((r: any) => ({
      title: r.title,
      url: r.link,
      snippet: r.summary,
    })),
  };
}
```

脚本自己打日志用 `console.error`，stderr 会进试跑与调用日志。

## 三、试跑：必须做，不然等于盲写

写完用 `plugin_try` 工具跑一次真实检索词：

```json
{ "slug": "intranet_kb", "query": "差旅报销标准" }
```

回执里的关键字段：

- `top_keys`：响应的顶层键。`items` 路径写错时，看这里就知道该改成什么。
- `status`：非 2xx 时业务错误体也在这，鉴权 / 参数问题一目了然。
- `hit` / `dropped`：命中与丢弃条数（没有 URL 的会被丢）。
- `samples`：前 3 条归一化结果，确认标题与摘要是不是想要的字段。

典型修法：

- `hit=0` 且 `top_keys` 有 `data`：`items` 改成 `data.<数组字段>`。
- `dropped>0`：`url` 路径指到了空字段，换成真实链接字段。
- `status=401/403`：`secret` 名字与管理员录入的不一致，或 `auth.in` 位置不对。
- `status=404`：`endpoint` 写错（注意 ES 的索引名在路径里）。

## 四、密钥与安全

- `secret` 只写名字，例如 `"secret": "intranet"`；值由管理员在系统空间「密钥配置」录入，scope 是 `search`，和平台内置搜索源共用一份凭证。
- 不要把密钥写进 `search.json`、脚本或 `plugin.json`：这些文件会进发布快照、备份与检索结果。
- 出网只走平台 Guard：域名白名单来自 `endpoint`，内网源还要 `allow_private: true`（推导 L3，安装到系统空间需要系统管理员）。即使开了 `allow_private`，链路本地地址（169.254.169.254）仍被拒绝。
- 源只读：不要指望它写数据，也不要让它返回非 http(s) 链接。

## 五、生效方式

- 装在**普通空间**：只对该空间生效，其它空间搜不到。
- 装在**系统空间**：全局生效，所有空间的 `web_search` 都能用。
- 安装 / 启用 / 停用 / 卸载**立刻生效**，不需要重启服务。
- 多个源同时可用时按 `scope` 与 `priority` 路由：`scope=auto`（默认）先查 internal 再补 external，结果按 URL 去重；`scope=internal` / `external` 只查对应范围。
- 同一个源换实现方式（spec → script）不需要改工具契约；两条路径的输出都经过同一套归一化（标题 300 字、摘要 2000 字、URL 必须能回溯）。
