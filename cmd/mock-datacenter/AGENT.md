# Mock 大数据平台 · MCP 工具说明

你通过 5 个 MCP 工具访问一个**模拟**数仓，能力是：找表 → 提交 Spark SQL → 查状态 → 取结果 / 下载文件。

数据是构造的：**SQL 只做「找表 → 拼样例行」，不执行聚合、过滤与列裁剪**。结论里必须说明数据来自 mock，不要把样例行当成真实统计值。

## 一、接口

| 项 | 值 |
|---|---|
| 端点 | `<base>/mcp` |
| 传输 | streamable HTTP：JSON-RPC 2.0 over HTTP POST，响应是 `application/json` |
| 协议版本 | `2024-11-05`（`initialize` 会回显请求里的版本） |
| 方法 | `initialize` / `ping` / `tools/list` / `tools/call`；通知类请求（无 `id`）回 202，无响应体 |
| 工具结果 | `result.content[0].text` 是一个 **JSON 字符串**，缩进排版，按字段读，不要按固定文本格式解析 |
| 鉴权 | 由调用方注入 `Authorization: Bearer <jwt>`。**工具参数里不要出现任何 token、cookie、账号**；也不要向用户索要凭证 |
| 业务失败 | `result.isError = true`，文本里是 `message` 或任务对象；协议级错误走 JSON-RPC `error` |

`serverInfo` 名称是 `mock-datacenter`，只暴露工具能力（`capabilities.tools`），没有资源与提示词。

## 二、工具

### 1. search_tables — 找表

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `keyword` | string | 否 | 关键字，为空时列出全部表 |
| `database` | string | 否 | 限定库名：`dw` / `dm` / `ods` |
| `page` | int | 否 | 页码，默认 1 |
| `page_size` | int | 否 | 每页条数，默认 20，最大 100 |

`keyword` 匹配范围：表全名、表名、库名、描述、数仓分层、标签、分区字段、**字段名与字段注释**、负责人姓名 / 工号 / 邮箱。

返回 `{ total, page, page_size, items[], caller }`，每个 item：

```json
{
  "database": "dm",
  "name": "dm_sales_daily",
  "full_name": "dm.dm_sales_daily",
  "description": "销售日报，按城市与统计日期汇总订单量、GMV 与退款量",
  "layer": "DM",
  "owner": { "name": "严小序", "job_number": "99999", "email": "yanxiaoxu99999@autohome.com.cn", "account_type": "个人账号", "status": "正常" },
  "columns": [{ "name": "stat_date", "type": "date", "comment": "统计日期", "nullable": false }],
  "partition_keys": ["stat_date"],
  "tags": ["销售", "日报", "指标"],
  "row_count": 12460,
  "size_bytes": 1048576,
  "update_time": "2026-11-14 09:20:00"
}
```

`items` 字段很全，先看 `full_name` 与 `columns`，需要时再展开。写 SQL 前**必须**先用它确认库表名与字段名，不要凭表名猜字段。

### 2. submit_query — 提交 SQL

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sql` | string | **是** | 必须是一条 SELECT |
| `engine` | string | 否 | 执行引擎，默认 `spark` |
| `wait_seconds` | int | 否 | 提交后阻塞等待的秒数，0 = 立即返回，上限 25 |

返回任务对象：

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
  "result_url": "<base>/api/v1/queries/<task_id>/result",
  "download_csv_url": "<base>/api/v1/queries/<task_id>/download?format=csv",
  "download_json_url": "<base>/api/v1/queries/<task_id>/download?format=json"
}
```

- **带上 `wait_seconds: 10`**：多数查询 1.5 秒内完成，一次调用就拿到终态，省掉轮询。不要用 25 这类上限值——等待再久也不会更快结束，只会拖长这次调用的耗时
- 任务执行失败时这次调用**直接 `isError=true`**，文本仍是任务对象（`status=failed` + `message`）。这是**执行失败，不是提交失败**，不要原样重试同一句 SQL
- `sql` 为空 → `isError=true`，文本 `sql 不能为空`

### 3. get_query_status — 查状态

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `task_id` | string | **是** | 提交时返回的任务 ID |
| `wait_seconds` | int | 否 | 阻塞等待秒数，0 = 立即返回，上限 25 |

状态机：`pending` → `running`（progress 10 / 35 / 60）→ `succeeded` | `failed`。

- `succeeded`：`row_count`、`duration_ms`、`message` 有值
- `failed`：`message` 是失败原因，没有结果可取
- 任务不存在 → `isError=true`，文本 `任务不存在：<id>`

### 4. get_query_result — 取结果（JSON）

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
  "columns": ["stat_date", "city", "order_cnt", "gmv"],
  "rows": [{ "stat_date": "2026-11-13", "city": "北京", "order_cnt": 4821, "gmv": 128930.5 }],
  "row_count": 5,
  "offset": 0,
  "has_more": false
}
```

- 任务**未结束不报错**：返回 `task_id` / `status` / `progress` / `message`，`isError=false`
- 任务**失败也不报错**：返回 `status=failed` + `message`，`columns` / `rows` 为空数组
- 所以拿到结果**先看 `status`**，不要只看有没有 `rows`
- 一次取 50 行足够阅读；确实需要更多再用 `offset` 翻页，不要一次 `limit: 200`

### 5. download_query_result — 下载文件

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
  "download_url": "<base>/api/v1/queries/<task_id>/download?format=csv",
  "preview": "stat_date,city\n2026-11-13,北京\n"
}
```

- 只有 `succeeded` 的任务能下载；未结束 / 失败 → `isError=true`
- 返回的是**完整结果集**的地址，不是分页预览；`preview` 只是前 5 行
- 这个链接需要带鉴权头访问，**你自己取不到内容**：把 `download_url` 原样交给用户，或先用 `get_query_result` 把数据读出来回答
- 单任务结果最多 200 行，下载与 `get_query_result` 是同一份数据

## 三、典型流程

```
search_tables(keyword="销售")                              # 1. 确认库表名与字段
submit_query(sql="select ... limit 100", wait_seconds=10)  # 2. 提交并等终态
   └─ status 仍 running：get_query_status(task_id, wait_seconds=10) 继续等
get_query_result(task_id)                                  # 3a. 取数据（可分页）
download_query_result(task_id, format="csv")               # 3b. 用户要文件时才用
```

判断终止条件，按顺序：

1. `status == succeeded` → `get_query_result` 或 `download_query_result`
2. `status == failed` → 把 `message` 原样告诉用户，不要重试同一句 SQL
3. `status` 是 `pending` / `running` → 带 `wait_seconds` 再等一次，最多重复 2~3 次；仍不结束就告诉用户任务还在跑并给出 `task_id`

省调用：`submit_query` 带 `wait_seconds` 能省掉一次 `get_query_status`；取数优先 `get_query_result`；同一句 SQL 不要反复提交。

## 四、可用的表（6 张）

| 全名 | 说明 | 分区 | 主要字段 |
|---|---|---|---|
| `dw.dw_order_detail` | 订单明细宽表，全渠道成交与退款订单 | `dt` | order_id, user_id, city, order_amount, order_status, channel, create_time |
| `dw.dw_user_profile` | 用户画像宽表，基础属性与关注车型 | `dt` | user_id, gender, age, city, register_date, is_new_user, prefer_series |
| `dm.dm_sales_daily` | 销售日报，按城市与日期汇总订单量、GMV、退款量 | `stat_date` | stat_date, city, order_cnt, gmv, avg_price, refund_cnt |
| `dm.dm_vehicle_visit` | 车型访问指标，PV / UV / 留资转化 | `stat_date` | stat_date, series_id, series_name, pv, uv, clue_cnt |
| `ods.ods_clue` | 线索流水，各渠道留资与跟进状态 | `dt` | clue_id, source, phone_masked, series_name, status, created_at |
| `dw.dim_city` | 城市维表，省份与大区归属 | 无 | city_code, city_name, province, region |

- 所有表负责人都是 **严小序（工号 99999，yanxiaoxu99999@autohome.com.cn）**，搜「严小序」或「99999」能列出全部表
- 可关联字段：`dw_order_detail.user_id` ↔ `dw_user_profile.user_id`；`dm_vehicle_visit.series_name` ↔ `ods_clue.series_name`；`city` 与 `city_name` 是同城异名

## 五、SQL 的真实行为

只解析 SQL 的一小部分，按下面的规则预期结果：

| 写法 | 实际行为 |
|---|---|
| `select a, b from t` | 返回 `t` 的**全部列**，`select` 列表被忽略 |
| `where` / `group by` / `order by` / 聚合函数 | **不生效**，返回样例行 |
| `limit N` | **生效**：返回 N 行（默认 5，上限 200） |
| `from a join b` | 只看第一个 `from` / `join` 后的表，join 不执行 |
| 带库名、双引号 | 支持 `dw.dim_city`、`"dw.dim_city"`；**不要用反引号**，解析不到表名会退化成 `result=1` |
| 没有 `from`（如 `select 1`） | 返回单列 `result`，值 `1` |
| insert / update / delete / drop / create / truncate / alter / merge 开头 | 失败：`当前账号为只读账号，不允许执行 DDL/DML，请只提交 SELECT 查询` |
| SQL 中出现 `fail` / `error` / `boom` | 故意失败（用于验证异常分支） |
| 表名不存在 | 失败：`Table or view not found: xxx` |

行内容是样例行**循环填充**到 `limit` 行，因此：

- 行数不等于真实数据量，**不要据此下「共 N 条记录」的结论**
- 大 `limit` 会出现重复行，去重后更少是正常现象
- 想要「真实感」用 `limit 5`~`20`，不要写 `limit 1000`（会被截到 200）

**仍然写标准 Spark SQL**（`select ... from 库.表 where 分区条件 limit N`），换成真实数仓时不用重写；只是要清楚 mock 不校验语义。

## 六、失败形态

| 场景 | 表现 |
|---|---|
| `sql` 为空、`format` 非法、参数不是 JSON 对象 | `isError=true` + 原因文本 |
| 任务不存在 | `isError=true` + `任务不存在：<id>` |
| `submit_query` 执行失败 | `isError=true` + 任务对象（`status=failed`） |
| 任务未结束就下载 | `isError=true` + `当前状态：<status>` |
| 失败任务下载 | `isError=true` + `任务执行失败，无结果可下载：<message>` |
| 任务未结束就取结果 | `isError=false`，`status` / `progress` 有值 |
| 失败任务取结果 | `isError=false`，`status=failed` + 空 `columns` / `rows` |

`isError=true` 时读文本里的 `message`，按原因纠正，**不要当网络错误盲目重试**。

## 七、回答口径

- 明确说明数据来自 **mock 数据集**，不是生产数仓
- 给出表名 + SQL + 行数时，行数是「本次返回行数」，不是表总量（表总量看元信息里的 `row_count`）
- 不要编造 `task_id`，也不要把上一次任务的数字套到新问题上
- 需求超出能力（真实聚合、导出全量、跨库血缘）时直接说明边界，并给可行替代：换 `limit`、换表、找负责人严小序
