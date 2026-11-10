package plugin

import (
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/policy"
)

func toolManifest(extra string) string {
	return `{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "crm_lookup",
  "name": "CRM 查询",
  "description": "按客户名查 CRM 记录，用户提到客户资料时使用",
  "capability": "http.request",
  "input_schema": {"type": "object", "properties": {}},
  "runtime": {"entry": "main.ts"}
  ` + extra + `
}`
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(toolManifest(`, "risk": 1`)), false)
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("未知字段必须直接失败，实际: %v", err)
	}
}

func TestParseRejectsDraftVersion(t *testing.T) {
	_, err := Parse([]byte(toolManifest(`, "version": "1.0.0"`)), false)
	if err == nil || !strings.Contains(err.Error(), "草稿不允许写 version") {
		t.Fatalf("草稿带版本必须被拒绝，实际: %v", err)
	}
}

func TestParseRequiresVersionWhenPublishing(t *testing.T) {
	_, err := Parse([]byte(toolManifest("")), true)
	if err == nil || !strings.Contains(err.Error(), "发布时必须提供") {
		t.Fatalf("发布缺版本必须被拒绝，实际: %v", err)
	}
}

func TestParseValidatesSlug(t *testing.T) {
	raw := strings.Replace(toolManifest(""), `"crm_lookup"`, `"CRM 查询"`, 1)
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("非法 slug 必须被拒绝")
	}
}

func TestDeriveRiskFromCapabilities(t *testing.T) {
	cases := []struct {
		name string
		caps string
		want int
	}{
		{
			name: "纯读取",
			caps: `[{"method":"http.request","hosts":["api.example.com"],"verbs":["GET"]}]`,
			want: policy.RiskReadCost,
		},
		{
			name: "外部写",
			caps: `[{"method":"http.request","hosts":["api.example.com"],"verbs":["POST"]}]`,
			want: policy.RiskWrite,
		},
		{
			name: "密钥 + 外部写 = 高敏",
			caps: `[{"method":"http.request","hosts":["api.example.com"],"verbs":["POST"]},{"method":"secret.use"}]`,
			want: policy.RiskSensitive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(toolManifest(`, "capabilities": `+tc.caps)), false)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			risk, effects := Derive(m)
			if risk != tc.want {
				t.Fatalf("风险等级 = %d，期望 %d（副作用 %s）", risk, tc.want, effects)
			}
		})
	}
}

func TestMCPIsAlwaysWriteRisk(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": {"type": "http", "url": "https://mcp.example.com/sse"},
  "tools": {"allow": ["*"]}
}`
	m, err := Parse([]byte(raw), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if risk, _ := Derive(m); risk < policy.RiskWrite {
		t.Fatalf("MCP 工具集由远端决定，风险不能低于写: %d", risk)
	}
}

func TestMCPRequiresToolAllowList(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": {"type": "http", "url": "https://mcp.example.com/sse"}
}`
	_, err := Parse([]byte(raw), false)
	if err == nil || !strings.Contains(err.Error(), "tools.allow") {
		t.Fatalf("MCP 必须显式声明 allow，实际: %v", err)
	}
}

func TestExtensibleDefaultsTrue(t *testing.T) {
	m, err := Parse([]byte(toolManifest("")), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !m.IsExtensible() {
		t.Fatal("未声明 extensible 应默认可扩展")
	}

	m, err = Parse([]byte(toolManifest(`, "extensible": false`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.IsExtensible() {
		t.Fatal("显式 false 应不可扩展")
	}
}

func TestExposedToolsRespectsAllowDeny(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": {"type": "http", "url": "https://mcp.example.com/sse"},
  "tools": {"allow": ["cust_*", "order_list"], "deny": ["cust_delete"]}
}`
	m, err := Parse([]byte(raw), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := m.ExposedTools()
	if len(got) == 0 {
		t.Fatal("allow 里至少要有条目")
	}
}

func TestAllowsToolPrefersDeny(t *testing.T) {
	m, err := Parse([]byte(`{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": {"type": "http", "url": "https://mcp.example.com/sse"},
  "tools": {"allow": ["cust_*", "order_list"], "deny": ["cust_delete"]}
}`), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	cases := map[string]bool{
		"cust_list":   true,
		"order_list":  true,
		"cust_delete": false,
		"order_edit":  false,
	}
	for name, want := range cases {
		if got := m.AllowsTool(name); got != want {
			t.Fatalf("AllowsTool(%q) = %v，期望 %v", name, got, want)
		}
	}
}

func TestHostsOnlyAcceptsBareDomains(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "crm_lookup",
  "name": "CRM 查询",
  "description": "按客户名查 CRM 记录，用户提到客户资料时使用",
  "capability": "http.request",
  "capabilities": [{"method":"http.request","hosts":["api.example.com","https://x.example.com/path"]}]
}`
	_, err := Parse([]byte(raw), false)
	if err == nil || !strings.Contains(err.Error(), "只写域名") {
		t.Fatalf("带协议/路径的 host 必须在解析期失败，实际: %v", err)
	}

	m, err := Parse([]byte(toolManifest(`, "capabilities": [{"method":"http.request","hosts":["api.example.com"]}]`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if hosts := m.Hosts(); len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Fatalf("出网白名单 = %v", hosts)
	}
}
