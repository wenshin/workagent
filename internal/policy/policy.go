// Package policy 实现 ToolInvoker 的授权策略（技术方案 §5.6.1 策略引擎、§2.2 默认拒绝）。
package policy

import (
	"context"
	"fmt"
)

// RiskLevel 与 capability.risk_level 对齐。
//
// 注意：风险等级**只用于展示**（卡片色块、事后 review），不参与审批判定。
// 是否弹审批由 Action 与工具/插件配置决定，避免「等级 × 副作用」的二维矩阵。
const (
	// RiskReadOnly 只读无副作用（列目录、查时间）。
	RiskReadOnly = 1
	// RiskReadCost 只读但消耗资源或涉及数据范围（检索、SQL 查询、MCP 远端工具）。
	RiskReadCost = 2
	// RiskWrite 写操作（含空间工作区、共享区、外部系统）。
	RiskWrite = 3
	// RiskSensitive 高敏（凭证、外发）。
	RiskSensitive = 4
)

// SideEffects 描述一次调用的影响范围，用于判定「能不能按空间权限直接放行」。
const (
	// SideEffectsReadOnly 无副作用。
	SideEffectsReadOnly = "read-only"
	// SideEffectsWorkspace 只改当前空间 workdir 内的数据（如 fs.write），
	// 范围由工具自身约束，授权只看空间成员权限。
	SideEffectsWorkspace = "write-workspace"
	// SideEffectsShared 只改跨空间的共享区（如企业 wiki 固定在系统空间的 wiki/ 下）。
	SideEffectsShared = "write-shared"
	// SideEffectsExternal 会改动空间外的资源（外部系统、凭证等）。
	SideEffectsExternal = "write-external"
)

// 需要审批的动作类别。都是「不可逆且平台无法替用户判断」的动作：
const (
	// ActionNetwork 出网且方法不是 GET/HEAD：外部系统可能已经收到这次改动。
	ActionNetwork = "net.write"
	// ActionShell 命中需审批命令名单的 shell 命令。
	ActionShell = "shell"
	// ActionPlugin 插件的审批级别被设成「每次调用都确认」：与调用内容无关，
	// 是安装方对该插件的整体要求（审计里要能和前两类区分开）。
	ActionPlugin = "plugin.approval"
)

// Action 是一次调用命中的审批动作，由 Invoker 按工具声明与实参算出。
type Action struct {
	Kind   string
	Detail string
	// Key 是本轮记忆的键：同一类动作（或同一条命令）在本轮内只问一次。
	Key string
	// Reason 是给用户看的一句话，说明为什么要确认。
	Reason string
	// Covers 是这次确认同时覆盖的其它动作键（含 Key 本身）。
	//
	// 一条命令可能既命中需确认命令名单、又需要联网准入：两件事合成一张卡、点一次
	// 就够，但执行时必须知道「这次批的包含给网」，否则用户批了联网还要再被问一次。
	Covers []string
}

// CoversKey 表示这次确认是否覆盖了动作键 k（Key 本身或 Covers 里的任一项）。
func (a *Action) CoversKey(k string) bool {
	if a == nil || k == "" {
		return false
	}
	if a.Key == k {
		return true
	}
	for _, c := range a.Covers {
		if c == k {
			return true
		}
	}
	return false
}

// Request 是一次授权判定的输入。
type Request struct {
	Actor       string
	Action      string // 目前只有 tool:invoke
	Tool        string
	Plugin      string
	Risk        int
	SpaceID     string
	SpacePerm   string // 调用方在该空间的权限，空表示不是成员
	SideEffects string // 来自工具声明
	// Approval 非空表示这次调用命中审批规则；为空表示直接执行（只做成员校验）。
	Approval *Action
}

// Decision 是判定结果。
type Decision struct {
	Allow        bool
	NeedApproval bool
	Reason       string
	Action       *Action
	// Risk 回传展示用等级，前端据此给卡片配色。
	Risk int
}

// Engine 是 PDP 接口：默认实现为 RBAC + 动作审批，后续可替换为外部策略服务。
type Engine interface {
	Check(ctx context.Context, req Request) (Decision, error)
}

// ErrDenied 表示策略拒绝。
type ErrDenied struct{ Reason string }

func (e *ErrDenied) Error() string { return "策略拒绝：" + e.Reason }

// ErrNeedApproval 表示需要审批后才能执行。
type ErrNeedApproval struct{ Reason string }

func (e *ErrNeedApproval) Error() string { return "需要审批：" + e.Reason }

// RBACEngine 是默认策略引擎。规则只有三条：
//
//  1. 非空间成员 → 拒绝（唯一硬拦）
//  2. 空间内写（workspace / shared）且只有 view → 拒绝
//  3. 命中审批动作 → 转审批；其余一律放行，只带风险等级供高亮
//
// fs 写不做审批：写入范围被工具锁死在当前空间，删错了也有回收站与审计；
// 出网写与命中名单的命令才需要人看一眼。
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

	// 空间内写仍然按空间权限判定：view 成员不能借「不审批」把文件改掉。
	if req.Risk >= RiskWrite &&
		(req.SideEffects == SideEffectsWorkspace || req.SideEffects == SideEffectsShared) &&
		req.SpacePerm != "write" && req.SpacePerm != "manage" {
		return deny(fmt.Sprintf("你在该空间没有写权限，无法执行 %s", req.Tool))
	}

	if req.Approval != nil {
		reason := req.Approval.Reason
		if reason == "" {
			reason = "该操作需要人工确认"
		}
		return Decision{NeedApproval: true, Reason: reason, Action: req.Approval, Risk: req.Risk}, nil
	}

	return Decision{Allow: true, Reason: "放行", Risk: req.Risk}, nil
}
