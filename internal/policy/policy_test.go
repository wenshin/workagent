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
		hasBind     bool
		allowWrite  bool
		wantAllow   bool
		wantApprove bool
		wantDenied  bool
	}{
		{name: "空间内写_manage", perm: "manage", sideEffects: SideEffectsWorkspace, wantAllow: true},
		{name: "空间内写_write", perm: "write", sideEffects: SideEffectsWorkspace, wantAllow: true},
		{name: "空间内写_viewer拒绝", perm: "view", sideEffects: SideEffectsWorkspace, wantDenied: true},
		{name: "空间外写_无绑定转审批", perm: "manage", sideEffects: SideEffectsExternal, wantApprove: true},
		{name: "空间外写_有绑定未授权转审批", perm: "manage", sideEffects: SideEffectsExternal, hasBind: true, wantApprove: true},
		{name: "空间外写_绑定且授权放行", perm: "manage", sideEffects: SideEffectsExternal, hasBind: true, allowWrite: true, wantAllow: true},
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
				HasBind:     tc.hasBind,
				Binding:     Constraints{AllowWrite: tc.allowWrite},
			})

			var denied *ErrDenied
			switch {
			case tc.wantDenied:
				if !errors.As(err, &denied) {
					t.Fatalf("期望拒绝，实际 err=%v dec=%+v", err, dec)
				}
			case tc.wantApprove:
				if err != nil || !dec.NeedApproval {
					t.Fatalf("期望转审批，实际 err=%v dec=%+v", err, dec)
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
	dec, err := RBACEngine{}.Check(context.Background(), Request{
		Tool: "fs.read", Risk: RiskReadOnly, SpaceID: "sp1", SpacePerm: "view",
	})
	if err != nil || !dec.Allow {
		t.Fatalf("只读工具应对空间成员放行: err=%v dec=%+v", err, dec)
	}
}
