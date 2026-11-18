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

	if decision.NeedApproval {
		approved, reason := i.askApproval(ctx, t, call, env, decision)
		if !approved {
			return i.finish(ctx, env, result, start, StatusDenied, reason)
		}
	}

	if t.Invoke == nil {
		return i.finish(ctx, env, result, start, StatusError, "工具未实现")
	}

	runCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	out, runErr := t.Invoke(runCtx, args, env)
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return i.finish(ctx, env, result, start, StatusError, fmt.Sprintf("工具执行超时（%s）", i.timeout))
		}
		if errors.Is(runErr, context.Canceled) {
			return i.finish(ctx, env, result, start, StatusError, "调用已取消")
		}
		i.log.Warn("工具执行失败", "tool", t.Name, "err", runErr)
		return i.finish(ctx, env, result, start, StatusError, "工具执行失败："+runErr.Error())
	}

	out = Mask(out)
	// 这里是回填前的硬上限；读类工具更大的额度由 Tool.MaxResultBytes 在
	// 提示词侧另行控制（见 agent.resultBudget）。
	out = clamp(out, clampMaxBytes)
	return i.finish(ctx, env, result, start, StatusOK, out)
}

// ErrApprovalTimeout 表示等待人工确认超时（按拒绝处理）。
var ErrApprovalTimeout = errors.New("等待审批超时")

// askApproval 阻塞等待用户决策。返回 false 时 reason 是要回给模型的说明。
//
// 没有审批通道时按拒绝处理：命中规则却没人能批，静默执行才是真正危险的默认值。
func (i *Invoker) askApproval(
	ctx context.Context,
	t Tool,
	call llm.ToolCall,
	env Env,
	decision policy.Decision,
) (bool, string) {
	if decision.Action == nil {
		return false, fmt.Sprintf("调用 %s 需要人工确认，但没有说明确认什么，已拒绝执行", t.Name)
	}
	if env.Approvals == nil {
		return false, fmt.Sprintf("调用 %s 需要人工确认（%s），但审批通道未配置，已拒绝执行", t.Name, decision.Reason)
	}
	d, err := env.Approvals.Decide(ctx, ApprovalRequest{
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
		Risk:    t.EffectiveRisk(json.RawMessage(call.Function.Arguments)),
		Timeout: DefaultApprovalTimeout,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrApprovalTimeout), errors.Is(err, context.DeadlineExceeded):
		return false, fmt.Sprintf("等待确认超时（%s），这次调用已取消。可以稍后重试。", DefaultApprovalTimeout)
	case errors.Is(err, context.Canceled):
		return false, "等待确认时运行被取消，这次调用没有执行"
	default:
		return false, "等待确认失败：" + err.Error()
	}
	if d.Allow {
		return true, ""
	}
	reason := d.Reason
	if reason == "" {
		reason = "用户拒绝了这次调用"
	}
	return false, reason + "（未执行）"
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
