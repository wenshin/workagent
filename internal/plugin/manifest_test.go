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

// 安装确认展示的权限条目：默认只读必须说清楚，写权限只在声明后出现。
func TestPermissions(t *testing.T) {
	m, err := Parse([]byte(toolManifest(`, "capabilities": [{"method": "http.request", "hosts": ["api.example.com"], "verbs": ["GET"]}]`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := strings.Join(m.Permissions(), "|")
	for _, want := range []string{"读取空间文件：全部", "不授予文件写权限", "访问网络：api.example.com（GET）"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际: %s", want, got)
		}
	}

	// 声明写目录：写权限出现，且没有「不授予写权限」这句自相矛盾的话
	withWrite, err := Parse([]byte(toolManifest(`, "fs": {"read": ["data/"], "write": ["output/"]}`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got = strings.Join(withWrite.Permissions(), "|")
	if !strings.Contains(got, "读取空间文件：data/") || !strings.Contains(got, "写入空间文件：output/") {
		t.Errorf("声明的路径没出现在权限里: %s", got)
	}
	if strings.Contains(got, "不授予文件写权限") {
		t.Errorf("声明了写目录还说没有写权限: %s", got)
	}
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
			caps: `[{"method":"http.request","hosts":["api.example.com"],"verbs":["POST"]},{"method":"secret.use","name":"token"}]`,
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

// MCP 调的是外部系统，对平台内数据没有读写权限；平台不替远端判断副作用。
// 因此它按「只读有成本」参与策略：有绑定即放行，不该默认挂审批。
func TestMCPIsExternalNotWrite(t *testing.T) {
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
	risk, effects := Derive(m)
	if risk >= policy.RiskWrite {
		t.Fatalf("MCP 不应被判为写风险: %d", risk)
	}
	if risk > policy.RiskReadCost {
		t.Fatalf("MCP 至多按「只读有成本」处理: %d", risk)
	}
	if effects != policy.SideEffectsExternal {
		t.Fatalf("MCP 必须标记外部影响，实际 = %s", effects)
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

// 内网 MCP 常常只有 http：协议默认卡 https，但作者显式声明 allow_http 就要放行，
// 且必须在权限文案里写清「明文」，安装的人才知道代价。
func TestMCPPlainHTTPNeedsExplicitAllow(t *testing.T) {
	body := func(transport string) string {
		return `{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": ` + transport + `,
  "tools": {"allow": ["*"]}
}`
	}

	// 内网 http + 未声明：拒绝，且报错要给出写法
	_, err := Parse([]byte(body(`{"type":"http","url":"http://crm.internal/mcp"}`)), false)
	if err == nil || !strings.Contains(err.Error(), "allow_http") {
		t.Fatalf("未声明 allow_http 的内网 http 必须拒绝，实际: %v", err)
	}

	// 声明后放行
	m, err := Parse([]byte(body(`{"type":"http","url":"http://crm.internal/mcp","allow_http":true}`)), false)
	if err != nil {
		t.Fatalf("声明 allow_http 后应放行: %v", err)
	}
	if !strings.Contains(strings.Join(m.Permissions(), "\n"), "明文 http") {
		t.Fatalf("明文连接必须在权限文案里写清: %v", m.Permissions())
	}

	// 本机调试不必声明
	if _, err := Parse([]byte(body(`{"type":"http","url":"http://127.0.0.1:3000/mcp"}`)), false); err != nil {
		t.Fatalf("本机 http 应放行: %v", err)
	}

	// 非 http(s) 一律拒绝
	if _, err := Parse([]byte(body(`{"type":"http","url":"ftp://crm.internal/mcp"}`)), false); err == nil {
		t.Fatal("非 http/https 协议必须拒绝")
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

// fs.* 已经不是宿主 method：写进 capabilities 必须解析失败，
// 否则作者会以为「声明了就能读写」，实际运行时根本没有这条通道。
func TestCapabilityRejectsFSMethods(t *testing.T) {
	for _, method := range []string{"fs.read", "fs.list", "fs.write"} {
		t.Run(method, func(t *testing.T) {
			raw := toolManifest(`, "capabilities": [{"method": "` + method + `"}]`)
			if _, err := Parse([]byte(raw), false); err == nil {
				t.Fatalf("%s 应该被拒绝", method)
			}
		})
	}
}

// 内置工具没有 capabilities，风险只能来自平台声明：
// 这条链路断了就会把「写空间文件」的工具显示成只读。
func TestDeriveUsesBuiltinOverride(t *testing.T) {
	risk, effects := Derive(Manifest{
		Kind: KindTool, Builtin: true,
		DeriveRisk:    policy.RiskWrite,
		DeriveEffects: policy.SideEffectsWorkspace,
	})
	if risk != policy.RiskWrite || effects != policy.SideEffectsWorkspace {
		t.Fatalf("内置件风险 = %d/%s，期望 %d/%s", risk, effects, policy.RiskWrite, policy.SideEffectsWorkspace)
	}
}

// secret.use 必须写明凭证名：只说「能读凭证」就等于能读整个 scope。
func TestSecretUseRequiresName(t *testing.T) {
	raw := toolManifest(`, "capabilities": [{"method": "secret.use"}]`)
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("没有 name 的 secret.use 应该被拒绝")
	}
}

// 只放行清单里逐个声明过的凭证，别的名字连 RPC 白名单都不该出现。
func TestHostAllowOnlyDeclaredSecrets(t *testing.T) {
	raw := toolManifest(`, "capabilities": [
		{"method": "http.request", "hosts": ["api.example.com"], "verbs": ["POST"]},
		{"method": "secret.use", "name": "dingtalk"}
	]`)
	m, err := Parse([]byte(raw), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	allow, verbs, secrets := m.HostAllow()
	if !secrets["dingtalk"] || len(secrets) != 1 {
		t.Fatalf("凭证集合 = %v，期望只有 dingtalk", secrets)
	}
	if !allow[MethodSecretGet] || !allow[MethodLog] {
		t.Fatalf("secret.get / log 应放行，实际 %v", allow)
	}
	if allow["http.request"] {
		t.Fatal("http.request 不再是宿主 RPC")
	}
	if !verbs["POST"] {
		t.Fatalf("动词集合 = %v，期望含 POST", verbs)
	}
}

// ws.connect 必须有 hosts：隧道是按域名授权的，没有域名就等于放行一切。
func TestWSConnectRequiresHosts(t *testing.T) {
	raw := toolManifest(`, "capabilities": [{"method": "ws.connect"}]`)
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("没有 hosts 的 ws.connect 应该被拒绝")
	}
}

// 隧道里看不到方法，写 verbs 只会让人以为「写了就管」。
func TestWSConnectRejectsVerbs(t *testing.T) {
	raw := toolManifest(`, "capabilities": [{"method": "ws.connect", "hosts": ["wss.example"], "verbs": ["GET"]}]`)
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("ws.connect 不应接受 verbs")
	}
}

// 隧道能跑任意协议，按外部写算风险，副作用也是 external。
func TestWSConnectDerivesWriteRisk(t *testing.T) {
	raw := toolManifest(`, "capabilities": [{"method": "ws.connect", "hosts": ["wss.example"]}]`)
	m, err := Parse([]byte(raw), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	risk, effects := Derive(m)
	if risk != policy.RiskWrite || effects != policy.SideEffectsExternal {
		t.Fatalf("推导 = %d/%s，期望 %d/%s", risk, effects, policy.RiskWrite, policy.SideEffectsExternal)
	}
	if got := m.WSNetHosts(); len(got) != 1 || got[0] != "wss.example" {
		t.Fatalf("隧道白名单 = %v", got)
	}
	if got := m.NetHosts(); len(got) != 0 {
		t.Fatalf("ws.connect 不应混进 HTTP 白名单: %v", got)
	}
}

// 默认一次性：不写 runtime.resident 就不进常驻池。
func TestResidentDefaultsOff(t *testing.T) {
	m, err := Parse([]byte(toolManifest("")), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.Runtime.Resident {
		t.Fatal("没写 resident 的工具不应该常驻")
	}
}

// 显式声明才常驻。
func TestResidentAcceptsTrue(t *testing.T) {
	m, err := Parse([]byte(toolManifest(`, "runtime": {"entry": "main.ts", "resident": true}`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !m.Runtime.Resident {
		t.Fatal("显式声明 resident 的工具应该常驻")
	}
}

// 技能/组件/智能体没有执行器，写 resident 只会让人以为有进程。
func TestResidentOnlyForTool(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "skill",
  "slug": "weekly_report",
  "name": "周报",
  "description": "写周报时使用",
  "skill": {"entry": "SKILL.md"},
  "runtime": {"resident": true}
}`
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("skill 声明 resident 应该被拒绝")
	}
}

// 内置工具由 Go 实现，没有脚本进程可常驻。
func TestResidentRejectsBuiltin(t *testing.T) {
	raw := `{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "web_search",
  "name": "联网搜索",
  "description": "内置工具",
  "builtin": true,
  "input_schema": {"type": "object", "properties": {}},
  "runtime": {"resident": true}
}`
	if _, err := Parse([]byte(raw), false); err == nil {
		t.Fatal("builtin 工具声明 resident 应该被拒绝")
	}
}

// 远端写必须由作者显式声明：MCP 工具对平台是黑盒，没有 verbs 可推导，
// 不声明就按只读处理（远端鉴权是第一道闸），声明了才要求逐次确认。
func TestMCPWritesNeedsDeclaration(t *testing.T) {
	body := func(extra string) string {
		return `{
  "protocol": "wa-plugin/1",
  "kind": "mcp",
  "slug": "crm_mcp",
  "name": "CRM MCP",
  "description": "CRM 远端服务，查客户与订单时使用",
  "capability": "plugin.invoke",
  "transport": {"type": "http", "url": "https://mcp.example.com/sse"},
  "tools": {"allow": ["*"]` + extra + `}
}`
	}

	readOnly, err := Parse([]byte(body("")), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if readOnly.WritesRemote() {
		t.Fatal("未声明 tools.writes 时不应要求人工确认")
	}
	if risk, _ := Derive(readOnly); risk > policy.RiskReadCost {
		t.Fatalf("未声明写入的 MCP 至多按只读有成本处理，实际 L%d", risk)
	}

	writes, err := Parse([]byte(body(`, "writes": true`)), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !writes.WritesRemote() {
		t.Fatal("声明 tools.writes 后必须走人工确认")
	}
	if risk, _ := Derive(writes); risk < policy.RiskWrite {
		t.Fatalf("声明写入的 MCP 至少要 L%d，实际 L%d", policy.RiskWrite, risk)
	}
}

// tools 段只属于 mcp：写在别的 kind 上会被静默忽略，作者却以为声明生效了。
func TestToolsSectionOnlyForMCP(t *testing.T) {
	_, err := Parse([]byte(toolManifest(`, "tools": {"allow": ["*"], "writes": true}`)), false)
	if err == nil || !strings.Contains(err.Error(), "只对 mcp") {
		t.Fatalf("非 mcp 声明 tools 段应报错，实际: %v", err)
	}
}
