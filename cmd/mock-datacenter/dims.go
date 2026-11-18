package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/workagent/auto-agent/internal/platform"
)

// 维度查询接口。
//
// 分析类智能体最常见的动作是「用户说的是名称，SQL 要的是 id」：城市、门店、
// 车系都是这种关系。这里提供一个统一的取值接口，让筛选组件和智能体共用同一份
// 映射——两处各查一次，迟早会出现「下拉里有的城市 SQL 查不到」。
//
// 契约（组件 options_from 直接吃这个形状）：
//
//	{"dim":"city","items":[{"value":"110100","label":"北京市","extra":{...}}],
//	 "total":313,"has_more":false}
//
// value 一律是字符串：行政区划码、门店编码都可能带前导零，转成数字就丢了。

type dimensionDef struct {
	ID        string   // 维度标识，同时也是 dim_lookup 的 dim 参数
	Label     string   // 展示名
	Table     string   // 落在哪张维表
	ValueCol  string   // 取值字段（提交给 SQL 的那一列）
	LabelCol  string   // 展示字段
	SearchCol []string // 关键字命中这些列
	ExtraCols []string // 额外返回的字段
	ParentCol string   // 级联过滤列（如 store.city_code）
	ParentDim string   // 父维度（如 city）
	Note      string
}

var dimensionDefs = []dimensionDef{
	{
		ID: "city", Label: "城市", Table: "dim_city",
		ValueCol: "city_code", LabelCol: "city_name",
		SearchCol: []string{"city_name", "province", "city_code"},
		ExtraCols: []string{"province", "region", "city_level"},
		Note:      "按城市名称或省份搜索；value 是行政区划码",
	},
	{
		ID: "store", Label: "门店", Table: "dim_store",
		ValueCol: "store_code", LabelCol: "store_name",
		SearchCol: []string{"store_name", "city_name", "store_code"},
		ExtraCols: []string{"city_code", "city_name", "channel", "status"},
		ParentCol: "city_code", ParentDim: "city",
		Note: "可按 parent（城市 code）级联过滤；value 是门店编码",
	},
	{
		ID: "series", Label: "车系", Table: "dim_series",
		ValueCol: "series_id", LabelCol: "series_name",
		SearchCol: []string{"series_name", "brand"},
		ExtraCols: []string{"brand", "energy_type", "price_band"},
		Note:      "按车系名或品牌搜索；value 是数字 id（返回时已转字符串）",
	},
	{
		ID: "channel", Label: "渠道", Table: "dim_channel",
		ValueCol: "channel_code", LabelCol: "channel_name",
		SearchCol: []string{"channel_name", "channel_code"},
		ExtraCols: []string{"channel_type"},
		Note:      "枚举很稳定，组件也可以直接写死字面量 options",
	},
}

const (
	dimDefaultLimit = 20
	dimMaxLimit     = 200
)

func findDimension(id string) *dimensionDef {
	for i := range dimensionDefs {
		if strings.EqualFold(dimensionDefs[i].ID, strings.TrimSpace(id)) {
			return &dimensionDefs[i]
		}
	}
	return nil
}

// dimensionList 给建模与智能体用：有哪些维度、取值字段是什么、能不能级联。
func dimensionList() map[string]any {
	items := make([]map[string]any, 0, len(dimensionDefs))
	for _, d := range dimensionDefs {
		table := findTable(d.Table)
		item := map[string]any{
			"dim":          d.ID,
			"label":        d.Label,
			"table":        d.Table,
			"value_field":  d.ValueCol,
			"label_field":  d.LabelCol,
			"extra_fields": d.ExtraCols,
			"note":         d.Note,
		}
		if d.ParentCol != "" {
			item["parent_col"] = d.ParentCol
			item["parent_dim"] = d.ParentDim
		}
		if table != nil {
			item["cardinality"] = table.RowCount
		}
		items = append(items, item)
	}
	return map[string]any{"items": items, "total": len(items)}
}

// dimensionLookup 支持三种取法：关键字模糊搜索、按父子级联过滤、按 id 批量反查。
func dimensionLookup(dim, keyword, parent string, values []string, limit int) (map[string]any, error) {
	def := findDimension(dim)
	if def == nil {
		return nil, fmt.Errorf("未知维度 %q，可用维度：%s", dim, dimensionIDs())
	}
	table := findTable(def.Table)
	if table == nil {
		return nil, fmt.Errorf("维度 %q 对应的表 %s 不存在", dim, def.Table)
	}
	if limit <= 0 {
		limit = dimDefaultLimit
	}
	if limit > dimMaxLimit {
		limit = dimMaxLimit
	}

	kw := strings.ToLower(strings.TrimSpace(keyword))
	parent = strings.TrimSpace(parent)
	wanted := map[string]bool{}
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			wanted[v] = true
		}
	}

	var matched []map[string]any
	for _, row := range table.rows {
		if parent != "" && def.ParentCol != "" && fmt.Sprint(row[def.ParentCol]) != parent {
			continue
		}
		if len(wanted) > 0 && !wanted[fmt.Sprint(row[def.ValueCol])] {
			continue
		}
		if kw != "" {
			hit := false
			for _, col := range def.SearchCol {
				if strings.Contains(strings.ToLower(fmt.Sprint(row[col])), kw) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		matched = append(matched, row)
	}

	// 完全匹配排前面：搜「北京市」时不要被「北京市朝阳区某某店」顶掉
	if kw != "" {
		sort.SliceStable(matched, func(i, j int) bool {
			a := fmt.Sprint(matched[i][def.LabelCol])
			b := fmt.Sprint(matched[j][def.LabelCol])
			ai := strings.EqualFold(a, keyword)
			bi := strings.EqualFold(b, keyword)
			if ai != bi {
				return ai
			}
			return false
		})
	}

	total := len(matched)
	hasMore := total > limit
	if hasMore {
		matched = matched[:limit]
	}

	items := make([]map[string]any, 0, len(matched))
	for _, row := range matched {
		extra := map[string]any{}
		for _, col := range def.ExtraCols {
			if v, ok := row[col]; ok && v != nil {
				extra[col] = v
			}
		}
		items = append(items, map[string]any{
			"value": fmt.Sprint(row[def.ValueCol]),
			"label": fmt.Sprint(row[def.LabelCol]),
			"extra": extra,
		})
	}
	return map[string]any{
		"dim":      def.ID,
		"items":    items,
		"total":    total,
		"has_more": hasMore,
		"limit":    limit,
	}, nil
}

func dimensionIDs() string {
	ids := make([]string, 0, len(dimensionDefs))
	for _, d := range dimensionDefs {
		ids = append(ids, d.ID)
	}
	return strings.Join(ids, ", ")
}

// dimensionsHandler 处理 GET /api/v1/dimensions。
func (s *server) dimensionsHandler(w http.ResponseWriter, r *http.Request) {
	platform.WriteJSON(w, http.StatusOK, dimensionList())
}

// dimensionLookupHandler 处理 GET /api/v1/dimensions/{dim}?keyword=&parent=&limit=&values=。
func (s *server) dimensionLookupHandler(w http.ResponseWriter, r *http.Request) {
	dim := r.PathValue("dim")
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			platform.WriteError(w, r, http.StatusBadRequest, "invalid_argument", "limit 必须是正整数")
			return
		}
		limit = n
	}
	var values []string
	if raw := q.Get("values"); raw != "" {
		values = strings.Split(raw, ",")
	}
	out, err := dimensionLookup(dim, q.Get("keyword"), q.Get("parent"), values, limit)
	if err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	platform.WriteJSON(w, http.StatusOK, out)
}
