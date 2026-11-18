package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/tool"
)

func TestNormalizeConfig(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		// 清单是否声明 allow_http：安装级配置不自己放宽这个决定
		allowHTTP bool
		wantErr   bool
		check     func(t *testing.T, got map[string]string)
	}{
		{
			name:   "空值表示沿用清单",
			values: map[string]string{ConfigURL: "  ", ConfigSecret: ""},
			check: func(t *testing.T, got map[string]string) {
				if len(got) != 0 {
					t.Fatalf("空值应被裁剪，得到 %v", got)
				}
			},
		},
		{
			name:   "https 通过",
			values: map[string]string{ConfigURL: "https://mcp.example.com/mcp", ConfigSecret: "mcp-key"},
			check: func(t *testing.T, got map[string]string) {
				if got[ConfigURL] != "https://mcp.example.com/mcp" || got[ConfigSecret] != "mcp-key" {
					t.Fatalf("配置未保留：%v", got)
				}
			},
		},
		{
			name:   "本机 http 放行",
			values: map[string]string{ConfigURL: "http://127.0.0.1:3000/mcp"},
		},
		{
			name:    "非本机 http 拒绝",
			values:  map[string]string{ConfigURL: "http://mcp.example.com/mcp"},
			wantErr: true,
		},
		{
			name:      "清单允许明文后内网 http 通过",
			values:    map[string]string{ConfigURL: "http://crm.internal/mcp"},
			allowHTTP: true,
			check: func(t *testing.T, got map[string]string) {
				if got[ConfigURL] != "http://crm.internal/mcp" {
					t.Fatalf("配置未保留：%v", got)
				}
			},
		},
		{
			name:    "带用户名密码拒绝",
			values:  map[string]string{ConfigURL: "https://user:pass@mcp.example.com/mcp"},
			wantErr: true,
		},
		{
			name:    "未知键拒绝",
			values:  map[string]string{"dsn": "postgres://x"},
			wantErr: true,
		},
		{
			name:    "凭证名非法字符拒绝",
			values:  map[string]string{ConfigSecret: "a b"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := Manifest{Kind: KindMCP, Transport: &Transport{Type: "http"}}
			meta.Transport.AllowHTTP = tc.allowHTTP
			got, err := normalizeConfig(meta, tc.values)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// 安装级覆盖只作用于 MCP，并且不修改发布快照本身。
func TestApplyInstallConfigShape(t *testing.T) {
	meta := Manifest{
		Kind:      KindMCP,
		Transport: &Transport{Type: "http", URL: "https://published.example.com/mcp"},
		Auth:      &Auth{In: "header", Name: "Authorization", Secret: "published-key"},
	}
	raw, _ := json.Marshal(map[string]string{ConfigURL: "https://space.example.com/mcp"})

	// 直接验证合并规则，避免依赖 DB：applyInstallConfig 的取值语义与这里一致。
	values := configValues(meta, raw)
	if values[ConfigURL] != "https://space.example.com/mcp" {
		t.Fatalf("覆盖值未生效：%v", values)
	}
	if values[ConfigSecret] != "published-key" {
		t.Fatalf("未覆盖的字段应沿用清单：%v", values)
	}
	if meta.Transport.URL != "https://published.example.com/mcp" {
		t.Fatalf("原始清单不应被修改")
	}
}

// shell 的「需确认命令」名单是审批的细粒度入口：字段必须由清单给出，
// 卡片上的「配置」按钮也据此出现（Item.Configurable）。
func TestConfigFieldsCoverShellApproval(t *testing.T) {
	shell := Manifest{Kind: KindTool, Capability: CapShellExec}
	fields := configFields(shell)
	var cmdField *ConfigField
	for i := range fields {
		if fields[i].Key == ConfigApprovalCommands {
			cmdField = &fields[i]
		}
	}
	if cmdField == nil {
		t.Fatalf("shell 应给出审批命令名单字段，实际 %+v", fields)
	}
	if cmdField.Type != "list" {
		t.Fatalf("名单是多项内容，应为 list，实际 %s", cmdField.Type)
	}

	// 技能与组件的参数属于清单/内容，安装页不给改（等于绕过发布校验）
	for _, m := range []Manifest{
		{Kind: KindSkill, Capability: CapSkillUse},
		{Kind: KindComponent, Capability: CapPluginInvok},
	} {
		if got := configFields(m); len(got) != 0 {
			t.Fatalf("%s/%s 不该有安装级配置，实际 %+v", m.Kind, m.Capability, got)
		}
	}
}

// 没配过时回显平台默认名单：留空框会让人以为「没有名单」，而保存空值等于全不确认。
func TestConfigValuesFillsDefaultApproval(t *testing.T) {
	shell := Manifest{Kind: KindTool, Capability: CapShellExec}

	fresh := configValues(shell, nil)
	if fresh[ConfigApprovalCommands] == "" {
		t.Fatal("未配置时应回显平台默认名单，而不是空")
	}
	if !strings.Contains(fresh[ConfigApprovalCommands], "rm") {
		t.Fatalf("默认名单应含 rm，实际 %q", fresh[ConfigApprovalCommands])
	}

	// 显式清空（库里有这个键、值是空串）是用户的决定，不再回填
	cleared := configValues(shell, json.RawMessage(`{"approval_commands":""}`))
	if cleared[ConfigApprovalCommands] != "" {
		t.Fatalf("显式清空后应保持空，实际 %q", cleared[ConfigApprovalCommands])
	}

	// 自己配过就按自己的来
	custom := configValues(shell, json.RawMessage(`{"approval_commands":"rm, dropdb"}`))
	if custom[ConfigApprovalCommands] != "rm, dropdb" {
		t.Fatalf("应回显用户名单，实际 %q", custom[ConfigApprovalCommands])
	}
}

// 内置件只装在系统空间一份，它的配置对全部空间生效：提示文案不能说「只影响当前空间」。
func TestConfigScopeForBuiltin(t *testing.T) {
	if got := configScope(Manifest{Kind: KindTool, Builtin: true}); got != ScopeGlobal {
		t.Fatalf("内置件的配置是全局的，实际 %s", got)
	}
	if got := configScope(Manifest{Kind: KindMCP}); got != ScopeSpace {
		t.Fatalf("普通插件按空间隔离，实际 %s", got)
	}
}

// 审批档位：脚本工具与 MCP 能配，内置件不能（内置件的档位由 tool.builtinPolicy 读）。
func TestConfigFieldsIncludeApprovalLevel(t *testing.T) {
	script := Manifest{Kind: KindTool, Capability: CapPluginInvok}
	fields := configFields(script)
	if len(fields) != 1 || fields[0].Key != ConfigApprovalPolicy || fields[0].Type != "select" {
		t.Fatalf("脚本工具应有审批档位下拉，实际 %+v", fields)
	}
	if len(fields[0].Options) != 3 {
		t.Fatalf("档位应有三档可选，实际 %+v", fields[0].Options)
	}

	// 内置工具也能配档位：档位由 tool.builtinPolicy 从系统空间那份安装配置读，
	// 配置写在同一处，所以卡片上必须给入口——否则就会出现「看得到档位、
	// 找不到改的地方」的卡片（file_write / wiki_write 这类）。
	for _, builtin := range []Manifest{
		{Kind: KindTool, Capability: CapFSWrite, Builtin: true},
		{Kind: KindTool, Capability: "wiki.write", Builtin: true},
	} {
		if got := configFields(builtin); len(got) != 1 || got[0].Key != ConfigApprovalPolicy {
			t.Fatalf("内置工具 %s 应有审批档位字段，实际 %+v", builtin.Capability, got)
		}
	}

	// shell：档位 + 命令名单
	shellFields := configFields(Manifest{Kind: KindTool, Capability: CapShellExec})
	if len(shellFields) != 2 || shellFields[0].Key != ConfigApprovalPolicy || shellFields[1].Key != ConfigApprovalCommands {
		t.Fatalf("shell 应给出档位与命令名单，实际 %+v", shellFields)
	}
}

// 档位取值必须显式校验：把 off 写成 none 的人应该看到报错，而不是以为关掉了确认。
func TestNormalizeConfigApprovalPolicy(t *testing.T) {
	toolMeta := Manifest{Kind: KindTool, Capability: CapShellExec}
	if _, err := normalizeConfig(toolMeta, map[string]string{ConfigApprovalPolicy: "none"}); err == nil {
		t.Fatal("非法档位应被拒绝，而不是静默降级")
	}

	got, err := normalizeConfig(toolMeta, map[string]string{ConfigApprovalPolicy: tool.ApprovalOff})
	if err != nil || got[ConfigApprovalPolicy] != tool.ApprovalOff {
		t.Fatalf("off 应被接受，实际 %v（err=%v）", got, err)
	}

	// 空值落到默认档，而不是把关掉确认当成默认
	got, err = normalizeConfig(toolMeta, map[string]string{ConfigApprovalPolicy: ""})
	if err != nil || got[ConfigApprovalPolicy] != tool.ApprovalDeclared {
		t.Fatalf("空值应落到默认档，实际 %v（err=%v）", got, err)
	}
}

// 旧安装记录没有这个键：读出来必须是默认档，不能被一次解析失败改变行为。
func TestApprovalLevelOf(t *testing.T) {
	cases := map[string]string{
		"空记录":    tool.ApprovalDeclared,
		"没有键":    tool.ApprovalDeclared,
		"非法值":    tool.ApprovalDeclared,
		"always": tool.ApprovalAlways,
		"off":    tool.ApprovalOff,
	}
	raw := map[string]string{
		"空记录":    ``,
		"没有键":    `{"url":"https://mcp.example.com/mcp"}`,
		"非法值":    `{"approval_policy":"of"}`,
		"always": `{"approval_policy":"always"}`,
		"off":    `{"approval_policy":"off"}`,
	}
	for name, want := range cases {
		var msg json.RawMessage
		if r := raw[name]; r != "" {
			msg = json.RawMessage(r)
		}
		if got := ApprovalLevelOf(msg); got != want {
			t.Fatalf("%s：期望 %s，实际 %s", name, want, got)
		}
	}
}

// 档位没配过时下拉框要回显默认档，否则用户以为「没设过就是不拦」。
func TestConfigValuesShowsDefaultApprovalLevel(t *testing.T) {
	shell := Manifest{Kind: KindTool, Capability: CapShellExec}
	if got := configValues(shell, nil)[ConfigApprovalPolicy]; got != tool.ApprovalDeclared {
		t.Fatalf("未配置时应回显默认档，实际 %q", got)
	}
	custom := configValues(shell, json.RawMessage(`{"approval_policy":"off"}`))
	if custom[ConfigApprovalPolicy] != tool.ApprovalOff {
		t.Fatalf("应回显已配置的档位，实际 %q", custom[ConfigApprovalPolicy])
	}
}

// 清单声明的配置项追加在平台字段之后，默认值与选项原样透出。
func TestConfigFieldsIncludeManifestConfig(t *testing.T) {
	meta := Manifest{
		Kind: KindTool, Capability: "http.request",
		Config: []ConfigItem{
			{Key: "region", Label: "服务区域", Type: ConfigTypeSelect, Default: "cn",
				Options: []ConfigOption{{Value: "cn", Label: "中国大陆"}}},
			{Key: "retry", Label: "重试次数", Type: ConfigTypeNumber, Min: "0", Max: "5"},
		},
	}
	fields := configFields(meta)
	if len(fields) != 3 {
		t.Fatalf("应有 1 个平台字段 + 2 个清单字段，实际 %+v", fields)
	}
	if fields[0].Key != ConfigApprovalPolicy {
		t.Fatalf("平台字段应排在最前，实际首项是 %q", fields[0].Key)
	}
	if fields[1].Key != "region" || fields[1].Default != "cn" || len(fields[1].Options) != 1 {
		t.Fatalf("清单字段的默认值/选项没透出：%+v", fields[1])
	}
	if fields[2].Type != ConfigTypeNumber || fields[2].Label != "重试次数" {
		t.Fatalf("清单字段类型没透出：%+v", fields[2])
	}
}

// 清单字段按声明类型校验；可选字段清空表示回到默认，不落库。
func TestNormalizeConfigManifestFields(t *testing.T) {
	meta := Manifest{Kind: KindTool, Capability: "http.request", Config: []ConfigItem{
		{Key: "region", Label: "服务区域", Type: ConfigTypeSelect, Default: "cn",
			Options: []ConfigOption{{Value: "cn", Label: "中国大陆"}, {Value: "intl", Label: "国际"}}},
		{Key: "retry", Label: "重试次数", Type: ConfigTypeNumber, Min: "0", Max: "5"},
		{Key: "flag", Label: "开关", Type: ConfigTypeBoolean},
	}}

	got, err := normalizeConfig(meta, map[string]string{"region": "intl", "retry": "3", "flag": "true"})
	if err != nil {
		t.Fatalf("合法值不应报错：%v", err)
	}
	if got["region"] != "intl" || got["retry"] != "3" || got["flag"] != "true" {
		t.Fatalf("合法值没保存：%v", got)
	}

	bad := []map[string]string{
		{"region": "mars"}, // 不在选项里
		{"retry": "9"},     // 超过 max
		{"retry": "3.5"},   // 不是整数
		{"flag": "yes"},    // 不是布尔
		{"unknown": "x"},   // 清单没声明
	}
	for _, values := range bad {
		if _, err := normalizeConfig(meta, values); err == nil {
			t.Fatalf("%v 应被拒绝", values)
		}
	}

	// 可选字段清空 = 回到声明里的默认值（运行时生效值就是 Default），不落库
	got, err = normalizeConfig(meta, map[string]string{"region": ""})
	if err != nil || len(got) != 0 {
		t.Fatalf("空值不应落库，实际 %v（err=%v）", got, err)
	}
}

// 没配过时回显默认值：空框会让人以为「这个参数现在是空的」。
func TestConfigValuesUsesManifestDefault(t *testing.T) {
	meta := Manifest{Kind: KindTool, Capability: "http.request", Config: []ConfigItem{
		{Key: "region", Label: "服务区域", Type: ConfigTypeText, Default: "cn"},
		{Key: "retry", Label: "重试次数", Type: ConfigTypeNumber},
	}}
	values := configValues(meta, nil)
	if values["region"] != "cn" {
		t.Fatalf("未配置时应回显默认值，实际 %q", values["region"])
	}
	if _, ok := values["retry"]; !ok {
		t.Fatal("没有默认值的字段也要出现（值为空串），否则前端渲染不出这一项")
	}

	stored := configValues(meta, json.RawMessage(`{"region":"intl"}`))
	if stored["region"] != "intl" {
		t.Fatalf("应回显已配置值，实际 %q", stored["region"])
	}
}
