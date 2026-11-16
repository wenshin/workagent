package policy

import (
	"context"
	"errors"
	"testing"
)

func TestRBACEngineWorkspaceWrite(t *testing.T) {
	engine := RBACEngine{}
	ctx := context.Background()

	cases := []struct {
		name        string
		perm        string
		sideEffects string
		wantAllow   bool
		wantDenied  bool
	}{
		{name: "空间内写_manage", perm: "manage", sideEffects: SideEffectsWorkspace, wantAllow: true},
		{name: "空间内写_write", perm: "write", sideEffects: SideEffectsWorkspace, wantAllow: true},
		{name: "空间内写_viewer拒绝", perm: "view", sideEffects: SideEffectsWorkspace, wantDenied: true},
		// fs 写不再逐次审批：范围由工具锁死在当前空间，删错还有回收站与审计
		{name: "空间外写_默认放行", perm: "manage", sideEffects: SideEffectsExternal, wantAllow: true},
		{name: "共享区写_write", perm: "write", sideEffects: SideEffectsShared, wantAllow: true},
		{name: "非成员一律拒绝", perm: "", sideEffects: SideEffectsWorkspace, wantDenied: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := engine.Check(ctx, Request{
				Actor:       "u1",
				Action:      "tool:invoke",
				Tool:        "fs.write",
				Risk:        RiskWrite,
				SpaceID:     "sp1",
				SpacePerm:   tc.perm,
				SideEffects: tc.sideEffects,
			})

			var denied *ErrDenied
			switch {
			case tc.wantDenied:
				if !errors.As(err, &denied) {
					t.Fatalf("期望拒绝，实际 err=%v dec=%+v", err, dec)
				}
			case tc.wantAllow:
				if err != nil || !dec.Allow {
					t.Fatalf("期望放行，实际 err=%v dec=%+v", err, dec)
				}
			}
		})
	}
}

func TestRBACEngineReadOnly(t *testing.T) {
	engine := RBACEngine{}

	for _, risk := range []int{RiskReadOnly, RiskReadCost} {
		dec, err := engine.Check(context.Background(), Request{
			Tool: "fs.read", Risk: risk, SpaceID: "sp1", SpacePerm: "view",
		})
		if err != nil || !dec.Allow {
			t.Fatalf("只读工具应对空间成员放行(risk=%d): err=%v dec=%+v", risk, err, dec)
		}
	}
}

// 审批只看动作，不看等级：高敏但没命中动作时直接放行，等级只用于高亮。
func TestRBACEngineSensitiveWithoutActionAllowed(t *testing.T) {
	dec, err := RBACEngine{}.Check(context.Background(), Request{
		Tool: "db_dump", Risk: RiskSensitive, SpaceID: "sp1", SpacePerm: "manage",
	})
	if err != nil || !dec.Allow {
		t.Fatalf("没有审批动作时应放行: err=%v dec=%+v", err, dec)
	}
}

// 命中动作才审批，且不看空间权限（manage 也要确认）。
func TestRBACEngineNeedsApprovalByAction(t *testing.T) {
	action := &Action{Kind: ActionNetwork, Key: ActionNetwork, Reason: "出网写"}
	dec, err := RBACEngine{}.Check(context.Background(), Request{
		Tool: "crm_update", Risk: RiskWrite, SpaceID: "sp1", SpacePerm: "manage",
		Approval: action,
	})
	if err != nil || !dec.NeedApproval || dec.Action == nil {
		t.Fatalf("命中动作应审批: err=%v dec=%+v", err, dec)
	}
	if dec.Action.Kind != ActionNetwork {
		t.Fatalf("动作类别传递错误: %+v", dec.Action)
	}
}
