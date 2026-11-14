package tool

import (
	"context"
	"testing"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/store"
)

// 内置工具不需要 tool_binding。
//
// plugin_try 声明为 L2（试跑会真实出网/进沙箱），策略对 L2 要求显式绑定，
// 而绑定是给「资源型工具」授权数据范围用的：内置件随平台发版、代码受控，
// 也没有按空间配置的入口。之前没有这条豁免，plugin_try 对所有空间都判成
// 「需要管理员绑定后才能使用」，整条插件开发闭环不可用。
func TestApplyBindingsTreatsBuiltinAsBound(t *testing.T) {
	tools := []Tool{
		{Name: "plugin_try", Runtime: RuntimeBuiltin, Risk: policy.RiskReadCost},
		{Name: "crm_lookup", Runtime: "plugin"},
		{Name: "crm.query", Runtime: "mcp", ServerName: "crm", Capability: "query"},
	}
	bindings := []store.ToolBinding{
		{ToolName: "crm/query", Constraints: []byte(`{"max_rows":5}`)},
	}

	applyBindings(tools, bindings)

	if !tools[0].HasBinding {
		t.Fatal("内置工具应视为已绑定")
	}
	if tools[1].HasBinding {
		t.Fatal("没有绑定的插件工具不应被放行")
	}
	if !tools[2].HasBinding {
		t.Fatal("MCP 工具按「服务名/能力」绑定后应放行")
	}
	if tools[2].Constraints.MaxRows != 5 {
		t.Fatalf("绑定约束没生效: %+v", tools[2].Constraints)
	}
}

// 豁免只放开「有没有绑定」这一项：写操作仍要 allow_write，高敏仍要审批。
func TestBuiltinBindingExemptionKeepsWriteAndSensitiveGuarded(t *testing.T) {
	engine := policy.RBACEngine{}
	base := policy.Request{SpaceID: "s1", SpacePerm: "manage", HasBind: true}

	readCost := base
	readCost.Tool, readCost.Risk = "plugin_try", policy.RiskReadCost
	if dec, err := engine.Check(context.Background(), readCost); err != nil || !dec.Allow {
		t.Fatalf("L2 内置工具应放行: dec=%+v err=%v", dec, err)
	}

	externalWrite := base
	externalWrite.Tool, externalWrite.Risk = "crm_write", policy.RiskWrite
	externalWrite.SideEffects = policy.SideEffectsExternal
	if dec, err := engine.Check(context.Background(), externalWrite); err != nil || !dec.NeedApproval {
		t.Fatalf("外部写仍应转审批: dec=%+v err=%v", dec, err)
	}

	sensitive := base
	sensitive.Tool, sensitive.Risk = "db_dump", policy.RiskSensitive
	if dec, err := engine.Check(context.Background(), sensitive); err != nil || !dec.NeedApproval {
		t.Fatalf("高敏仍应审批: dec=%+v err=%v", dec, err)
	}
}
