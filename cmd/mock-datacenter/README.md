# Mock 大数据平台（mock-datacenter）

一个本地可跑的模拟数仓，用来联调「数据分析类智能体」。它提供两组等价的能力：

- **MCP 工具**（推荐）：`search_tables` / `submit_query` / `get_query_status` / `get_query_result` / `download_query_result`
- **REST 接口**：`/api/v1/tables`、`/api/v1/queries...`，语义与工具一一对应

> **给智能体**：数据是模拟的。SQL 只做「找表 → 拼样例结果」的处理，**不执行聚合、过滤与列裁剪**。
> 结论要说明数据来自 mock，不要把样例行当成真实统计值。

---

## 1. 工具清单

### search_tables — 找表

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `keyword` | string | 否 | 关键字；为空时列出全部表 |
| `database` | string | 否 | 限定库名：`dw` / `dm` / `ods` |
| `page` | int | 否 | 页码，默认 1 |
| `page_size` | int | 否 | 每页条数，默认 20，最大 100 |

`keyword` 会匹配：表全名、表名、库名、描述、数仓分层、标签、分区字段、**字段名与字段注释**、负责人姓名/工号/邮箱。

返回：

```json
{
  "total": 1,
  "page": 1,
  "page_size": 20,
  "items": [
    {
      "database": "dm",
      "name": "dm_sales_daily",
      "full_name": "dm.dm_sales_daily",
      "description": "销售日报，按城市与统计日期汇总订单量、GMV 与退款量",
      "layer": "DM",
      "owner": {
        "name": "严小序",
        "account_type": "个人账号",
        "status": "正常",
        "job_number": "99999",
        "email": "yanxiaoxu99999@autohome.com.cn",
        "account_id": "03651554640120054733"
      },
      "columns": [
        { "name": "stat_date", "type": "date", "comment": "统计日期", "nullable": false },
        { "name": "city", "type": "string", "comment": "城市", "nullable": false }
      ],
      "partition_keys": ["stat_date"],
      "tags": ["销售", "日报", "指标"],
      "row_count": 12460,
      "size_bytes": 1048576,
      "update_time": "2026-11-14 09:20:00"
    }
  ],
  "caller": "严小序"
}
```

`items` 是完整元信息，字段较多。先看 `full_name` 和 `columns`，需要时再展开。

### submit_query — 提交 SQL

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sql` | string | **是** | 必须是一条 SELECT |
| `engine` | string | 否 | 执行引擎，默认 `spark` |
| `wait_seconds` | int | 否 | 提交后阻塞等待的秒数，0 = 立即返回，最大 25 |

返回任务对象（`task_id` 形如 `spark-1763100000-0007`）：

```json
{
  "task_id": "spark-1763100000-0007",
  "engine": "spark",
  "sql": "select city, gmv from dm.dm_sales_daily limit 5",
  "status": "succeeded",
  "progress": 100,
  "message": "任务执行成功，返回 5 行",
  "columns": ["stat_date", "city", "order_cnt", "gmv", "avg_price", "refund_cnt"],
  "row_count": 5,
  "duration_ms": 1500,
  "created_at": "2026-11-14T10:00:00+08:00",
  "started_at": "2026-11-14T10:00:00+08:00",
  "finished_at": "2026-11-14T10:00:01+08:00",
  "result_url": "http://127.0.0.1:8322/api/v1/queries/spark-1763100000-0007/result",
  "download_csv_url": "http://127.0.0.1:8322/api/v1/queries/spark-1763100000-0007/download?format=csv",
  "download_json_url": "http://127.0.0.1:8322/api/v1/queries/spark-1763100000-0007/download?format=json"
}
```

**直接传 `wait_seconds: 20`**：多数查询 1.5 秒内完成，一次调用就拿到终态，省掉轮询。

任务执行失败时这次调用**直接返回 `isError=true`**，文本里是同样的任务对象（`status=failed` + `message`），不要当成提交失败重试。

### get_query_status — 查状态

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `task_id` | string | **是** | 提交时返回的任务 ID |
| `wait_seconds` | int | 否 | 阻塞等待秒数，0 = 立即返回，最大 25 |

状态机：`pending` → `running`（progress 10 / 35 / 60）→ `succeeded` | `failed`。

- `succeeded`：`row_count`、`duration_ms`、`message` 有值
- `failed`：`message` 是失败原因，无结果可取
- 任务不存在：MCP 返回 `isError=true`，文本为 `任务不存在：<id>`

### get_query_result — 取结果（JSON）

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `task_id` | string | **是** | 任务 ID |
| `offset` | int | 否 | 起始行号，默认 0 |
| `limit` | int | 否 | 返回行数，默认 50，最大 200 |

成功时：

```json
{
  "task_id": "spark-1763100000-0007",
  "status": "succeeded",
  "columns": ["stat_date", "city", "order_cnt", "gmv", "avg_price", "refund_cnt"],
  "rows": [
    { "stat_date": "2026-11-13", "city": "北京", "order_cnt": 4821, "gmv": 128930.5, "avg_price": 26.74, "refund_cnt": 61 }
  ],
  "row_count": 5,
  "offset": 0,
  "has_more": false
}
```

未结束时**不报错**，返回 `status` + `progress` + `message`；失败时返回 `status=failed`、`message`、空 `columns`/`rows`。两种情况下都要先看 `status`。

### download_query_result — 下载文件

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `task_id` | string | **是** | 任务 ID |
| `format` | string | 否 | `csv`（默认）或 `json` |

```json
{
  "task_id": "spark-1763100000-0007",
  "format": "csv",
  "file_name": "spark-1763100000-0007.csv",
  "row_count": 5,
  "columns": ["stat_date", "city"],
  "download_url": "http://127.0.0.1:8322/api/v1/queries/spark-1763100000-0007/download?format=csv",
  "note": "下载链接需要携带同一个 JWT 才能访问（Authorization: Bearer <token>）",
  "preview": "stat_date,city\n2026-11-13,北京\n"
}
```

- 只有 `succeeded` 的任务能下载；未结束 / 失败时 `isError=true`
- 下载链接是完整结果集，不是分页预览
- 结果有限（单任务最多 200 行），下载和 `get_query_result` 拿到的是同一份数据

---

## 2. 典型流程

```
search_tables(keyword="销售")                       # 1. 找表，确认库表名与字段
submit_query(sql="select ... limit 100", wait_seconds=20)   # 2. 提交并等到终态
   └─ 若 status 仍是 running：get_query_status(task_id, wait_seconds=20) 继续等
get_query_result(task_id)                           # 3a. 看数据（分页）
download_query_result(task_id, format="csv")        # 3b. 要文件时用这个
```

判断终止条件的顺序：

1. `status == succeeded` → 用 `get_query_result` 或 `download_query_result`
2. `status == failed` → 把 `message` 原样告诉用户，不要重试同样的 SQL
3. `status` 是 `pending`/`running` → 用 `wait_seconds` 再等一次，最多重复 2~3 次；仍不结束就告诉用户任务还在跑，给出 `task_id`

**省调用**：`submit_query` 带上 `wait_seconds` 能省掉一次 `get_query_status`；取数优先 `get_query_result`，用户明确要文件时才 `download_query_result`。不要为同一个问题反复提交相同 SQL。

---

## 3. 可用的表（共 6 张）

| 全名 | 说明 | 分区 | 主要字段 |
|---|---|---|---|
| `dw.dw_order_detail` | 订单明细宽表，全渠道成交与退款订单 | `dt` | order_id, user_id, city, order_amount, order_status, channel, create_time |
| `dw.dw_user_profile` | 用户画像宽表，基础属性与关注车型 | `dt` | user_id, gender, age, city, register_date, is_new_user, prefer_series |
| `dm.dm_sales_daily` | 销售日报，按城市与日期汇总订单量、GMV、退款量 | `stat_date` | stat_date, city, order_cnt, gmv, avg_price, refund_cnt |
| `dm.dm_vehicle_visit` | 车型访问指标，PV / UV / 留资转化 | `stat_date` | stat_date, series_id, series_name, pv, uv, clue_cnt |
| `ods.ods_clue` | 线索流水，各渠道留资与跟进状态 | `dt` | clue_id, source, phone_masked, series_name, status, created_at |
| `dw.dim_city` | 城市维表，省份与大区归属 | 无 | city_code, city_name, province, region |

所有表负责人都是 **严小序（工号 99999，yanxiaoxu99999@autohome.com.cn）**，搜「严小序」或「99999」能列出全部表。

跨表字段可以关联：`dw_order_detail.user_id` ↔ `dw_user_profile.user_id`，`dm_vehicle_visit.series_name` ↔ `ods_clue.series_name`，`city` / `city_name` 是同城异名。

---

## 4. SQL 的真实行为（重要）

这个 mock **只解析 SQL 的一小部分**，写 SQL 时按下面的规则预期结果：

| SQL 写法 | 实际行为 |
|---|---|
| `select a, b from t` | 返回 `t` 的**全部列**，`select` 列表被忽略 |
| `where` / `group by` / `order by` / 聚合函数 | **不生效**，返回样例行 |
| `limit N` | **生效**：返回 N 行（默认 5，上限 200） |
| `from a join b` | 只看第一个 `from`/`join` 后的表，join 本身不执行 |
| 表名带库名、双引号包裹 | 支持：`dw.dim_city`、`"dw.dim_city"`；**不要用反引号**，解析不到表名会退化成 `result=1` |
| 没有 `from`（如 `select 1`） | 返回单列 `result`，值 `1` |
| 以写操作关键字开头（insert/update/delete/drop/create/truncate/alter/merge） | 失败：`当前账号为只读账号，不允许执行 DDL/DML，请只提交 SELECT 查询` |
| SQL 中出现 `fail` / `error` / `boom` | 故意失败（测试异常分支用） |
| 表名不存在 | 失败：`Table or view not found: xxx` |

行内容是指定表的样例行**循环填充**到 `limit` 行，所以：

- 行数不等于真实数据量，**不要据此下「共 N 条记录」的结论**
- 大 `limit` 会出现重复行，去重后行数更少是正常现象
- 需要「真实感」时用 `limit 5`~`20`，别写 `limit 1000`（会被截到 200）

**写 SQL 的建议**：仍然写标准 Spark SQL（`select ... from 库.表 where 分区条件 limit N`），这样换成真实数仓时不用重写；只是要清楚 mock 目前不校验语义。

---

## 5. 错误与状态

| 场景 | REST | MCP |
|---|---|---|
| 缺 token / token 无效 | 401 `unauthenticated` / `invalid_token` | 连接阶段失败 |
| `sql` 为空、`format` 非法、请求体不是 JSON | 400 `invalid_argument` | `isError=true` + 原因文本 |
| 任务不存在 | 404 `task_not_found` | `isError=true` + `任务不存在：<id>` |
| 任务未结束就下载 | 409 `task_not_finished` | `isError=true` |
| 失败任务下载 | 422 `task_failed` | `isError=true` |
| 任务未结束就取结果 | 202 + `status`/`progress` | 正常返回（`isError=false`），先看 `status` |

MCP 侧的失败有两种形态，别混：

- `isError=true`：参数错、任务不存在、`submit_query` 执行失败、任务未结束或已失败却仍要结果/下载
  → 读文本里的 `message`，按原因纠正，不要当网络错误盲目重试
- `isError=false` 但 `status=failed`：只有 `get_query_result` 这样返回——它把「任务失败」当数据回执
  → 同样没有数据，读 `message`

---

## 6. 身份与鉴权（智能体不用处理）

平台以 MCP 插件形式接入时会自动注入身份，header 为 `Authorization: Bearer <jwt>`（HS256），claims 含 `sub` / `user_id` / `email` / `job_number` / `iss` / `aud` / `exp`。

- 工具调用**不需要**模型提供任何 token、cookie 或账号参数
- 不要向用户索要凭证，也不要在回答里复述 token
- 手工 curl 调试时用 `POST /auth/token` 签一个（见附录）

服务端校验签名、`iss`、`aud`、`exp`；任一不通过都返回 401，与业务错误无关。

---

## 7. 回答用户时的口径

- 明确说明数据来自 **mock 数据集**，不是生产数仓
- 给出表名 + SQL + 行数时，行数是「本次返回行数」，不是表总量（表总量看 `row_count` 元信息）
- 不要编造 `task_id`，也不要把上一次任务的数字套到新问题上
- 需求超出能力（真实聚合、导出全量、跨库血缘）时直接说明边界，给出可行替代（换 `limit`、换表、找负责人严小序）

---

## 附录：启动与接入（开发者用，模型可跳过）

```bash
make mock-dc          # 127.0.0.1:8322，http
```

环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `MOCK_DC_ADDR` | `127.0.0.1:8322` | 监听地址 |
| `MOCK_DC_JWT_SECRET` | `mock-datacenter-dev-secret` | HS256 密钥；**接平台 jwt 模式时填应用密钥** |
| `MOCK_DC_JWT_ISS` | `workagent-mock-dc` | 签发者；平台签的是 `workagent` |
| `MOCK_DC_JWT_AUD` | `mock-dc` | 受众；平台签的是**应用名** |
| `MOCK_DC_JWT_SKIP_AUD` | 空 | `true` 时不校验 aud |
| `MOCK_DC_JWT_DISABLE_AUTH` | 空 | `true` 时跳过鉴权（仅本地调试） |
| `MOCK_DC_ALLOW_TOKEN_ISSUE` | `true` | `false` 关闭 `/auth/token` |
| `MOCK_DC_QUERY_DELAY_MS` | `1500` | 单任务模拟耗时 |
| `MOCK_DC_PUBLIC_URL` | 空 | 下载链接前缀，默认按请求 Host 推导 |
| `MOCK_DC_TLS_CERT` / `MOCK_DC_TLS_KEY` | 空 | 同时配置时以 https 监听（插件 `transport.url` 强制 https，需要这条） |

本地 TLS：

```bash
mkcert -install && mkcert 127.0.0.1 localhost
MOCK_DC_TLS_CERT=./127.0.0.1+localhost.pem \
MOCK_DC_TLS_KEY=./127.0.0.1+localhost-key.pem make mock-dc
```

平台接入（插件清单，`files/plugins/<slug>/plugin.json`）：

```json
{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "mock_datacenter",
  "name": "Mock 大数据平台",
  "description": "表搜索、Spark SQL 异步任务、结果预览与下载",
  "capability": "http.request",
  "transport": { "type": "http", "url": "https://127.0.0.1:8322/mcp" },
  "auth": { "in": "header", "name": "Authorization", "mode": "jwt", "app": "mock_dc" },
  "tools": {
    "allow": [
      "search_tables", "submit_query", "get_query_status",
      "get_query_result", "download_query_result"
    ]
  }
}
```

`mode: jwt` 时平台为应用 `mock_dc` 生成密钥并按用户现签令牌，**不需要**在会话里录入凭证。把这个密钥配到本服务：

```bash
MOCK_DC_JWT_SECRET=<发布响应里的 jwt.key> \
MOCK_DC_JWT_AUD=mock_dc \
MOCK_DC_JWT_ISS=workagent \
make mock-dc
```

密钥获取：发布响应里的 `jwt.key`，或 `GET /spaces/{id}/plugins/{slug}/jwt-key`（空间 manage 权限）。

用静态凭证调试（不走平台签发）：

```bash
curl -s -XPOST localhost:8322/auth/token -H 'Content-Type: application/json' \
  -d '{"ttl_seconds":2592000}' | jq -r .token
# 再把 token 填进凭证（scope=mcp 的 secret_request，或管理员接口 PUT /admin/secrets/plugin/<name>）
```

注意 MCP 插件被 `Derive` 判为 `risk=write`，首次调用需要管理员绑定/审批；这与鉴权失败是两回事。
