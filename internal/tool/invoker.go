package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/store"
)

// InvokeStatus 是一次工具调用的结局。
const (
	StatusOK       = "ok"
	StatusDenied   = "denied"
	StatusApproval = "approval"
	StatusError    = "error"
)

// InvokeResult 是执行链的产出。
type InvokeResult struct {
	Content   string
	Status    string
	LatencyMS int64
	Risk      int
	ToolName  string
	// Cached 表示这条结果来自续跑回放，而不是这次真的执行了一遍。
	// 前端与审计据此区分「做过」与「又做了一次」。
	Cached bool
	// Parked 表示这次调用没执行：它命中了审批，本轮停在这里等人确认。
	//
	// 它不是一条工具回执，不落库、也不进消息序列——调用本身留在断点的
	// Pending 里，用户点同意后由续跑把它重新发出来（结论按参数复用）。
	Parked bool
	// Args 是本次调用的原始参数，落库时一并写入（续跑按参数认调用）。
	Args json.RawMessage
}

// Recorder 是 Invoker 需要的最小落库能力：工具调用记录 + 审计。
// 收窄成两个方法后，黄金用例与单元测试可以注入内存实现，
// 不必为了跑一次工具循环伪造整个 Store。
type Recorder interface {
	RecordToolCall(ctx context.Context, r store.ToolCallRecord) error
	AppendAuditLog(ctx context.Context, e store.AuditEntry) error
}

// Invoker 是 ToolInvoker（§5.7.7），把一次工具调用收敛成固定流程：
//
//	参数校验 → PDP 授权 → 审批检查 → 执行（超时保护）→ 脱敏 → 截断 → 审计
type Invoker struct {
	engine  policy.Engine
	rec     Recorder
	log     *slog.Logger
	timeout time.Duration
	// maxArgBytes 是单次调用参数上限，0 表示用 DefaultMaxArgBytes(0)。
	// 由装配层按 LLM_MAX_TOKENS 设置，见 SetMaxArgBytes。
	maxArgBytes int
}

// NewInvoker 构造执行器；engine 为空时使用默认 RBAC 策略，rec 为空时只告警不落库。
func NewInvoker(engine policy.Engine, rec Recorder, log *slog.Logger) *Invoker {
	if engine == nil {
		engine = policy.RBACEngine{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Invoker{engine: engine, rec: rec, log: log, timeout: 30 * time.Second}
}

// SetMaxArgBytes 设置单次调用参数上限（<=0 表示用 DefaultMaxArgBytes(0)）。
// 装配层应传 DefaultMaxArgBytes(cfg.LLMMaxTokens)。
func (i *Invoker) SetMaxArgBytes(n int) { i.maxArgBytes = n }

// Invoke 执行一次工具调用。策略拒绝与需要审批不会中断对话，而是作为结果回喂模型。
func (i *Invoker) Invoke(ctx context.Context, t Tool, call llm.ToolCall, env Env) InvokeResult {
	result := InvokeResult{ToolName: t.Name, Risk: t.Risk}
	start := time.Now()

	args := json.RawMessage(call.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return i.finish(ctx, env, result, start, StatusError, "工具参数不是合法 JSON")
	}
	if limit := i.argLimit(); len(args) > limit {
		return i.finish(ctx, env, result, start, StatusError, argTooLarge(t.Name, len(args), limit))
	}
	result.Args = args
	// 展示等级按这次实参细化（如 shell_exec 按命令判只读/写入）。
	// 下面策略判定仍用声明的 t.Risk：「这条命令看起来只读」不能绕过空间写权限。
	result.Risk = t.EffectiveRisk(args)

	// 审批动作在这里算：只有它需要看实参（命令、出网方法），策略层只做成员校验。
	action := ApprovalFor(t, args)
	decision, err := i.engine.Check(ctx, policy.Request{
		Actor:       env.UserID,
		Action:      "tool:invoke",
		Tool:        t.Name,
		Plugin:      t.ServerName,
		Risk:        t.Risk,
		SpaceID:     env.SpaceID,
		SpacePerm:   env.SpacePerm,
		SideEffects: t.SideEffects,
		Approval:    action,
	})
	if err != nil {
		var denied *policy.ErrDenied
		if errors.As(err, &denied) {
			return i.finish(ctx, env, result, start, StatusDenied, denied.Error())
		}
		return i.finish(ctx, env, result, start, StatusError, err.Error())
	}
	// 续跑回放：这一次调用在中断前已经产生过结果，副作用不能再发生一遍。
	// 放在策略之后：成员与写权限的校验不该因为「是回放」而被跳过。
	if cached, ok := env.Replay[ArgsHash(t.Name, args)]; ok {
		i.log.Info("回放中断前已完成的工具结果",
			"tool", t.Name, "run_id", env.RunID, "status", cached.Status)
		replayed := result
		replayed.Cached = true
		if cached.Risk > 0 {
			// 徽标沿用当初记录的等级：历史回看与实际执行的展示一致
			replayed.Risk = cached.Risk
		}
		return i.finish(ctx, env, replayed, start, cached.Status, cached.Content)
	}

	if t.Invoke == nil {
		return i.finish(ctx, env, result, start, StatusError, "工具未实现")
	}

	// shell 的网络准入：判据在 ShellNetPlan（工具挂在 NetPlan 上把参数换算成策略），
	// 编排在这里——工具内部等人点按钮会被下面 30s 的墙钟掐断。
	// granted 表示这次落给沙箱的 Spec.Network。
	plan := ShellNetPolicy{}
	if t.NetPlan != nil {
		plan = t.NetPlan(args)
	}
	netKey := ""
	if plan.NeedsApproval() {
		netKey = shellNetApprovalKey(t.Name, args)
	}
	granted := plan.Mode == ShellNetOn
	deniedNote := ""

	if decision.NeedApproval {
		approved, reason, parked := i.askApproval(ctx, t, call, env, decision)
		if parked {
			// 审批是会话的暂停点：这次调用不执行、不产回执，本轮到此为止。
			// 调用留在断点的 Pending 里，用户点同意后由续跑重新发出来。
			result.Parked = true
			return result
		}
		if approved {
			granted = true
		} else {
			// 只有「用户拒的是联网」且策略允许降级时才继续：网络工具拒绝即不执行，
			// 装依赖 + 脚本则按无网跑完（用户拒的是网，不是这个任务）。
			netDenied := netKey != "" && decision.Action != nil && decision.Action.Key == netKey
			if !netDenied || plan.OnDeny == DenyRefuse {
				return i.finish(ctx, env, result, start, StatusDenied, reason+"（未执行）")
			}
			deniedNote = reason + "；本次按无网执行。\n"
		}
	} else if netKey != "" && NormalizeApprovalLevel(t.ApprovalLevel) == ApprovalOff {
		// 安装方显式关掉确认：网络准入视为已批准，否则这条命令永远拿不到网
		granted = true
	}

	out, runErr := i.execute(ctx, t, args, env, granted)
	if runErr != nil {
		var need errNetNeeded
		if errors.As(runErr, &need) {
			// 纯脚本因断网失败：按规则申请联网重跑一次（审批在墙钟之外）。
			// 这次许可同样可能是暂停点：那这次调用的结果就整体延后，不留半截无网回执。
			retried, parked := i.escalateNetwork(ctx, t, call, env, args, out)
			if parked {
				result.Parked = true
				return result
			}
			out = retried
		} else if errors.Is(runErr, context.DeadlineExceeded) {
			return i.finish(ctx, env, result, start, StatusError, fmt.Sprintf("工具执行超时（%s）", i.timeout))
		} else if errors.Is(runErr, context.Canceled) {
			return i.finish(ctx, env, result, start, StatusError, "调用已取消")
		} else {
			i.log.Warn("工具执行失败", "tool", t.Name, "err", runErr)
			return i.finish(ctx, env, result, start, StatusError, "工具执行失败："+runErr.Error())
		}
	}
	if deniedNote != "" {
		out = deniedNote + out
	}

	out = Mask(out)
	// 这里是回填前的硬上限；读类工具更大的额度由 Tool.MaxResultBytes 在
	// 提示词侧另行控制（见 agent.resultBudget）。
	out = clamp(out, clampMaxBytes)
	return i.finish(ctx, env, result, start, StatusOK, out)
}

// askApproval 取用户决策。返回 false 时 reason 是要回给模型的说明；
// parked 为 true 表示本轮停在这里等人确认，调用方应当整体结束这一轮。
//
// 没有审批通道时按拒绝处理：命中规则却没人能批，静默执行才是真正危险的默认值。
func (i *Invoker) askApproval(
	ctx context.Context,
	t Tool,
	call llm.ToolCall,
	env Env,
	decision policy.Decision,
) (allow bool, reason string, parked bool) {
	if decision.Action == nil {
		return false, fmt.Sprintf("调用 %s 需要人工确认，但没有说明确认什么，已拒绝执行", t.Name), false
	}
	return i.decide(ctx, t, env, ApprovalRequest{
		ID:        ApprovalID(env.RunID, call.ID),
		RunID:     env.RunID,
		SessionID: env.SessionID,
		CallID:    call.ID,
		Tool:      t.Name,
		Kind:      decision.Action.Kind,
		Key:       decision.Action.Key,
		ArgsHash:  ArgsHash(t.Name, json.RawMessage(call.Function.Arguments)),
		Detail:    decision.Action.Detail,
		Reason:    decision.Reason,
		// 确认卡上的徽标与工具卡一致：同样按这次实参细化
		Risk: t.EffectiveRisk(json.RawMessage(call.Function.Arguments)),
	})
}

// decide 把一条审批请求交给通道，并统一处理暂停 / 中断 / 拒绝的措辞。
func (i *Invoker) decide(ctx context.Context, t Tool, env Env, req ApprovalRequest) (bool, string, bool) {
	if env.Approvals == nil {
		return false, fmt.Sprintf("调用 %s 需要人工确认（%s），但审批通道未配置，已拒绝执行", t.Name, req.Reason), false
	}
	d, err := env.Approvals.Decide(ctx, req)
	switch {
	case errors.Is(err, ErrApprovalParked):
		// 本轮到此为止：这不是结论，别翻译成拒绝记进回执
		return false, "", true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false, "等待确认时这一轮被中断，这次调用没有执行", false
	case err != nil:
		return false, "等待确认失败：" + err.Error(), false
	}
	if d.Allow {
		return true, "", false
	}
	reason := d.Reason
	if reason == "" {
		reason = "用户拒绝了这次调用"
	}
	return false, reason, false
}

// execute 跑一次工具，并告知它这次是否已获准联网。
//
// 每次尝试单独计时：升级重跑是第二次执行，不该和前一次共享 30s 预算。
func (i *Invoker) execute(ctx context.Context, t Tool, args json.RawMessage, env Env, netGrant bool) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	env.NetGrant = netGrant
	return t.Invoke(runCtx, args, env)
}

// escalateNetwork 实现「只有脚本时先无网跑，失败且像网络问题再申请联网重跑」。
//
// offline 是那次无网执行的回执：拿不到许可就把它原样回给模型（并注明仍是无网），
// 批准后才真的重跑一次——所以拒绝的代价只是一次失败，不是一条被放行的命令。
// parked 为 true 表示这次许可还在等人：无网回执先不发，等续跑重放这一条调用。
func (i *Invoker) escalateNetwork(
	ctx context.Context,
	t Tool,
	call llm.ToolCall,
	env Env,
	args json.RawMessage,
	offline string,
) (string, bool) {
	// 没人可问的情况：安装方关掉了确认，或平台没有审批通道。保持无网结果。
	if NormalizeApprovalLevel(t.ApprovalLevel) == ApprovalOff || env.Approvals == nil {
		return offline + ShellNetRetryDeniedNote, false
	}
	approved, reason, parked := i.decide(ctx, t, env, ApprovalRequest{
		// 与首次审批不同 ID：同一次调用的两种许可（先问装依赖、后问联网重跑）
		// 不能互相覆盖，否则第二次点击会落在已终态的记录上。
		ID:        ApprovalID(env.RunID, call.ID) + ":net",
		RunID:     env.RunID,
		SessionID: env.SessionID,
		CallID:    call.ID,
		Tool:      t.Name,
		Kind:      policy.ActionShell,
		Key:       shellNetRetryKey(t.Name, args),
		ArgsHash:  ArgsHash(t.Name, args) + ":net",
		Detail:    shellCommandOf(args),
		Reason:    ShellNetRetryReason,
		Risk:      t.EffectiveRisk(args),
	})
	if parked {
		return "", true
	}
	if !approved {
		return offline + reason + ShellNetRetryDeniedNote, false
	}
	retried, err := i.execute(ctx, t, args, env, true)
	if err != nil {
		i.log.Warn("联网重跑失败", "tool", t.Name, "err", err)
		return offline + "\n[提示] 用户批准了联网重跑，但重跑失败：" + err.Error() + "\n", false
	}
	return retried + ShellNetRetryDoneNote, false
}

// ApprovalID 由 run 与 call 派生稳定 ID：前端点按钮时原样回传。
func ApprovalID(runID, callID string) string {
	if callID == "" {
		callID = "anonymous"
	}
	if runID == "" {
		return "ap_" + callID
	}
	return "ap_" + runID + "_" + callID
}

func (i *Invoker) finish(
	ctx context.Context,
	env Env,
	result InvokeResult,
	start time.Time,
	status, content string,
) InvokeResult {
	result.Status = status
	result.Content = content
	result.LatencyMS = time.Since(start).Milliseconds()

	// 落库与审计失败不影响对话本身，只告警
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	if i.rec == nil {
		return result
	}
	args := result.Args
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	// 结果一起落库：续跑靠它回放，而不是把副作用再做一遍
	if err := i.rec.RecordToolCall(recordCtx, store.ToolCallRecord{
		SessionID: env.SessionID,
		RunID:     env.RunID,
		ToolName:  result.ToolName,
		Args:      args,
		Status:    status,
		LatencyMS: int(result.LatencyMS),
		RiskLevel: result.Risk,
		Result:    content,
	}); err != nil {
		i.log.Warn("记录工具调用失败", "tool", result.ToolName, "err", err)
	}

	detail, _ := json.Marshal(map[string]any{
		"tool":       result.ToolName,
		"status":     status,
		"latency_ms": result.LatencyMS,
		"risk":       result.Risk,
	})
	if err := i.rec.AppendAuditLog(recordCtx, store.AuditEntry{
		Actor:  env.UserID,
		Action: "tool:invoke",
		Object: result.ToolName,
		Result: status,
		Detail: detail,
	}); err != nil {
		i.log.Warn("写审计日志失败", "tool", result.ToolName, "err", err)
	}

	return result
}

// DefaultMaxArgBytes 返回单次调用参数的上限。
//
// 参数是模型在一次响应里生成的，所以它和 LLM_MAX_TOKENS 是同一个约束的两半：
// 16KB ≈ 4096 output tokens（英文 4 字节/token，中文 0.6 token/字 ≈ 5 字节/字）。
// 写死一个常数会跟模型配置脱节——运维把 LLM_MAX_TOKENS 调到 16384，工具却还卡在 16KB；
// 反过来调低时又拦不住半截 JSON。所以这里按单次输出预算折算，上限跟着配置走。
func DefaultMaxArgBytes(maxTokens int) int {
	// 1 token ≈ 4 字节是保守值：中文按 UTF-8 三字节/字折算下来每 token 约 5 字节，
	// 取小值只会让上限更紧一点，不会放过模型根本生成不出来的参数。
	if b := maxTokens * 4; b > 16<<10 {
		return b
	}
	return 16 << 10
}

// argLimit 返回本次调用的参数上限。
func (i *Invoker) argLimit() int {
	if i.maxArgBytes > 0 {
		return i.maxArgBytes
	}
	return DefaultMaxArgBytes(0)
}

// argTooLarge 是参数超限时回给模型的说明。
//
// 只说「参数过大」的话，模型往往原地重发一遍同样的内容，再撞一次同一个上限——
// 它不是不知道超了，是不知道该切成几块、切完之后用哪种 mode 续上。
func argTooLarge(toolName string, got, limit int) string {
	return fmt.Sprintf(
		"工具参数过大（%s 单次上限 %s，本次 %s），这次调用没有执行。"+
			"请缩小这一次的调用规模后重试：如果内容是文件正文，请拆成多块写入——"+
			"第一块用 mode=overwrite（新文件用 create），其余各块用 mode=append 依次追加到同一路径；"+
			"其它工具请减少条目、缩短文本或分批处理。",
		toolName, kib(limit), kib(got))
}

func kib(n int) string {
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}

// clampMaxBytes 是工具结果回填的硬上限，避免超长结果撑爆上下文。
// 读类工具通过 Tool.MaxResultBytes 申请更大额度，那是在提示词侧生效的另一层。
const clampMaxBytes = 32 * 1024

// clamp 按上限截断输出。
func clamp(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = clampMaxBytes
	}
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "") + "…（结果已截断）"
}
