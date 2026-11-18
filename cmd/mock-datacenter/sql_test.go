package main

import (
	"fmt"
	"math"
	"testing"
)

func mustQuery(t *testing.T, sql string) ([]string, []map[string]any) {
	t.Helper()
	cols, rows, err := executeSQL(sql)
	if err != nil {
		t.Fatalf("%s 执行失败：%v", sql, err)
	}
	return cols, rows
}

func TestCountMatchesRowCount(t *testing.T) {
	_, rows := mustQuery(t, `select count(*) as cnt from dw_order_detail`)
	got := rows[0]["cnt"].(int64)
	table := findTable("dw_order_detail")
	if got != table.RowCount {
		t.Fatalf("count(*) = %d，元数据 row_count = %d，两者必须一致", got, table.RowCount)
	}
}

func TestWhereAndGroupBy(t *testing.T) {
	_, rows := mustQuery(t, `select city_code, count(*) as cnt from dw_order_detail
		where city = '北京市' group by city_code`)
	if len(rows) != 1 {
		t.Fatalf("北京应只有一个城市 code，实际 %d 行", len(rows))
	}
	if rows[0]["city_code"] != "110100" {
		t.Fatalf("北京 code 应为 110100，实际 %v", rows[0]["city_code"])
	}
	if rows[0]["cnt"].(int64) <= 0 {
		t.Fatal("北京订单数应大于 0")
	}
}

func TestInAndBetween(t *testing.T) {
	_, rows := mustQuery(t, `select count(*) as cnt from dw_order_detail
		where city_code in ('110100', '310100') and order_amount between 100000 and 200000`)
	cnt := rows[0]["cnt"].(int64)
	if cnt <= 0 {
		t.Fatal("in + between 应能筛出数据，实际 0 行——筛选条件没生效")
	}

	// 逐行核对，避免「聚合函数自己算错」被当成通过
	table := findTable("dw_order_detail")
	var expect int64
	for _, r := range table.rows {
		code := fmt.Sprint(r["city_code"])
		amount, _ := toNumber(r["order_amount"])
		if (code == "110100" || code == "310100") && amount >= 100000 && amount <= 200000 {
			expect++
		}
	}
	if cnt != expect {
		t.Fatalf("SQL 计数 %d，逐行核对 %d", cnt, expect)
	}
}

func TestJoinDimension(t *testing.T) {
	_, rows := mustQuery(t, `select c.region, count(*) as cnt
		from dw_order_detail o join dim_city c on o.city_code = c.city_code
		group by c.region`)
	if len(rows) < 5 {
		t.Fatalf("按大区聚合应覆盖多个大区，实际 %d 行", len(rows))
	}
	var total int64
	for _, r := range rows {
		total += r["cnt"].(int64)
	}
	// join 后不应丢行（城市维度里每个 fact 城市都存在）
	if want := findTable("dw_order_detail").RowCount; total != want {
		t.Fatalf("join 后总行数 %d，明细表 %d", total, want)
	}
}

func TestOrderByLimit(t *testing.T) {
	_, rows := mustQuery(t, `select order_id, order_amount from dw_order_detail
		order by order_amount desc limit 3`)
	if len(rows) != 3 {
		t.Fatalf("limit 3 应返回 3 行，实际 %d", len(rows))
	}
	a, _ := toNumber(rows[0]["order_amount"])
	b, _ := toNumber(rows[1]["order_amount"])
	if a < b {
		t.Fatal("order by desc 排序不对")
	}
}

func TestUnknownColumn(t *testing.T) {
	if _, _, err := executeSQL(`select not_exist_col from dw_order_detail where 1=1`); err == nil {
		t.Fatal("未知字段应报错，而不是静默返回空表")
	}
}

func TestRejectWrite(t *testing.T) {
	if _, _, err := executeSQL(`delete from dw_order_detail`); err == nil {
		t.Fatal("写操作必须拒绝")
	}
}

func TestFactAndDailyConsistent(t *testing.T) {
	orders := findTable("dw_order_detail").rows
	var want float64
	for _, r := range orders {
		if r["order_status"] == "paid" {
			v, _ := toNumber(r["order_amount"])
			want += v
		}
	}
	_, rows := mustQuery(t, `select sum(gmv) as gmv from dm_sales_daily`)
	got, _ := toNumber(rows[0]["gmv"])
	// 日报按日期+城市分组后各自 round2，累加会有分位误差
	if math.Abs(got-want) > 1 {
		t.Fatalf("日报 gmv 合计 %.2f 与明细 %.2f 不一致——派生数据不自治", got, want)
	}
}
