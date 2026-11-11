// Package policy 实现 ToolInvoker 的授权策略（技术方案 §5.6.1 策略引擎、§2.2 默认拒绝）。
package policy

import (
	"context"
	"encoding/json"
	"fmt"
)

// RiskLevel 与 capability.risk_level 对齐。
const (
	// RiskReadOnly 只读无副作用（列目录、查时间），默认放行。
	RiskReadOnly = 1
	// RiskReadCost 只读但消耗资源或涉及数据范围（检索、SQL 查询），需要显式绑定。
	RiskReadCost = 2
	// RiskWrite 写操作，必须绑定且二次确认。
	RiskWrite = 3
	// RiskSensitive 高敏（凭证、外发），一律走审批。
	RiskSensitive = 4
)

// SideEffects 描述一次调用的影响范围，决定写操作能否走「空间工作区」放行。
const (
	// SideEffectsReadOnly 无副作用。
	SideEffectsReadOnly = "read-only"
	// SideEffectsWorkspace 只改当前空间 workdir 内的数据（如 fs.write），
	// 范围由工具自身约束，授权只看空间成员权限。
	SideEffectsWorkspace = "write-workspace"
	// SideEffectsShared 只改跨空间的共享区（如企业 wiki 固定在系统空间的 wiki/ 下）。
	// 范围同样由工具锁死，授权沿用「在当前空间能否写」：
	// 能写当前空间的成员，才有资格往共享区沉淀内容。
	SideEffectsShared = "write-shared"
	// SideEffectsExternal 会改动空间外的资源（外部系统、凭证等），一律走审批。
	SideEffectsExternal = "write-external"
)

// Constraints 是绑定上的执行约束。
type Constraints struct {
	MaxRows          int  `json:"max_rows,omitempty"`
	MaxBytes         int  `json:"max_bytes,omitempty"`
	ApprovalRequired bool `json:"approval_required,omitempty"`
	AllowWrite       bool `json:"allow_write,omitempty"`
}

// ParseConstraints 解析绑定上的 jsonb 约束；空值返回零值。
func ParseConstraints(raw []byte) Constraints {
	var c Constraints
	if len(raw) == 0 {
		return c
	}
	_ = json.Unmarshal(raw, &c)
	return c
}

// Request 是一次授权判定的输入。
type Request struct {
	Actor     string
	Action    string // 目前只有 tool:invoke
	Tool      string
	Risk      int
	SpaceID   string
	SpacePerm string // 调用方在该空间的权限，空表示不是成员
	// SideEffects 来自工具声明，写操作据此区分「空间工作区」与「外部资源」
	SideEffects string
	HasBind     bool
	Binding     Constraints
}

// Decision 是判定结果。
type Decision struct {
	Allow        bool
	NeedApproval bool
	Reason       string
	Constraints  Constraints
}

// Engine 是 PDP 接口：默认实现为 RBAC + 风险分级，后续可替换为外部策略服务。
type Engine interface {
	Check(ctx context.Context, req Request) (Decision, error)
}

// ErrDenied 表示策略拒绝。
type ErrDenied struct{ Reason string }

func (e *ErrDenied) Error() string { return "策略拒绝：" + e.Reason }

// ErrNeedApproval 表示需要审批后才能执行。
type ErrNeedApproval struct{ Reason string }

func (e *ErrNeedApproval) Error() string { return "需要审批：" + e.Reason }

// RBACEngine 是默认策略引擎，规则按风险等级递进：
//
//	L1 只读无副作用 → 放行（仍需空间成员身份）
//	L2 只读有成本   → 必须有显式绑定
//	L3 写操作       → 绑定 + constraints.allow_write，否则转审批
//	L4 高敏感       → 一律审批
type RBACEngine struct{}

var _ Engine = RBACEngine{}

// Check 实现 Engine。
func (RBACEngine) Check(_ context.Context, req Request) (Decision, error) {
	deny := func(reason string) (Decision, error) {
		return Decision{}, &ErrDenied{Reason: reason}
	}

	// 非空间成员一律拒绝，内置能力也不例外
	if req.SpacePerm == "" {
		return deny(fmt.Sprintf("不是空间成员，无法使用工具 %s", req.Tool))
	}

	constraints := req.Binding
	if constraints.MaxRows <= 0 {
		constraints.MaxRows = 200
	}
	if constraints.MaxBytes <= 0 {
		constraints.MaxBytes = 32 * 1024
	}

	switch {
	case req.Risk >= RiskSensitive:
		return Decision{NeedApproval: true, Reason: "高敏工具需审批", Constraints: constraints}, nil
	case req.Risk >= RiskWrite:
		// 工作区 / 共享区写入：范围已由工具限制死（前者当前空间根，后者固定前缀），
		// 因此由空间权限直接决定，不再逐次要求绑定与人工审批。
		if req.SideEffects == SideEffectsWorkspace || req.SideEffects == SideEffectsShared {
			if req.SpacePerm == "write" || req.SpacePerm == "manage" {
				reason := "空间工作区写入"
				if req.SideEffects == SideEffectsShared {
					reason = "共享知识区写入"
				}
				return Decision{Allow: true, Reason: reason, Constraints: constraints}, nil
			}
			return deny(fmt.Sprintf("你在该空间没有写权限，无法执行 %s", req.Tool))
		}
		if !req.HasBind || !constraints.AllowWrite {
			return Decision{NeedApproval: true, Reason: "写操作未授权", Constraints: constraints}, nil
		}
		return Decision{Allow: true, Reason: "写操作已授权", Constraints: constraints}, nil
	case req.Risk >= RiskReadCost:
		if !req.HasBind {
			return deny(fmt.Sprintf("工具 %s 涉及数据范围，需要管理员绑定后才能使用", req.Tool))
		}
		return Decision{Allow: true, Reason: "只读能力已绑定", Constraints: constraints}, nil
	default:
		return Decision{Allow: true, Reason: "只读无副作用", Constraints: constraints}, nil
	}
}
