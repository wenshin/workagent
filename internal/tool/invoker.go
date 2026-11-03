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
}

// Invoker 是 ToolInvoker（§5.7.7），把一次工具调用收敛成固定流程：
//
//	参数校验 → PDP 授权 → 审批检查 → 执行（超时保护）→ 脱敏 → 截断 → 审计
type Invoker struct {
	engine  policy.Engine
	store   store.Store
	log     *slog.Logger
	timeout time.Duration
}

// NewInvoker 构造执行器；engine 为空时使用默认 RBAC 策略。
func NewInvoker(engine policy.Engine, st store.Store, log *slog.Logger) *Invoker {
	if engine == nil {
		engine = policy.RBACEngine{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Invoker{engine: engine, store: st, log: log, timeout: 30 * time.Second}
}

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
	if len(args) > 16*1024 {
		return i.finish(ctx, env, result, start, StatusError, "工具参数过大（上限 16KB）")
	}

	decision, err := i.engine.Check(ctx, policy.Request{
		Actor:       env.UserID,
		Action:      "tool:invoke",
		Tool:        t.Name,
		Risk:        t.Risk,
		SpaceID:     env.SpaceID,
		SpacePerm:   env.SpacePerm,
		SideEffects: t.SideEffects,
		HasBind:     t.HasBinding,
		Binding:     t.Constraints,
	})
	if err != nil {
		var denied *policy.ErrDenied
		if errors.As(err, &denied) {
			return i.finish(ctx, env, result, start, StatusDenied, denied.Error())
		}
		return i.finish(ctx, env, result, start, StatusError, err.Error())
	}
	if decision.NeedApproval {
		reason := decision.Reason
		if reason == "" {
			reason = "该工具需要人工审批"
		}
		return i.finish(ctx, env, result, start, StatusApproval,
			fmt.Sprintf("调用 %s 需要审批，已生成审批请求，等待通过后可重试。原因：%s", t.Name, reason))
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
	out = clamp(out, decision.Constraints.MaxBytes)
	return i.finish(ctx, env, result, start, StatusOK, out)
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

	if err := i.store.RecordToolCall(recordCtx, store.ToolCallRecord{
		SessionID: env.SessionID,
		RunID:     env.RunID,
		ToolName:  result.ToolName,
		Args:      []byte(`{}`),
		Status:    status,
		LatencyMS: int(result.LatencyMS),
		RiskLevel: result.Risk,
	}); err != nil {
		i.log.Warn("记录工具调用失败", "tool", result.ToolName, "err", err)
	}

	detail, _ := json.Marshal(map[string]any{
		"tool":       result.ToolName,
		"status":     status,
		"latency_ms": result.LatencyMS,
		"risk":       result.Risk,
	})
	if err := i.store.AppendAuditLog(recordCtx, store.AuditEntry{
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

// clamp 按绑定约束截断输出，避免超长结果撑爆上下文。
func clamp(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 32 * 1024
	}
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "") + "…（结果已截断）"
}
