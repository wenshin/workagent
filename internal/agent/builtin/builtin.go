// Package builtin 是内置的 ReAct 编排实现（技术方案 §5.7.2 主循环）：
// 「模型 → 工具 → 模型」循环，同一轮的工具调用并行执行，预算来自平台配置。
//
// 平台侧能力不在这里实现（§5.7.7 能力边界）：
//   - 审批判定在 tool.Invoker 的策略链里，本实现只把结果翻译成 approval.req 事件；
//   - checkpoint 由会话层按事件落 run_step；
//   - 上下文压缩由 ctxmgr 在调用前完成。
package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// Name 是实现名，对应 AGENT_IMPL=builtin，也是默认实现。
const Name = "builtin"

func init() { agent.Register(Name, New) }

// 单轮对话内的容量上限，避免模型输出把上下文撑爆。
const (
	maxToolArgBytes = 8 * 1024
	// maxToolFeedBytes 是工具结果回填给模型的默认上限；
	// 读类工具通过 Tool.MaxResultBytes 申请更高额度（见 file_read / resource_read）。
	maxToolFeedBytes = 4 * 1024
)

// Agent 是内置 ReAct 实现，满足 agent.Agent。
type Agent struct {
	cfg      *config.Config
	llm      *llm.Router
	registry *tool.Registry
	invoker  *tool.Invoker
	log      *slog.Logger
}

// New 构造内置实现。
func New(d agent.Deps) (agent.Agent, error) {
	if d.Config == nil {
		return nil, fmt.Errorf("缺少配置")
	}
	if d.LLM == nil {
		return nil, fmt.Errorf("缺少模型路由")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Agent{cfg: d.Config, llm: d.LLM, registry: d.Registry, invoker: d.Invoker, log: d.Log}, nil
}

// Name 返回实现名。
func (a *Agent) Name() string { return Name }

// Capabilities 声明内置实现具备的能力。
func (a *Agent) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Tools:         true,
		ParallelTools: true,
		Streaming:     true,
		Reasoning:     true,
		Approval:      true,
	}
}

// Ready 判断模型路由是否可用。
func (a *Agent) Ready() bool { return a.llm != nil && a.llm.Ready() }

// Model 返回当前路由命中的模型名。
func (a *Agent) Model() string { return a.llm.Model(llm.RouteChat) }

// Budget 返回本次执行的预算上限，写入 run.budget（§5.7.2 Token 预算、§6 run）。
func (a *Agent) Budget() map[string]any {
	return map[string]any{
		"max_rounds":       a.cfg.LLMMaxRounds,
		"max_tool_calls":   a.cfg.LLMMaxToolCalls,
		"max_wall_clock_s": int(a.cfg.LLMMaxWallClock.Seconds()),
		"max_tokens":       a.cfg.LLMMaxTokens,
	}
}

// Run 执行一次带工具调用的对话，过程中通过 emit 推送增量事件。
func (a *Agent) Run(ctx context.Context, in agent.RunInput, emit func(agent.Event) error) (agent.Result, error) {
	var res agent.Result
	if !a.Ready() {
		return res, fmt.Errorf("模型服务未配置：请设置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
	}
	if emit == nil {
		emit = func(agent.Event) error { return nil }
	}

	tools := in.Tools
	if len(tools) == 0 && a.registry != nil {
		loaded, err := a.registry.Load(ctx, in.Env, in.Query)
		if err != nil {
			return res, fmt.Errorf("加载工具失败：%w", err)
		}
		tools = loaded
	}
	defs := tool.Definitions(tools)

	messages := make([]llm.Message, 0, len(in.History)+2)
	messages = append(messages, agent.BuildSystemMessage(in))
	messages = append(messages, in.History...)

	var reasoning strings.Builder

	deadline := time.Now().Add(a.cfg.LLMMaxWallClock)
	toolCalls := 0

	for round := 0; round < a.cfg.LLMMaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("%w：本轮对话超过时间预算（%s）", agent.ErrBudgetExceeded, a.cfg.LLMMaxWallClock)
		}

		out, provider, chatErr := a.llm.Chat(ctx, llm.RouteChat, llm.ChatRequest{Messages: messages, Tools: defs}, func(d llm.Delta) error {
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
			}
			if d.Content != "" {
				res.Content += d.Content
				return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Content})
			}
			return nil
		})
		if chatErr != nil {
			return res, chatErr
		}
		res.Provider = provider
		res.Model = out.Model
		res.Usage.InputTokens += out.Usage.InputTokens
		res.Usage.OutputTokens += out.Usage.OutputTokens

		if len(out.ToolCalls) == 0 {
			res.Reasoning = reasoning.String()
			return res, nil
		}

		if toolCalls+len(out.ToolCalls) > a.cfg.LLMMaxToolCalls {
			return res, fmt.Errorf("%w：工具调用次数超过上限（%d）", agent.ErrBudgetExceeded, a.cfg.LLMMaxToolCalls)
		}
		toolCalls += len(out.ToolCalls)

		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   out.Content,
			ToolCalls: out.ToolCalls,
		})

		for _, call := range out.ToolCalls {
			state := &agent.ToolState{
				ID:     call.ID,
				Name:   call.Function.Name,
				Args:   agent.TruncateText(call.Function.Arguments, maxToolArgBytes),
				Status: "running",
			}
			if t, ok := tool.Find(tools, call.Function.Name); ok {
				state.Risk = t.Risk
			}
			if err := emit(agent.Event{Type: agent.EventToolCall, Tool: state}); err != nil {
				return res, err
			}
		}

		// 同一轮的多个调用并行执行，但事件与消息拼接保持确定顺序
		results := make([]tool.InvokeResult, len(out.ToolCalls))
		sem := make(chan struct{}, maxParallel(a.cfg.LLMMaxParallel))
		var wg sync.WaitGroup
		for i, call := range out.ToolCalls {
			wg.Add(1)
			go func(i int, call llm.ToolCall) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				t, ok := tool.Find(tools, call.Function.Name)
				if !ok {
					results[i] = tool.InvokeResult{ToolName: call.Function.Name, Status: tool.StatusError,
						Content: fmt.Sprintf("未知工具：%s", call.Function.Name)}
					return
				}
				if a.invoker == nil {
					results[i] = tool.InvokeResult{ToolName: call.Function.Name, Status: tool.StatusError,
						Content: "工具执行器未配置，无法执行工具"}
					return
				}
				results[i] = a.invoker.Invoke(ctx, t, call, in.Env)
			}(i, call)
		}
		wg.Wait()

		for i, call := range out.ToolCalls {
			r := results[i]
			state := &agent.ToolState{
				ID:         call.ID,
				Name:       call.Function.Name,
				Args:       agent.TruncateText(call.Function.Arguments, maxToolArgBytes),
				Result:     agent.TruncateText(r.Content, 8*1024),
				Status:     r.Status,
				Risk:       r.Risk,
				DurationMS: r.LatencyMS,
			}
			if err := emit(agent.Event{Type: agent.EventToolResult, Tool: state}); err != nil {
				return res, err
			}
			// 策略判定需要审批时额外推一条审批请求（§5.7.5），前端据此挂起并提示
			if state.Status == tool.StatusApproval {
				res.NeedApproval = true
				if err := emit(agent.Event{Type: agent.EventApprovalReq, Approval: &agent.ApprovalState{
					ID:       agent.ApprovalID(call.ID),
					CallID:   call.ID,
					Tool:     state.Name,
					Reason:   r.Content,
					TimeoutS: agent.ApprovalTimeoutS,
				}}); err != nil {
					return res, err
				}
			}

			res.Steps = append(res.Steps, agent.Step{
				Type:       "tool",
				ID:         state.ID,
				Name:       state.Name,
				Args:       state.Args,
				Result:     state.Result,
				Status:     state.Status,
				Risk:       state.Risk,
				DurationMS: state.DurationMS,
			})

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Content:    agent.TruncateText(r.Content, resultBudget(tools, call.Function.Name)),
			})
		}
	}

	res.Reasoning = reasoning.String()
	return res, nil
}

// resultBudget 返回单次工具结果回填给模型的字节上限。
func resultBudget(tools []tool.Tool, name string) int {
	if t, ok := tool.Find(tools, name); ok && t.MaxResultBytes > 0 {
		return t.MaxResultBytes
	}
	return maxToolFeedBytes
}

func maxParallel(configured int) int {
	if configured <= 0 {
		return 4
	}
	return configured
}
