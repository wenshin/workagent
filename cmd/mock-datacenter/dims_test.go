package main

import (
	"fmt"
	"testing"
)

func lookup(t *testing.T, dim, keyword, parent string, values []string, limit int) map[string]any {
	t.Helper()
	out, err := dimensionLookup(dim, keyword, parent, values, limit)
	if err != nil {
		t.Fatalf("dim_lookup(%s) 失败：%v", dim, err)
	}
	return out
}

func TestCityLookupExactFirst(t *testing.T) {
	out := lookup(t, "city", "北京市", "", nil, 20)
	items := out["items"].([]map[string]any)
	if len(items) == 0 {
		t.Fatal("按名称搜索城市应有结果")
	}
	if items[0]["label"] != "北京市" || items[0]["value"] != "110100" {
		t.Fatalf("完全匹配应排第一，实际 %v", items[0])
	}
}

func TestCityLookupHasMore(t *testing.T) {
	out := lookup(t, "city", "", "", nil, 5)
	if len(out["items"].([]map[string]any)) != 5 {
		t.Fatal("limit 应生效")
	}
	if out["has_more"] != true {
		t.Fatal("候选多于 limit 时 has_more 必须为 true")
	}
	if out["total"].(int) <= 200 {
		t.Fatalf("城市维度作为搜索型下拉需要足够基数，实际 total=%v", out["total"])
	}
}

func TestStoreCascadeByParent(t *testing.T) {
	out := lookup(t, "store", "", "110100", nil, 200)
	items := out["items"].([]map[string]any)
	if len(items) == 0 {
		t.Fatal("按城市级联查门店应有结果")
	}
	for _, item := range items {
		extra := item["extra"].(map[string]any)
		if extra["city_code"] != "110100" {
			t.Fatalf("级联过滤失效，出现其他城市门店：%v", item)
		}
	}
}

func TestSeriesByBrandAndValues(t *testing.T) {
	out := lookup(t, "series", "比亚迪", "", nil, 20)
	if len(out["items"].([]map[string]any)) < 3 {
		t.Fatal("按品牌搜索应命中多个车系")
	}
	first := out["items"].([]map[string]any)[0]
	id := first["value"].(string)
	back := lookup(t, "series", "", "", []string{id}, 10)
	items := back["items"].([]map[string]any)
	if len(items) != 1 || items[0]["label"] != first["label"] {
		t.Fatalf("按 id 反查名称失败：%v", back)
	}
}

func TestUnknownDimension(t *testing.T) {
	if _, err := dimensionLookup("nope", "", "", nil, 10); err == nil {
		t.Fatal("未知维度应报错并提示可用维度")
	}
}

func TestDimensionValueIsString(t *testing.T) {
	out := lookup(t, "store", "", "", nil, 5)
	for _, item := range out["items"].([]map[string]any) {
		if _, ok := item["value"].(string); !ok {
			t.Fatalf("value 必须是字符串（编码可能带前导零）：%v", item)
		}
	}
}

func TestDimensionListFields(t *testing.T) {
	out := dimensionList()
	items := out["items"].([]map[string]any)
	seen := map[string]bool{}
	for _, item := range items {
		seen[fmt.Sprint(item["dim"])] = true
		if item["value_field"] == "" || item["label_field"] == "" {
			t.Fatalf("维度清单必须给出取值/展示字段：%v", item)
		}
	}
	for _, want := range []string{"city", "store", "series"} {
		if !seen[want] {
			t.Fatalf("维度清单缺少 %s", want)
		}
	}
}
