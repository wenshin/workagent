package plugin

import (
	"strings"
	"testing"
)

func testSources() []DataSource {
	return []DataSource{
		{ID: "orders", Plugin: "mysql", Tool: "sql_query", Mode: "read"},
		{ID: "submit", Plugin: "crm", Tool: "lead_create", Mode: "action"},
	}
}

const validDSL = `{
  "dsl": "wa-ui/1",
  "height": 420,
  "state": {"period": "month"},
  "data": {"orders": {"params": {"period": "{{state.period}}"}}},
  "view": {
    "type": "Stack",
    "gap": "md",
    "children": [
      {"type": "Heading", "text": "销售看板", "level": 2},
      {"type": "Text", "text": "共 {{data.orders.total}} 条", "tone": "muted"},
      {"type": "Table",
       "columns": [{"key": "name", "label": "客户"}, {"key": "amount", "label": "金额"}],
       "source": "{{data.orders.items}}"},
      {"type": "Button", "text": "提交", "action": {
        "type": "invoke", "source": "submit", "confirm": "确认提交？",
        "params": {"note": "{{state.period}}"}}}
    ]
  }
}`

func TestValidateDSLAcceptsWellFormed(t *testing.T) {
	height, err := ValidateDSL([]byte(validDSL), testSources())
	if err != nil {
		t.Fatalf("合法 DSL 不应报错：%v", err)
	}
	if height != 420 {
		t.Fatalf("height = %d，期望 420", height)
	}
}

func TestValidateDSLRejectsUnknownNode(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","view":{"type":"Script","text":"alert(1)"}}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("未知节点类型必须拒绝")
	}
}

func TestValidateDSLRejectsUnknownField(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","view":{"type":"Text","text":"hi","onClick":"x"}}`
	_, err := ValidateDSL([]byte(raw), testSources())
	if err == nil || !strings.Contains(err.Error(), "不支持字段") {
		t.Fatalf("节点未知字段必须拒绝，收到 %v", err)
	}
}

func TestValidateDSLRejectsUndeclaredDataSource(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","data":{"other":{}},"view":{"type":"Text","text":"hi"}}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("未声明的数据源必须拒绝")
	}
	if _, err := ValidateDSL([]byte(`{"dsl":"wa-ui/1","view":{"type":"Text","text":"{{data.other.x}}"}}`), testSources()); err == nil {
		t.Fatal("绑定到未声明的数据源必须拒绝")
	}
}

func TestValidateDSLRejectsWriteSourceOnLoad(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","data":{"submit":{}},"view":{"type":"Text","text":"hi"}}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("写数据源不能自动加载")
	}
}

func TestValidateDSLRejectsInvokeOnReadSource(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","view":{"type":"Button","text":"x","action":{"type":"invoke","source":"orders"}}}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("invoke 只能指向写数据源")
	}
}

func TestValidateDSLRejectsBadTemplates(t *testing.T) {
	cases := []string{
		`{"dsl":"wa-ui/1","view":{"type":"Text","text":"a }} b"}}`,
		`{"dsl":"wa-ui/1","view":{"type":"Text","text":"a {{b c}} d"}}`,
		`{"dsl":"wa-ui/1","view":{"type":"Text","text":"{{env.HOME}}"}}`,
	}
	for _, raw := range cases {
		if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
			t.Fatalf("非法绑定必须拒绝：%s", raw)
		}
	}
}

func TestValidateDSLRejectsDeepNesting(t *testing.T) {
	node := `{"type":"Text","text":"x"}`
	for i := 0; i < maxDSLDepth; i++ {
		node = `{"type":"Stack","children":[` + node + `]}`
	}
	raw := `{"dsl":"wa-ui/1","view":` + node + `}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("超深嵌套必须拒绝")
	}
}

func TestValidateDSLHeightBounds(t *testing.T) {
	raw := `{"dsl":"wa-ui/1","height":10,"view":{"type":"Text","text":"x"}}`
	if _, err := ValidateDSL([]byte(raw), testSources()); err == nil {
		t.Fatal("height 越界必须拒绝")
	}
}

// 内置生成技能里的示例必须能通过校验：文档写出来跑不通，模型就会照着错的写。
func TestBuiltinSkillExampleValidates(t *testing.T) {
	m, err := Parse([]byte(`{
  "protocol": "wa-plugin/1",
  "kind": "component",
  "slug": "sales_board",
  "name": "销售看板",
  "description": "按周期展示销售明细，需要看本月/本季销售时使用",
  "data_sources": [
    {"id": "orders", "plugin": "mysql", "tool": "sql_query", "mode": "read", "params": {"limit": 20}},
    {"id": "submit", "plugin": "crm", "tool": "lead_create", "mode": "action"}
  ]
}`), false)
	if err != nil {
		t.Fatalf("示例 plugin.json 应通过：%v", err)
	}
	ui := `{
  "dsl": "wa-ui/1",
  "height": 360,
  "state": {"period": "month"},
  "data": {"orders": {"params": {"period": "{{state.period}}"}}},
  "view": {"type": "Stack", "gap": "md", "children": [
    {"type": "Heading", "text": "销售看板", "level": 2},
    {"type": "Table", "columns": [{"key": "name", "label": "客户"}], "source": "{{data.orders.items}}"},
    {"type": "Button", "text": "提交", "action": {"type": "invoke", "source": "submit", "confirm": "确认提交？"}}
  ]}
}`
	if _, err := ValidateDSL([]byte(ui), m.DataSources); err != nil {
		t.Fatalf("示例 ui.json 应通过：%v", err)
	}
}

func TestComponentManifestUsesDSLFile(t *testing.T) {
	m, err := Parse([]byte(`{
  "protocol": "wa-plugin/1",
  "kind": "component",
  "slug": "sales_dashboard",
  "name": "销售看板",
  "description": "展示销售数据",
  "data_sources": [{"id": "orders", "plugin": "mysql"}]
}`), false)
	if err != nil {
		t.Fatalf("默认 ui.entry 应可解析：%v", err)
	}
	if m.UI == nil || m.UI.Entry != DSLFile {
		t.Fatalf("ui.entry 应默认为 %s，收到 %+v", DSLFile, m.UI)
	}
	if m.DataSources[0].Mode != "read" {
		t.Fatalf("data_sources.mode 应默认为 read，收到 %q", m.DataSources[0].Mode)
	}

	if _, err := Parse([]byte(`{
  "protocol": "wa-plugin/1", "kind": "component", "slug": "x1",
  "name": "x", "description": "x", "ui": {"entry": "ui.js"}
}`), false); err == nil {
		t.Fatal("组件入口必须是 .json DSL，脚本入口必须拒绝")
	}
}
