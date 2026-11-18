# Mock 大数据平台 · MCP 工具说明

你通过 7 个 MCP 工具访问一个**模拟**数仓：找表 → 查维度 → 提交 Spark SQL → 查状态 → 取结果 / 下载文件。

SQL 是**真的执行**的：`where` / `group by` / `join` / 聚合 / 排序 / 分页都生效，结果与维度表自洽（`count(*)` 等于元信息里的 `row_count`）。但数据量是造出来的（几百到几千行），结论里必须说明数据来自 mock，不要把行数当成生产规模。

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

## 二、工具

### 1. dim_list — 有哪些维度

无参数。返回维度清单，用来确认**取值字段名**，不要凭记忆猜：

```json
{
  "items": [
    {
      "dim": "city",
      "label": "城市",
      "table": "dim_city",
      "value_field": "city_code",
      "label_field": "city_name",
      "extra_fields": ["province", "region", "city_level"],
      "cardinality": 309,
      "note": "按城市名称或省份搜索；value 是行政区划码"
    }
  ],
  "total": 4
}
```

### 2. dim_lookup — 名称 ↔ id

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `dim` | string | **是** | `city` / `store` / `series` / `channel` |
| `keyword` | string | 否 | 名称关键字，例如 `北京`、`比亚迪`、`旗舰` |
| `parent` | string | 否 | 级联过滤，查门店时传城市 code |
| `values` | array | 否 | 按 id 批量反查名称 |
| `limit` | int | 否 | 返回条数，默认 20，最大 200 |

```json
{
  "dim": "city",
  "items": [{ "value": "110100", "label": "北京市", "extra": { "province": "北京市", "region": "华北", "city_level": "一线" } }],
  "total": 2,
  "has_more": false,
  "limit": 20
}
```

- `value` 一律是**字符串**（行政区划码可能带前导零），拼 SQL 时按字符串加引号
- `has_more=true` 说明候选被截断，加 `keyword` 收窄，不要用大 `limit` 硬拉
- 完全匹配会排在前面（搜「北京市」不会先出现「北京市某某店」）

### 3. search_tables — 找表

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `keyword` | string | 否 | 关键字，为空时列出全部表 |
| `database` | string | 否 | 限定库名：`dw` / `dm` / `ods` / `dim` |
| `page` | int | 否 | 页码，默认 1 |
| `page_size` | int | 否 | 每页条数，默认 20，最大 100 |

`keyword` 匹配范围：表全名、表名、库名、描述、数仓分层、标签、分区字段、**字段名与字段注释**、负责人姓名 / 工号 / 邮箱。

返回 `{ total, page, page_size, items[], caller }`，item 含 `full_name` / `columns` / `partition_keys` / `row_count` / `owner` 等。写 SQL 前**必须**先用它确认库表名与字段名。

### 4. submit_query — 提交 SQL

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sql` | string | **是** | 必须是一条 SELECT |
| `engine` | string | 否 | 执行引擎，默认 `spark` |
| `wait_seconds` | int | 否 | 提交后阻塞等待的秒数，0 = 立即返回，上限 25 |

返回任务对象（`task_id` / `status` / `columns` / `row_count` / `message` / 下载地址）。

- **带上 `wait_seconds: 10`**：多数查询 1.5 秒内完成，一次调用就拿到终态
- 执行失败时本次调用 `isError=true`，文本里是 `status=failed` + `message`（**执行失败，不是提交失败**），不要原样重试同一句 SQL
- 不写 `limit` 时默认返回 50 行，单任务上限 200 行

### 5. get_query_status — 查状态

状态机：`pending` → `running`（progress 10 / 35 / 60）→ `succeeded` | `failed`。`wait_seconds` 可阻塞等待，上限 25。

### 6. get_query_result — 取结果（JSON）

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `task_id` | string | **是** | 任务 ID |
| `offset` | int | 否 | 起始行号，默认 0 |
| `limit` | int | 否 | 返回行数，默认 50，最大 200 |

- 任务**未结束不报错**：返回 `status` / `progress`
- 任务**失败也不报错**：返回 `status=failed` + 空 `columns` / `rows`
- 拿到结果**先看 `status`**

### 7. download_query_result — 下载文件

只有 `succeeded` 的任务能下载；返回 `download_url` 与前 5 行 `preview`。链接需要鉴权头，**你自己取不到内容**：把地址交给用户，或先用 `get_query_result` 读数据。

## 三、典型流程

```
dim_list()                                                  # 需要按城市/门店/车系筛时先看字段
dim_lookup(dim="city", keyword="北京")                       # 名称 → id
search_tables(keyword="销售")                                # 确认表与字段
submit_query(sql="select ... limit 100", wait_seconds=10)    # 提交并等终态
   └─ status 仍 running：get_query_status(task_id, wait_seconds=10)
get_query_result(task_id)                                    # 取数据
download_query_result(task_id, format="csv")                 # 用户要文件时才用
```

## 四、筛选条件：先问清，再查

用户说「查一下华东上个月的销售」时，**不要自己猜城市 code、不要用中文名硬拼 where**：

1. `dim_lookup` 把名称换成 id（「华东」是 `region`，走 `dim_city` 先取该大区下的城市 code；「上个月」换成 `dt between 'YYYY-MM-01' and 'YYYY-MM-31'`）
2. 缺条件就渲染筛选组件（` ```ui-block {"slug":"...","props":{...}} `），把已推断出的条件用 `props` 预填，不要渲染一个全空的表单
3. 用户在消息里回传 ` ```filter-params ` 后，**里面的 `*_codes` 原样用于 SQL**，不要改写、不要再用名称查一遍；`*_names` 只用于复述
4. 条件齐全时直接查，不要再弹组件

维度与事实表的对应关系：

| 维度 | 事实表字段 | 维度表 |
|---|---|---|
| 城市 | `city_code` | `dim_city.city_code` |
| 门店 | `store_code` | `dim_store.store_code` |
| 车系 | `series_id` | `dim_series.series_id` |
| 渠道 | `channel_code` | `dim_channel.channel_code` |

## 五、可用的表（9 张）

| 全名 | 说明 | 分区 | 主要字段 |
|---|---|---|---|
| `dim.dim_city` | 城市维表（300+ 个城市） | 无 | city_code, city_name, province, region, city_level |
| `dim.dim_store` | 门店维表（900+ 家） | 无 | store_code, store_name, city_code, city_name, channel, open_date, status |
| `dim.dim_series` | 车系维表（62 个车系） | 无 | series_id, series_name, brand, energy_type, price_band |
| `dim.dim_channel` | 渠道维表 | 无 | channel_code, channel_name, channel_type |
| `dw.dw_order_detail` | 订单明细宽表 | `dt` | order_id, user_id, city_code, city, store_code, series_id, series_name, order_amount, discount_amount, order_status, channel_code, channel, create_time, dt |
| `dw.dw_user_profile` | 用户画像宽表 | 无 | user_id, gender, age, city_code, city, register_date, is_new_user, prefer_series_id, prefer_series |
| `dm.dm_sales_daily` | 销售日报（按日期+城市，由订单派生） | `dt` | dt, city_code, city, order_cnt, gmv, avg_price, refund_cnt |
| `dm.dm_vehicle_visit` | 车系流量日报（30 天 × 62 车系） | `dt` | dt, series_id, series_name, pv, uv, clue_cnt |
| `ods.ods_clue` | 线索明细 | 无 | clue_id, series_id, series_name, city_code, city, source, status, created_at |

- 所有表负责人都是 **严小序（工号 99999，yanxiaoxu99999@autohome.com.cn）**
- 可关联：`dw_order_detail.city_code` ↔ `dim_city.city_code`、`dw_order_detail.store_code` ↔ `dim_store.store_code`、`dw_order_detail.series_id` ↔ `dim_series.series_id`
- `dm_sales_daily` 由 `dw_order_detail` 聚合而来：两边做交叉验证时 GMV 应基本一致（日报按城市、日期分组后各自 round2，允许分位误差）

## 六、SQL 的真实行为

支持范围（够跑通分析链路，不做完备实现）：

| 写法 | 实际行为 |
|---|---|
| `select a, b / *` | **按列表裁剪**；`*` 返回主表全部列 |
| `where` + `and/or/not`、`= != > >= < <=` | **生效** |
| `in (...)`、`between ... and ...`、`like '%x%'`、`is [not] null` | **生效** |
| `group by` + `count/sum/avg/min/max` | **生效**，支持 `as` 别名 |
| `order by ... [asc|desc]`、`limit n [offset m]` | **生效**，`limit` 缺省 50，上限 200 |
| `[inner|left] join 表 on a.x = b.y` | **生效**（单条件等值连接） |
| `distinct` | 生效 |
| 子查询 / 窗口函数 / `case when` / `having` | **不支持**，返回语法错误 |
| 没有 `from`（如 `select 1`） | 返回单列 `result`，值 `1` |
| insert / update / delete / drop / create / truncate / alter / merge 开头 | 失败：只读账号 |
| SQL 中出现 `fail` / `error` / `boom` | 故意失败（用于验证异常分支） |
| 表名 / 字段名不存在 | 失败：`Table or view not found: xxx` / `Unknown column 'xxx'` |

仍然写标准 Spark SQL（`select ... from 库.表 where ... group by ...`），换成真实数仓时不用重写。

## 七、失败形态

| 场景 | 表现 |
|---|---|
| `sql` 为空、`dim` 不存在、参数不是 JSON 对象 | `isError=true` + 原因文本 |
| 任务不存在 | `isError=true` + `任务不存在：<id>` |
| `submit_query` 执行失败 | `isError=true` + 任务对象（`status=failed`） |
| 任务未结束就取结果 | `isError=false`，`status` / `progress` 有值 |
| 失败任务取结果 | `isError=false`，`status=failed` + 空结果 |

`isError=true` 时读文本里的 `message`，按原因纠正（改字段名、改表名、去掉不支持的语法），**不要当网络错误盲目重试**。

## 八、回答口径

- 明确说明数据来自 **mock 数据集**，不是生产数仓
- 给出「共 N 条」时，先确认是 `count(*)` 的结果，不要拿本次返回行数当总数
- 不要编造 `task_id`，也不要把上一次任务的数字套到新问题上
- 需求超出能力（子查询、窗口函数、导出全量）时直接说明边界，并给可行替代：拆成两步查询、换表、找负责人严小序
