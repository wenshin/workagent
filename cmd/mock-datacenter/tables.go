package main

import (
	"strings"
	"time"
)

// Column 是表的字段元信息。
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Comment  string `json:"comment"`
	Nullable bool   `json:"nullable"`
}

// Owner 是表的维护人。字段按大数据平台「表负责人」维度返回，
// 与平台上的人事信息对齐（姓名 / 账号类型 / 状态 / 工号 / 邮箱 / 账号 ID）。
type Owner struct {
	Name        string `json:"name"`
	AccountType string `json:"account_type"`
	Status      string `json:"status"`
	JobNumber   string `json:"job_number"`
	Email       string `json:"email"`
	AccountID   string `json:"account_id"`
}

// Table 是返回给调用方的表元信息；rows 是 mock 的样例行，不对外序列化。
type Table struct {
	Database      string   `json:"database"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Description   string   `json:"description"`
	Layer         string   `json:"layer"`
	Owner         Owner    `json:"owner"`
	Columns       []Column `json:"columns"`
	PartitionKeys []string `json:"partition_keys,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	RowCount      int64    `json:"row_count"`
	SizeBytes     int64    `json:"size_bytes"`
	UpdateTime    string   `json:"update_time"`

	rows []map[string]any
}

// ownerYan 是需求指定的表维护人。
var ownerYan = Owner{
	Name:        "严小序",
	AccountType: "个人账号",
	Status:      "正常",
	JobNumber:   "99999",
	Email:       "yanxiaoxu99999@autohome.com.cn",
	AccountID:   "03651554640120054733",
}

// mockTables 返回全部 mock 表。数据是固定的，便于断言；
// 只有 update_time 按当前时间回推，看起来像真的在更新。
func mockTables() []*Table {
	now := time.Now()
	ago := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02 15:04:05") }

	return []*Table{
		{
			Database: "dw", Name: "dw_order_detail", FullName: "dw.dw_order_detail",
			Description: "订单明细宽表，按下单时间分区，覆盖全渠道成交与退款订单",
			Layer:       "DW", Owner: ownerYan,
			PartitionKeys: []string{"dt"},
			Tags:          []string{"订单", "交易", "核心表"},
			RowCount:      186_420_913, SizeBytes: 42_610_664_704,
			UpdateTime: ago(3 * time.Hour),
			Columns: []Column{
				{Name: "order_id", Type: "string", Comment: "订单号"},
				{Name: "user_id", Type: "bigint", Comment: "用户ID"},
				{Name: "city", Type: "string", Comment: "城市"},
				{Name: "order_amount", Type: "decimal(18,2)", Comment: "订单金额（元）"},
				{Name: "order_status", Type: "string", Comment: "订单状态：paid/refunded/closed"},
				{Name: "channel", Type: "string", Comment: "下单渠道"},
				{Name: "create_time", Type: "timestamp", Comment: "下单时间"},
			},
			rows: []map[string]any{
				{"order_id": "A20261113001", "user_id": int64(1002341), "city": "北京", "order_amount": 12.8, "order_status": "paid", "channel": "APP", "create_time": "2026-11-13 09:12:31"},
				{"order_id": "A20261113002", "user_id": int64(1002342), "city": "上海", "order_amount": 26.5, "order_status": "paid", "channel": "小程序", "create_time": "2026-11-13 09:31:02"},
				{"order_id": "A20261113003", "user_id": int64(1002343), "city": "广州", "order_amount": 8.8, "order_status": "refunded", "channel": "APP", "create_time": "2026-11-13 10:04:55"},
				{"order_id": "A20261113004", "user_id": int64(1002344), "city": "成都", "order_amount": 33.0, "order_status": "paid", "channel": "H5", "create_time": "2026-11-13 10:47:20"},
				{"order_id": "A20261113005", "user_id": int64(1002345), "city": "杭州", "order_amount": 15.6, "order_status": "closed", "channel": "APP", "create_time": "2026-11-13 11:22:08"},
				{"order_id": "A20261113006", "user_id": int64(1002346), "city": "深圳", "order_amount": 41.2, "order_status": "paid", "channel": "小程序", "create_time": "2026-11-13 12:05:44"},
			},
		},
		{
			Database: "dw", Name: "dw_user_profile", FullName: "dw.dw_user_profile",
			Description: "用户画像宽表，包含基础属性、注册信息与关注车型",
			Layer:       "DW", Owner: ownerYan,
			PartitionKeys: []string{"dt"},
			Tags:          []string{"用户", "画像"},
			RowCount:      52_884_120, SizeBytes: 9_342_177_280,
			UpdateTime: ago(5 * time.Hour),
			Columns: []Column{
				{Name: "user_id", Type: "bigint", Comment: "用户ID"},
				{Name: "gender", Type: "string", Comment: "性别：M/F/U"},
				{Name: "age", Type: "int", Comment: "年龄"},
				{Name: "city", Type: "string", Comment: "常驻城市"},
				{Name: "register_date", Type: "date", Comment: "注册日期"},
				{Name: "is_new_user", Type: "boolean", Comment: "是否新用户"},
				{Name: "prefer_series", Type: "string", Comment: "关注车型"},
			},
			rows: []map[string]any{
				{"user_id": int64(1002341), "gender": "M", "age": 32, "city": "北京", "register_date": "2024-03-11", "is_new_user": false, "prefer_series": "奥迪A4L"},
				{"user_id": int64(1002342), "gender": "F", "age": 28, "city": "上海", "register_date": "2025-07-02", "is_new_user": true, "prefer_series": "宝马3系"},
				{"user_id": int64(1002343), "gender": "M", "age": 41, "city": "广州", "register_date": "2023-11-19", "is_new_user": false, "prefer_series": "奔驰C级"},
				{"user_id": int64(1002344), "gender": "U", "age": 24, "city": "成都", "register_date": "2026-01-08", "is_new_user": true, "prefer_series": "特斯拉Model 3"},
				{"user_id": int64(1002345), "gender": "F", "age": 36, "city": "杭州", "register_date": "2022-05-30", "is_new_user": false, "prefer_series": "比亚迪汉"},
				{"user_id": int64(1002346), "gender": "M", "age": 45, "city": "深圳", "register_date": "2021-09-14", "is_new_user": false, "prefer_series": "理想L7"},
			},
		},
		{
			Database: "dm", Name: "dm_sales_daily", FullName: "dm.dm_sales_daily",
			Description: "销售日报，按城市与统计日期汇总订单量、GMV 与退款量",
			Layer:       "DM", Owner: ownerYan,
			PartitionKeys: []string{"stat_date"},
			Tags:          []string{"销售", "日报", "指标"},
			RowCount:      12_460, SizeBytes: 1_048_576,
			UpdateTime: ago(90 * time.Minute),
			Columns: []Column{
				{Name: "stat_date", Type: "date", Comment: "统计日期"},
				{Name: "city", Type: "string", Comment: "城市"},
				{Name: "order_cnt", Type: "bigint", Comment: "订单量"},
				{Name: "gmv", Type: "decimal(18,2)", Comment: "成交总额（元）"},
				{Name: "avg_price", Type: "decimal(18,2)", Comment: "客单价（元）"},
				{Name: "refund_cnt", Type: "bigint", Comment: "退款单量"},
			},
			rows: []map[string]any{
				{"stat_date": "2026-11-13", "city": "北京", "order_cnt": int64(4821), "gmv": 128_930.5, "avg_price": 26.74, "refund_cnt": int64(61)},
				{"stat_date": "2026-11-13", "city": "上海", "order_cnt": int64(5187), "gmv": 141_208.0, "avg_price": 27.22, "refund_cnt": int64(74)},
				{"stat_date": "2026-11-13", "city": "广州", "order_cnt": int64(3264), "gmv": 84_512.3, "avg_price": 25.89, "refund_cnt": int64(52)},
				{"stat_date": "2026-11-13", "city": "成都", "order_cnt": int64(2410), "gmv": 60_882.9, "avg_price": 25.26, "refund_cnt": int64(33)},
				{"stat_date": "2026-11-13", "city": "杭州", "order_cnt": int64(1985), "gmv": 52_771.4, "avg_price": 26.58, "refund_cnt": int64(28)},
			},
		},
		{
			Database: "ods", Name: "ods_clue", FullName: "ods.ods_clue",
			Description: "线索流水表，记录各渠道留资线索与跟进状态",
			Layer:       "ODS", Owner: ownerYan,
			PartitionKeys: []string{"dt"},
			Tags:          []string{"线索", "留资", "增长"},
			RowCount:      8_214_660, SizeBytes: 2_147_483_648,
			UpdateTime: ago(20 * time.Minute),
			Columns: []Column{
				{Name: "clue_id", Type: "string", Comment: "线索ID"},
				{Name: "source", Type: "string", Comment: "来源渠道"},
				{Name: "phone_masked", Type: "string", Comment: "脱敏手机号"},
				{Name: "series_name", Type: "string", Comment: "意向车型"},
				{Name: "status", Type: "string", Comment: "线索状态：new/following/converted/invalid"},
				{Name: "created_at", Type: "timestamp", Comment: "创建时间"},
			},
			rows: []map[string]any{
				{"clue_id": "C-20261113-0001", "source": "落地页", "phone_masked": "138****6621", "series_name": "奥迪A4L", "status": "new", "created_at": "2026-11-13 08:01:12"},
				{"clue_id": "C-20261113-0002", "source": "APP", "phone_masked": "139****1043", "series_name": "宝马3系", "status": "following", "created_at": "2026-11-13 08:45:39"},
				{"clue_id": "C-20261113-0003", "source": "小程序", "phone_masked": "137****8890", "series_name": "奔驰C级", "status": "converted", "created_at": "2026-11-13 09:30:07"},
				{"clue_id": "C-20261113-0004", "source": "电话", "phone_masked": "135****2277", "series_name": "比亚迪汉", "status": "invalid", "created_at": "2026-11-13 10:12:51"},
				{"clue_id": "C-20261113-0005", "source": "落地页", "phone_masked": "136****9014", "series_name": "理想L7", "status": "new", "created_at": "2026-11-13 11:03:26"},
				{"clue_id": "C-20261113-0006", "source": "APP", "phone_masked": "133****5568", "series_name": "特斯拉Model 3", "status": "following", "created_at": "2026-11-13 11:58:40"},
			},
		},
		{
			Database: "dm", Name: "dm_vehicle_visit", FullName: "dm.dm_vehicle_visit",
			Description: "车型访问指标，按日统计车型页 PV / UV 与留资转化",
			Layer:       "DM", Owner: ownerYan,
			PartitionKeys: []string{"stat_date"},
			Tags:          []string{"流量", "车型", "指标"},
			RowCount:      96_320, SizeBytes: 12_582_912,
			UpdateTime: ago(2 * time.Hour),
			Columns: []Column{
				{Name: "stat_date", Type: "date", Comment: "统计日期"},
				{Name: "series_id", Type: "bigint", Comment: "车系ID"},
				{Name: "series_name", Type: "string", Comment: "车系名称"},
				{Name: "pv", Type: "bigint", Comment: "页面浏览量"},
				{Name: "uv", Type: "bigint", Comment: "独立访客数"},
				{Name: "clue_cnt", Type: "bigint", Comment: "留资数"},
			},
			rows: []map[string]any{
				{"stat_date": "2026-11-13", "series_id": int64(10231), "series_name": "奥迪A4L", "pv": int64(182_334), "uv": int64(96_112), "clue_cnt": int64(824)},
				{"stat_date": "2026-11-13", "series_id": int64(10245), "series_name": "宝马3系", "pv": int64(210_889), "uv": int64(108_442), "clue_cnt": int64(961)},
				{"stat_date": "2026-11-13", "series_id": int64(10260), "series_name": "奔驰C级", "pv": int64(163_027), "uv": int64(88_305), "clue_cnt": int64(712)},
				{"stat_date": "2026-11-13", "series_id": int64(10318), "series_name": "比亚迪汉", "pv": int64(245_116), "uv": int64(131_780), "clue_cnt": int64(1_203)},
				{"stat_date": "2026-11-13", "series_id": int64(10377), "series_name": "理想L7", "pv": int64(198_450), "uv": int64(101_226), "clue_cnt": int64(887)},
			},
		},
		{
			Database: "dw", Name: "dim_city", FullName: "dw.dim_city",
			Description: "城市维表，含省份与大区归属，用于报表维度关联",
			Layer:       "DIM", Owner: ownerYan,
			Tags:     []string{"维表", "城市", "维度"},
			RowCount: 1_846, SizeBytes: 262_144,
			UpdateTime: ago(26 * time.Hour),
			Columns: []Column{
				{Name: "city_code", Type: "string", Comment: "城市编码"},
				{Name: "city_name", Type: "string", Comment: "城市名称"},
				{Name: "province", Type: "string", Comment: "所属省份"},
				{Name: "region", Type: "string", Comment: "所属大区"},
			},
			rows: []map[string]any{
				{"city_code": "110100", "city_name": "北京", "province": "北京", "region": "华北"},
				{"city_code": "310100", "city_name": "上海", "province": "上海", "region": "华东"},
				{"city_code": "440100", "city_name": "广州", "province": "广东", "region": "华南"},
				{"city_code": "510100", "city_name": "成都", "province": "四川", "region": "西南"},
				{"city_code": "330100", "city_name": "杭州", "province": "浙江", "region": "华东"},
				{"city_code": "440300", "city_name": "深圳", "province": "广东", "region": "华南"},
			},
		},
	}
}

// matchTable 按 kw 模糊匹配（表名 / 中文名 / 描述 / 标签 / 负责人），空 kw 全命中。
func matchTable(t *Table, kw string) bool {
	if kw == "" {
		return true
	}
	kw = strings.ToLower(kw)
	haystack := []string{
		t.FullName, t.Name, t.Database, t.Description, t.Layer,
		t.Owner.Name, t.Owner.JobNumber, t.Owner.Email,
		strings.Join(t.Tags, " "),
	}
	if t.PartitionKeys != nil {
		haystack = append(haystack, strings.Join(t.PartitionKeys, " "))
	}
	for _, c := range t.Columns {
		haystack = append(haystack, c.Name, c.Comment)
	}
	for _, s := range haystack {
		if strings.Contains(strings.ToLower(s), kw) {
			return true
		}
	}
	return false
}

// findTable 按全名/短名查找表，SQL 解析与结果生成共用。
func findTable(name string) *Table {
	name = strings.ToLower(strings.Trim(name, "`\"' "))
	if name == "" {
		return nil
	}
	for _, t := range mockTables() {
		if strings.ToLower(t.FullName) == name || strings.ToLower(t.Name) == name {
			return t
		}
	}
	// 允许只写库名之外的别名，如 dw_order_detail 带库名前缀不完整的情况
	for _, t := range mockTables() {
		if strings.HasSuffix(strings.ToLower(t.FullName), "."+name) {
			return t
		}
	}
	return nil
}
