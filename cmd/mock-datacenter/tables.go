package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
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

// Table 是返回给调用方的表元信息；rows 是 mock 的数据行，不对外序列化。
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

var (
	tablesOnce  sync.Once
	tablesCache []*Table
)

// mockTables 返回全部 mock 表。
//
// 数据用固定种子生成一次：行数、行内容、维度值三者自洽（row_count 就是真实行数，
// count(*) 与之相等），这样才能拿它做闭环测试——筛选条件生效与否是能看出来的。
func mockTables() []*Table {
	tablesOnce.Do(func() { tablesCache = buildTables() })
	return tablesCache
}

func buildTables() []*Table {
	now := time.Now()
	rnd := rand.New(rand.NewSource(20261118))

	cities := buildCities()
	stores := buildStores(rnd, cities)
	series := buildSeries()
	orders := buildOrders(rnd, now, cities, stores, series)
	users := buildUsers(rnd, now, cities, series)
	daily := buildSalesDaily(orders)
	visits := buildVisits(rnd, now, series)
	clues := buildClues(rnd, now, cities, series, series)

	return []*Table{
		newTable("dim", "dim_city", "城市维度表，省份 / 大区 / 行政区划码，供按城市筛选时把名称换成 code",
			"", "DIM", []string{"城市", "维度"}, cities,
			Column{"city_code", "string", "行政区划码（主键，字符串以免前导零丢失）", false},
			Column{"city_name", "string", "城市名称", false},
			Column{"province", "string", "所属省份", false},
			Column{"region", "string", "大区：华北/东北/华东/华中/华南/西南/西北", false},
			Column{"city_level", "string", "城市等级：一线/新一线/二线/三线", true},
		),
		newTable("dim", "dim_store", "门店维度表，门店与城市的对应关系，支持按城市级联筛选",
			"", "DIM", []string{"门店", "维度"}, stores,
			Column{"store_code", "string", "门店编码（主键）", false},
			Column{"store_name", "string", "门店名称", false},
			Column{"city_code", "string", "所属城市 code", false},
			Column{"city_name", "string", "所属城市名称", false},
			Column{"channel", "string", "门店类型：旗舰店/交付中心/城市展厅", false},
			Column{"open_date", "string", "开业日期", true},
			Column{"status", "string", "营业状态：active/closed", false},
		),
		newTable("dim", "dim_series", "车系维度表，品牌 / 能源类型 / 价格带",
			"", "DIM", []string{"车系", "维度"}, series,
			Column{"series_id", "bigint", "车系 ID（主键）", false},
			Column{"series_name", "string", "车系名称", false},
			Column{"brand", "string", "品牌", false},
			Column{"energy_type", "string", "能源类型：燃油/纯电/插混/增程", false},
			Column{"price_band", "string", "价格带", true},
		),
		newTable("dim", "dim_channel", "渠道维度表", "", "DIM", []string{"渠道", "维度"},
			[]map[string]any{
				{"channel_code": "APP", "channel_name": "官方 APP", "channel_type": "自有"},
				{"channel_code": "MINI", "channel_name": "微信小程序", "channel_type": "自有"},
				{"channel_code": "H5", "channel_name": "H5 落地页", "channel_type": "自有"},
				{"channel_code": "STORE", "channel_name": "门店到店", "channel_type": "线下"},
				{"channel_code": "TEL", "channel_name": "电话外呼", "channel_type": "线下"},
			},
			Column{"channel_code", "string", "渠道编码（主键）", false},
			Column{"channel_name", "string", "渠道名称", false},
			Column{"channel_type", "string", "渠道类别：自有/线下", false},
		),
		newTable("dw", "dw_order_detail", "订单明细宽表，按下单日期分区，覆盖全渠道成交与退款订单",
			"dt", "DW", []string{"订单", "交易", "核心表"}, orders,
			Column{"order_id", "string", "订单号", false},
			Column{"user_id", "bigint", "用户 ID", false},
			Column{"city_code", "string", "城市 code（与 dim_city 关联）", false},
			Column{"city", "string", "城市名称（冗余字段）", false},
			Column{"store_code", "string", "门店编码（与 dim_store 关联）", true},
			Column{"series_id", "bigint", "车系 ID（与 dim_series 关联）", true},
			Column{"series_name", "string", "车系名称", true},
			Column{"order_amount", "double", "订单金额（元）", false},
			Column{"discount_amount", "double", "优惠金额（元）", true},
			Column{"order_status", "string", "订单状态：paid/refunded/closed", false},
			Column{"channel_code", "string", "渠道编码", false},
			Column{"channel", "string", "渠道名称", false},
			Column{"create_time", "string", "下单时间", false},
			Column{"dt", "string", "分区日期 yyyy-MM-dd", false},
		),
		newTable("dw", "dw_user_profile", "用户画像宽表，一用户一行",
			"", "DW", []string{"用户", "画像"}, users,
			Column{"user_id", "bigint", "用户 ID（主键）", false},
			Column{"gender", "string", "性别：男/女/未知", true},
			Column{"age", "int", "年龄", true},
			Column{"city_code", "string", "常驻城市 code", true},
			Column{"city", "string", "常驻城市", true},
			Column{"register_date", "string", "注册日期", false},
			Column{"is_new_user", "boolean", "是否新用户", false},
			Column{"prefer_series_id", "bigint", "偏好车系 ID", true},
			Column{"prefer_series", "string", "偏好车系", true},
		),
		newTable("dm", "dm_sales_daily", "销售日报汇总，按日期 + 城市聚合，由 dw_order_detail 派生",
			"dt", "DM", []string{"销售", "汇总", "看板"}, daily,
			Column{"dt", "string", "统计日期 yyyy-MM-dd", false},
			Column{"city_code", "string", "城市 code", false},
			Column{"city", "string", "城市名称", false},
			Column{"order_cnt", "bigint", "订单数", false},
			Column{"gmv", "double", "成交金额（元，不含退款）", false},
			Column{"avg_price", "double", "客单价（元）", false},
			Column{"refund_cnt", "bigint", "退款单数", false},
		),
		newTable("dm", "dm_vehicle_visit", "车系流量日报，按日期 + 车系聚合",
			"dt", "DM", []string{"流量", "车系"}, visits,
			Column{"dt", "string", "统计日期 yyyy-MM-dd", false},
			Column{"series_id", "bigint", "车系 ID", false},
			Column{"series_name", "string", "车系名称", false},
			Column{"pv", "bigint", "浏览量", false},
			Column{"uv", "bigint", "访客数", false},
			Column{"clue_cnt", "bigint", "留资数", false},
		),
		newTable("ods", "ods_clue", "线索明细表，各渠道留资原始数据",
			"", "ODS", []string{"线索", "原始层"}, clues,
			Column{"clue_id", "string", "线索 ID", false},
			Column{"series_id", "bigint", "车系 ID", true},
			Column{"series_name", "string", "车系名称", true},
			Column{"city_code", "string", "城市 code", true},
			Column{"city", "string", "城市名称", true},
			Column{"source", "string", "线索来源：APP/小程序/门店/电话", false},
			Column{"status", "string", "线索状态：new/following/invalid", false},
			Column{"created_at", "string", "创建时间", false},
		),
	}
}

// matchTable 判定关键字是否命中表：表名、字段名、描述、标签、负责人。
// 空关键字视为全部命中。
func matchTable(t *Table, keyword string) bool {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	if kw == "" {
		return true
	}
	fields := []string{t.Database, t.Name, t.FullName, t.Description, t.Layer, t.Owner.Name, t.Owner.JobNumber, t.Owner.Email}
	fields = append(fields, t.Tags...)
	for _, c := range t.Columns {
		fields = append(fields, c.Name, c.Comment)
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), kw) {
			return true
		}
	}
	return false
}

func newTable(db, name, desc, partition, layer string, tags []string, rows []map[string]any, cols ...Column) *Table {
	partitions := []string(nil)
	if partition != "" {
		partitions = []string{partition}
	}
	size := int64(len(rows)) * 256
	return &Table{
		Database:      db,
		Name:          name,
		FullName:      db + "." + name,
		Description:   desc,
		Layer:         layer,
		Owner:         ownerYan,
		Columns:       cols,
		PartitionKeys: partitions,
		Tags:          tags,
		RowCount:      int64(len(rows)),
		SizeBytes:     size,
		UpdateTime:    time.Now().Add(-3 * time.Hour).Format("2006-01-02 15:04:05"),
		rows:          rows,
	}
}

// ---------------------------------------------------------------------------
// 维度数据
// ---------------------------------------------------------------------------

type cityRow struct {
	code, name, province, region, level string
}

type provinceDef struct {
	name   string
	region string
	cities []string
}

// 真实行政区划名称，用来让「按城市搜索」有真实的模糊匹配效果（搜「州」能出一堆）。
var provinces = []provinceDef{
	{"北京市", "华北", []string{"北京市"}},
	{"天津市", "华北", []string{"天津市"}},
	{"河北省", "华北", []string{"石家庄市", "唐山市", "秦皇岛市", "邯郸市", "邢台市", "保定市", "张家口市", "承德市", "沧州市", "廊坊市", "衡水市"}},
	{"山西省", "华北", []string{"太原市", "大同市", "阳泉市", "长治市", "晋城市", "朔州市", "晋中市", "运城市", "忻州市", "临汾市", "吕梁市"}},
	{"内蒙古自治区", "华北", []string{"呼和浩特市", "包头市", "乌海市", "赤峰市", "通辽市", "鄂尔多斯市", "呼伦贝尔市", "巴彦淖尔市", "乌兰察布市"}},
	{"辽宁省", "东北", []string{"沈阳市", "大连市", "鞍山市", "抚顺市", "本溪市", "丹东市", "锦州市", "营口市", "阜新市", "辽阳市", "盘锦市", "铁岭市", "朝阳市", "葫芦岛市"}},
	{"吉林省", "东北", []string{"长春市", "吉林市", "四平市", "辽源市", "通化市", "白山市", "松原市", "白城市", "延边州"}},
	{"黑龙江省", "东北", []string{"哈尔滨市", "齐齐哈尔市", "鸡西市", "鹤岗市", "双鸭山市", "大庆市", "伊春市", "佳木斯市", "七台河市", "牡丹江市", "黑河市", "绥化市"}},
	{"上海市", "华东", []string{"上海市"}},
	{"江苏省", "华东", []string{"南京市", "无锡市", "徐州市", "常州市", "苏州市", "南通市", "连云港市", "淮安市", "盐城市", "扬州市", "镇江市", "泰州市", "宿迁市"}},
	{"浙江省", "华东", []string{"杭州市", "宁波市", "温州市", "嘉兴市", "湖州市", "绍兴市", "金华市", "衢州市", "舟山市", "台州市", "丽水市"}},
	{"安徽省", "华东", []string{"合肥市", "芜湖市", "蚌埠市", "淮南市", "马鞍山市", "淮北市", "铜陵市", "安庆市", "黄山市", "滁州市", "阜阳市", "宿州市", "六安市", "亳州市", "池州市", "宣城市"}},
	{"福建省", "华东", []string{"福州市", "厦门市", "莆田市", "三明市", "泉州市", "漳州市", "南平市", "龙岩市", "宁德市"}},
	{"江西省", "华东", []string{"南昌市", "景德镇市", "萍乡市", "九江市", "新余市", "鹰潭市", "赣州市", "吉安市", "宜春市", "抚州市", "上饶市"}},
	{"山东省", "华东", []string{"济南市", "青岛市", "淄博市", "枣庄市", "东营市", "烟台市", "潍坊市", "济宁市", "泰安市", "威海市", "日照市", "临沂市", "德州市", "聊城市", "滨州市", "菏泽市"}},
	{"河南省", "华中", []string{"郑州市", "开封市", "洛阳市", "平顶山市", "安阳市", "鹤壁市", "新乡市", "焦作市", "濮阳市", "许昌市", "漯河市", "三门峡市", "南阳市", "商丘市", "信阳市", "周口市", "驻马店市"}},
	{"湖北省", "华中", []string{"武汉市", "黄石市", "十堰市", "宜昌市", "襄阳市", "鄂州市", "荆门市", "孝感市", "荆州市", "黄冈市", "咸宁市", "随州市", "恩施州"}},
	{"湖南省", "华中", []string{"长沙市", "株洲市", "湘潭市", "衡阳市", "邵阳市", "岳阳市", "常德市", "张家界市", "益阳市", "郴州市", "永州市", "怀化市", "娄底市", "湘西州"}},
	{"广东省", "华南", []string{"广州市", "韶关市", "深圳市", "珠海市", "汕头市", "佛山市", "江门市", "湛江市", "茂名市", "肇庆市", "惠州市", "梅州市", "汕尾市", "河源市", "阳江市", "清远市", "东莞市", "中山市", "潮州市", "揭阳市", "云浮市"}},
	{"广西壮族自治区", "华南", []string{"南宁市", "柳州市", "桂林市", "梧州市", "北海市", "防城港市", "钦州市", "贵港市", "玉林市", "百色市", "贺州市", "河池市", "来宾市", "崇左市"}},
	{"海南省", "华南", []string{"海口市", "三亚市", "三沙市", "儋州市"}},
	{"重庆市", "西南", []string{"重庆市"}},
	{"四川省", "西南", []string{"成都市", "自贡市", "攀枝花市", "泸州市", "德阳市", "绵阳市", "广元市", "遂宁市", "内江市", "乐山市", "南充市", "眉山市", "宜宾市", "广安市", "达州市", "雅安市", "巴中市", "资阳市"}},
	{"贵州省", "西南", []string{"贵阳市", "六盘水市", "遵义市", "安顺市", "毕节市", "铜仁市"}},
	{"云南省", "西南", []string{"昆明市", "曲靖市", "玉溪市", "保山市", "昭通市", "丽江市", "普洱市", "临沧市"}},
	{"西藏自治区", "西南", []string{"拉萨市", "日喀则市", "昌都市", "林芝市", "山南市", "那曲市"}},
	{"陕西省", "西北", []string{"西安市", "铜川市", "宝鸡市", "咸阳市", "渭南市", "延安市", "汉中市", "榆林市", "安康市", "商洛市"}},
	{"甘肃省", "西北", []string{"兰州市", "嘉峪关市", "金昌市", "白银市", "天水市", "武威市", "张掖市", "平凉市", "酒泉市", "庆阳市", "定西市", "陇南市"}},
	{"青海省", "西北", []string{"西宁市", "海东市"}},
	{"宁夏回族自治区", "西北", []string{"银川市", "石嘴山市", "吴忠市", "固原市", "中卫市"}},
	{"新疆维吾尔自治区", "西北", []string{"乌鲁木齐市", "克拉玛依市", "吐鲁番市", "哈密市", "昌吉州", "博尔塔拉州", "巴音郭楞州", "阿克苏地区", "喀什地区", "和田地区", "伊犁州", "塔城地区", "阿勒泰地区"}},
}

var firstTier = map[string]bool{"北京市": true, "上海市": true, "广州市": true, "深圳市": true}
var newFirstTier = map[string]bool{
	"成都市": true, "杭州市": true, "重庆市": true, "西安市": true, "苏州市": true,
	"武汉市": true, "南京市": true, "天津市": true, "郑州市": true, "长沙市": true,
	"东莞市": true, "宁波市": true, "佛山市": true, "合肥市": true, "青岛市": true,
}

func buildCities() []map[string]any {
	var out []map[string]any
	for i, p := range provinces {
		for j, name := range p.cities {
			level := "三线"
			switch {
			case firstTier[name]:
				level = "一线"
			case newFirstTier[name]:
				level = "新一线"
			case i < 16:
				level = "二线"
			}
			out = append(out, map[string]any{
				"city_code":  fmt.Sprintf("%02d%02d00", 11+i, j+1),
				"city_name":  name,
				"province":   p.name,
				"region":     p.region,
				"city_level": level,
			})
		}
	}
	return out
}

func cityName(raw any) string { return fmt.Sprint(raw) }

func buildStores(rnd *rand.Rand, cities []map[string]any) []map[string]any {
	types := []string{"旗舰店", "交付中心", "城市展厅"}
	out := make([]map[string]any, 0, len(cities)*4)
	seq := 1
	base := time.Date(2016, 3, 1, 0, 0, 0, 0, time.Local)
	for _, c := range cities {
		n := 2 + rnd.Intn(3)
		for k := 0; k < n; k++ {
			out = append(out, map[string]any{
				"store_code": fmt.Sprintf("S%05d", seq),
				"store_name": fmt.Sprintf("%s%s", strings.TrimSuffix(cityName(c["city_name"]), "市"), types[rnd.Intn(len(types))]),
				"city_code":  cityName(c["city_code"]),
				"city_name":  cityName(c["city_name"]),
				"channel":    types[rnd.Intn(len(types))],
				"open_date":  base.AddDate(0, rnd.Intn(90), rnd.Intn(28)).Format("2006-01-02"),
				"status":     "active",
			})
			seq++
		}
	}
	// 留少量闭店门店，方便测「状态筛选」
	for i := 0; i < 17; i++ {
		out[i*13%len(out)]["status"] = "closed"
	}
	return out
}

type seriesDef struct {
	name   string
	brand  string
	energy string
	price  string
}

var seriesCatalog = []seriesDef{
	{"奥迪A4L", "奥迪", "燃油", "25-35万"}, {"奥迪A6L", "奥迪", "燃油", "40-55万"},
	{"奥迪Q5L", "奥迪", "燃油", "38-50万"}, {"宝马3系", "宝马", "燃油", "28-40万"},
	{"宝马5系", "宝马", "燃油", "42-55万"}, {"宝马X3", "宝马", "燃油", "38-48万"},
	{"奔驰C级", "奔驰", "燃油", "30-38万"}, {"奔驰E级", "奔驰", "燃油", "45-55万"},
	{"奔驰GLC", "奔驰", "燃油", "40-52万"}, {"特斯拉Model 3", "特斯拉", "纯电", "23-34万"},
	{"特斯拉Model Y", "特斯拉", "纯电", "26-36万"}, {"比亚迪秦PLUS", "比亚迪", "插混", "8-14万"},
	{"比亚迪汉", "比亚迪", "纯电", "17-28万"}, {"比亚迪宋PLUS", "比亚迪", "插混", "13-18万"},
	{"比亚迪海豚", "比亚迪", "纯电", "10-13万"}, {"比亚迪唐", "比亚迪", "插混", "20-33万"},
	{"蔚来ES6", "蔚来", "纯电", "34-45万"}, {"蔚来ET5", "蔚来", "纯电", "30-38万"},
	{"小鹏P7", "小鹏", "纯电", "20-28万"}, {"小鹏G6", "小鹏", "纯电", "20-27万"},
	{"理想L7", "理想", "增程", "30-38万"}, {"理想L8", "理想", "增程", "33-40万"},
	{"理想L9", "理想", "增程", "40-46万"}, {"吉利星越L", "吉利", "燃油", "13-19万"},
	{"吉利帝豪", "吉利", "燃油", "6-9万"}, {"领克03", "领克", "燃油", "11-17万"},
	{"领克08", "领克", "插混", "20-26万"}, {"长安CS75PLUS", "长安", "燃油", "10-15万"},
	{"长安UNI-V", "长安", "燃油", "10-13万"}, {"哈弗H6", "哈弗", "燃油", "9-15万"},
	{"坦克300", "坦克", "燃油", "20-30万"}, {"奇瑞瑞虎8", "奇瑞", "燃油", "9-16万"},
	{"红旗H5", "红旗", "燃油", "15-22万"}, {"红旗HS5", "红旗", "燃油", "18-25万"},
	{"埃安AION Y", "埃安", "纯电", "10-15万"}, {"五菱宏光MINI EV", "五菱", "纯电", "3-7万"},
	{"大众朗逸", "大众", "燃油", "8-14万"}, {"大众迈腾", "大众", "燃油", "18-26万"},
	{"丰田凯美瑞", "丰田", "燃油", "17-26万"}, {"丰田RAV4荣放", "丰田", "燃油", "17-25万"},
	{"本田雅阁", "本田", "燃油", "16-25万"}, {"本田CR-V", "本田", "燃油", "18-26万"},
	{"日产轩逸", "日产", "燃油", "9-14万"}, {"别克君威", "别克", "燃油", "16-22万"},
	{"沃尔沃XC60", "沃尔沃", "燃油", "30-40万"}, {"凯迪拉克CT5", "凯迪拉克", "燃油", "22-30万"},
	{"雷克萨斯ES", "雷克萨斯", "燃油", "30-45万"}, {"极氪001", "极氪", "纯电", "26-35万"},
	{"问界M7", "问界", "增程", "25-33万"}, {"阿维塔11", "阿维塔", "纯电", "30-42万"},
	{"深蓝SL03", "深蓝", "纯电", "14-20万"}, {"哪吒S", "哪吒", "纯电", "15-22万"},
	{"零跑C11", "零跑", "纯电", "15-20万"}, {"腾势D9", "腾势", "插混", "33-45万"},
	{"岚图FREE", "岚图", "增程", "25-33万"}, {"长安深蓝S7", "深蓝", "增程", "14-20万"},
	{"广汽传祺M8", "广汽传祺", "燃油", "18-28万"}, {"现代伊兰特", "现代", "燃油", "8-12万"},
	{"福特锐界", "福特", "燃油", "22-30万"}, {"马自达CX-5", "马自达", "燃油", "14-20万"},
	{"雪佛兰科鲁泽", "雪佛兰", "燃油", "7-10万"}, {"起亚K5", "起亚", "燃油", "15-20万"},
}

func buildSeries() []map[string]any {
	out := make([]map[string]any, 0, len(seriesCatalog))
	for i, s := range seriesCatalog {
		out = append(out, map[string]any{
			"series_id":   int64(30001 + i),
			"series_name": s.name,
			"brand":       s.brand,
			"energy_type": s.energy,
			"price_band":  s.price,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 事实数据
// ---------------------------------------------------------------------------

var channels = []struct{ code, name string }{
	{"APP", "官方 APP"},
	{"MINI", "微信小程序"},
	{"H5", "H5 落地页"},
	{"STORE", "门店到店"},
	{"TEL", "电话外呼"},
}

func buildOrders(rnd *rand.Rand, now time.Time, cities, stores, series []map[string]any) []map[string]any {
	const total = 620
	today := now.Truncate(24 * time.Hour)
	out := make([]map[string]any, 0, total)
	statuses := []string{"paid", "paid", "paid", "paid", "paid", "paid", "paid", "refunded", "refunded", "closed"}
	for i := 0; i < total; i++ {
		c := cities[rnd.Intn(len(cities))]
		s := series[rnd.Intn(len(series))]
		var storeCode any
		if rnd.Intn(4) != 0 {
			// 门店必须与城市一致，否则 join 出来的城市维度会互相矛盾
			for _, st := range stores {
				if st["city_code"] == c["city_code"] {
					storeCode = st["store_code"]
					break
				}
			}
		}
		day := today.AddDate(0, 0, -rnd.Intn(90))
		created := day.Add(time.Duration(8+rnd.Intn(12))*time.Hour + time.Duration(rnd.Intn(60))*time.Minute)
		amount := float64(80000+rnd.Intn(420000)) / 1.0
		ch := channels[rnd.Intn(len(channels))]
		status := statuses[rnd.Intn(len(statuses))]
		out = append(out, map[string]any{
			"order_id":        fmt.Sprintf("A2026%05d", i+1),
			"user_id":         int64(1000001 + rnd.Intn(200)),
			"city_code":       c["city_code"],
			"city":            c["city_name"],
			"store_code":      storeCode,
			"series_id":       s["series_id"],
			"series_name":     s["series_name"],
			"order_amount":    round2(amount),
			"discount_amount": round2(float64(rnd.Intn(20000))),
			"order_status":    status,
			"channel_code":    ch.code,
			"channel":         ch.name,
			"create_time":     created.Format("2006-01-02 15:04:05"),
			"dt":              day.Format("2006-01-02"),
		})
	}
	return out
}

func buildUsers(rnd *rand.Rand, now time.Time, cities, series []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, 200)
	genders := []string{"男", "女", "未知"}
	base := time.Date(2019, 6, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 200; i++ {
		c := cities[rnd.Intn(len(cities))]
		s := series[rnd.Intn(len(series))]
		reg := base.AddDate(0, rnd.Intn(72), rnd.Intn(28))
		out = append(out, map[string]any{
			"user_id":          int64(1000001 + i),
			"gender":           genders[rnd.Intn(len(genders))],
			"age":              int64(22 + rnd.Intn(38)),
			"city_code":        c["city_code"],
			"city":             c["city_name"],
			"register_date":    reg.Format("2006-01-02"),
			"is_new_user":      reg.After(now.AddDate(0, -3, 0)),
			"prefer_series_id": s["series_id"],
			"prefer_series":    s["series_name"],
		})
	}
	return out
}

func buildSalesDaily(orders []map[string]any) []map[string]any {
	type agg struct {
		cnt, refund int64
		gmv         float64
		city        string
	}
	bucket := map[string]*agg{}
	var keys []string
	for _, o := range orders {
		key := fmt.Sprint(o["dt"]) + "|" + fmt.Sprint(o["city_code"])
		a, ok := bucket[key]
		if !ok {
			a = &agg{city: fmt.Sprint(o["city"])}
			bucket[key] = a
			keys = append(keys, key)
		}
		amount, _ := toNumber(o["order_amount"])
		a.cnt++
		if o["order_status"] == "paid" {
			a.gmv += amount
		} else if o["order_status"] == "refunded" {
			a.refund++
		}
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		a := bucket[key]
		parts := strings.SplitN(key, "|", 2)
		avg := 0.0
		if a.cnt > 0 {
			avg = a.gmv / float64(a.cnt)
		}
		out = append(out, map[string]any{
			"dt":         parts[0],
			"city_code":  parts[1],
			"city":       a.city,
			"order_cnt":  a.cnt,
			"gmv":        round2(a.gmv),
			"avg_price":  round2(avg),
			"refund_cnt": a.refund,
		})
	}
	// 按日期倒序，最贴近「看最近数据」的使用习惯
	sort.SliceStable(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["dt"]) > fmt.Sprint(out[j]["dt"])
	})
	return out
}

func buildVisits(rnd *rand.Rand, now time.Time, series []map[string]any) []map[string]any {
	today := now.Truncate(24 * time.Hour)
	out := make([]map[string]any, 0, 30*len(series))
	for d := 0; d < 30; d++ {
		day := today.AddDate(0, 0, -d).Format("2006-01-02")
		for _, s := range series {
			pv := int64(800 + rnd.Intn(6000))
			uv := int64(float64(pv) * (0.3 + rnd.Float64()*0.25))
			out = append(out, map[string]any{
				"dt":          day,
				"series_id":   s["series_id"],
				"series_name": s["series_name"],
				"pv":          pv,
				"uv":          uv,
				"clue_cnt":    int64(float64(uv) * (0.01 + rnd.Float64()*0.04)),
			})
		}
	}
	return out
}

func buildClues(rnd *rand.Rand, now time.Time, cities, series, _ []map[string]any) []map[string]any {
	const total = 500
	sources := []string{"APP", "小程序", "门店", "电话"}
	statuses := []string{"new", "new", "following", "following", "invalid"}
	out := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		c := cities[rnd.Intn(len(cities))]
		s := series[rnd.Intn(len(series))]
		created := now.Add(-time.Duration(rnd.Intn(30*24)) * time.Hour)
		out = append(out, map[string]any{
			"clue_id":     fmt.Sprintf("C2026%05d", i+1),
			"series_id":   s["series_id"],
			"series_name": s["series_name"],
			"city_code":   c["city_code"],
			"city":        c["city_name"],
			"source":      sources[rnd.Intn(len(sources))],
			"status":      statuses[rnd.Intn(len(statuses))],
			"created_at":  created.Format("2006-01-02 15:04:05"),
		})
	}
	return out
}
