// Package builtin 是内置的 ReAct 编排实现（技术方案 §5.7.2 主循环）：
// 「模型 → 工具 → 模型」循环，同一轮的工具调用并行执行，预算来自平台配置。
//
// 长任务的成败主要不在「轮次够不够」，而在循环能不能识别自己白干：
// 因此循环里常驻一个进度守卫（见 guard.go），每一轮结束都判断「下一步还有没有价值」，
// 判定停滞时立刻收尾，而不是烧完预算再吐一条空回答。
//
// 平台侧能力不在这里实现（§5.7.7 能力边界）：
//   - 审批判定在 tool.Invoker 的策略链里，本实现只把结果翻译成 approval.req 事件；
//   - checkpoint 由会话层按事件落 run_step；
//   - 跨轮的上下文压缩由 ctxmgr 在调用前完成；轮内的工具往返折叠在这里（compact.go），
//     因为只有本实现知道「哪几轮是一个完整的工具往返」。
package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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
	// 读类工具通过 Tool.MaxResultBytes 申请更高额度（见 file_read / plugin_read）。
	maxToolFeedBytes = 4 * 1024
	// defaultRounds 是配置缺失时的轮次兜底：一轮 = 一次模型调用。
	//
	// 它只是「防死循环」的最后一道，不是长任务的判据：真正决定收尾的是
	// 进度守卫（连续无新信息）与 token 预算。按 10 轮给，长任务跑不满 10 轮就被砍
	// 才是不该发生的事。
	defaultRounds = 40
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
		Resume:        true,
		Dispatch:      true,
		Guard:         true,
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
		"max_total_tokens": a.cfg.LLMMaxTotalTokens,
		"context_tokens":   a.cfg.LLMContextTokens,
		"stall_rounds":     a.cfg.AgentStallRounds,
		"max_depth":        a.maxDepth(),
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
	// 派发能力只在允许且未到深度上限时暴露：子 Agent 也能派发，但层数有上限
	if a.canDispatch(in) {
		tools = append(tools, a.dispatchTool(in, tools))
	}
	defs := tool.Definitions(tools)

	// 续跑：直接沿用断点里的消息序列与预算计数。
	//
	// 系统提示词、历史、每一轮的工具回执都在里面，所以「接着走」不是重新规划——
	// 已经执行过的工具不会因为续跑再执行一遍（副作用类工具尤其重要）。
	var messages []llm.Message
	roundStart := 0
	toolCalls := 0
	totalTokens := 0
	if in.Resume != nil && len(in.Resume.Messages) > 0 {
		messages = append(messages, in.Resume.Messages...)
		roundStart = in.Resume.Round
		toolCalls = in.Resume.ToolCalls
		totalTokens = in.Resume.TotalTokens
	} else {
		messages = make([]llm.Message, 0, len(in.History)+2)
		messages = append(messages, agent.BuildSystemMessage(in))
		messages = append(messages, in.History...)
	}

	var reasoning strings.Builder
	maxRounds := a.roundLimit(in)
	guard := newProgressGuard(a.cfg.AgentStallRounds)
	// lastInputTokens 是上一轮模型回报的输入规模：它比字数估算准，
	// 也是「这一轮上下文有多长」的唯一真实观测值
	lastInputTokens := 0

	// 续跑也可能把轮次用光：这时不必再问模型，直接按预算用尽收尾，
	// 否则会走到「没有返回内容」那条更含糊的错误路径。
	if roundStart >= maxRounds {
		res.StopReason = "budget"
		return res, fmt.Errorf("%w：续跑时轮次预算已用尽（%d 轮）", agent.ErrBudgetExceeded, maxRounds)
	}

	// checkpoint 只记「已经完成的轮次」：一轮里的模型增量没有重放价值，
	// 工具回执才是真的进展，也是重入时必须原样带回模型的东西。
	checkpoint := func(round int) {
		if in.Checkpoint == nil {
			return
		}
		state := agent.ResumeState{
			Messages:    messages,
			Round:       round + 1,
			ToolCalls:   toolCalls,
			TotalTokens: totalTokens,
			Query:       in.Query,
		}
		_ = in.Checkpoint(state)
	}

	for round := roundStart; round < maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		// 每一轮先问「下一步还有没有价值」，再决定给不给工具
		decision := guard.beforeRound(round, maxRounds)
		roundDefs := defs
		if decision.ForceFinal {
			roundDefs = nil
			messages = append(messages, finalInstruction(decision.Detail))
		}

		// 上下文管理：超预算就折叠较早的工具往返，别等模型报错。
		// 判据用上一轮真实的 input_tokens，没有就按字数估。
		messages = a.fitContext(messages, lastInputTokens)

		var roundText strings.Builder
		// 注意用 = 而不是 :=：:= 会在循环里新建一个 messages，
		// 外层那份就再也不长了（上下文每轮都被重置成系统提示词）
		var out llm.Result
		var provider string
		var chatErr error
		out, provider, messages, chatErr = a.chatRound(ctx, messages, roundDefs, func(d llm.Delta) error {
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
			}
			if d.Content != "" {
				roundText.WriteString(d.Content)
				return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Content})
			}
			return nil
		})
		if chatErr != nil {
			return res, chatErr
		}
		// 这一轮模型看到的输入规模，就是下一轮压缩的判据
		if out.Usage.InputTokens > 0 {
			lastInputTokens = out.Usage.InputTokens
		}
		appendRoundContent(&res, roundText.String())
		res.Provider = provider
		res.Model = out.Model
		res.Usage.InputTokens += out.Usage.InputTokens
		res.Usage.OutputTokens += out.Usage.OutputTokens
		totalTokens += out.Usage.InputTokens + out.Usage.OutputTokens

		if len(out.ToolCalls) == 0 {
			res.Reasoning = reasoning.String()
			res.StopReason = "done"
			if decision.ForceFinal {
				// 结论是「被逼出来的」，如实记录：这次运行没有正常收敛，
				// 但用户拿到了可用结果，因此不算失败。
				res.StopReason = decision.Reason
			}
			return res, nil
		}
		// 收尾轮已经不给工具了，模型仍在调工具：再要一次结论（仍不带工具），
		// 而不是把「空内容 + 一次没执行的工具调用」当成正常结束。
		if roundDefs == nil {
			res.Reasoning = reasoning.String()
			return a.finish(ctx, &res, messages, emit, decision.Reason, decision.Detail, false)
		}

		if limit := a.cfg.LLMMaxTotalTokens; limit > 0 && totalTokens > limit {
			return a.finish(ctx, &res, messages, emit, "budget",
				fmt.Sprintf("累计 token 超过上限（%d/%d）", totalTokens, limit), true)
		}
		if toolCalls+len(out.ToolCalls) > a.cfg.LLMMaxToolCalls {
			return a.finish(ctx, &res, messages, emit, "budget",
				fmt.Sprintf("工具调用次数超过上限（%d）", a.cfg.LLMMaxToolCalls), true)
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

		// 重复调用在进入执行前拦截：同样的工具 + 同样的参数不会产生新结果，
		// 让它执行只会烧预算，还会让模型误以为「又试了一次」。
		pending := make([]pendingCall, len(out.ToolCalls))
		for i, call := range out.ToolCalls {
			pending[i] = pendingCall{ID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments}
		}
		rejected := guard.inspect(pending)

		// 同一轮的多个调用并行执行，但事件与消息拼接保持确定顺序
		results := make([]tool.InvokeResult, len(out.ToolCalls))
		sem := make(chan struct{}, maxParallel(a.cfg.LLMMaxParallel))
		var wg sync.WaitGroup
		for i, call := range out.ToolCalls {
			if reason, blocked := rejected[call.ID]; blocked {
				results[i] = tool.InvokeResult{
					ToolName: call.Function.Name,
					Status:   tool.StatusError,
					Content:  reason,
					Risk:     riskOf(tools, call.Function.Name),
				}
				continue
			}
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

		records := make([]callRecord, 0, len(out.ToolCalls))
		lastToolMsg := -1
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
			lastToolMsg = len(messages) - 1
			records = append(records, callRecord{
				Name:   call.Function.Name,
				Args:   call.Function.Arguments,
				Status: r.Status,
				Output: r.Content,
			})
		}

		// 本轮没有新信息时提醒模型换方法，而不是等它自己发现
		if hint := guard.afterRound(records); hint != "" && lastToolMsg >= 0 {
			messages[lastToolMsg].Content = hint + "\n\n" + messages[lastToolMsg].Content
		}
		checkpoint(round)
	}

	// 轮次用尽：不再静默返回半截内容，而是逼一次结论
	return a.finish(ctx, &res, messages, emit, "budget",
		fmt.Sprintf("已用完 %d 轮工具调用", maxRounds), true)
}

// finish 在预算用尽或判定停滞时再做一次「不带工具」的调用，逼模型给出结论。
//
// 没有这一步，用户拿到的是一条空回答——这是长任务最常见的坏体验，
// 也是最难归因的故障之一（前端只看到一条空消息）。
//
// hard 决定「拿到结论后要不要向上报错」：
//   - false：结论可用，这次运行算成功，只把 stop_reason 记录在案；
//   - true：预算确实用尽了，返回 ErrBudgetExceeded / ErrStalled，
//     会话层据此标 cancelled 并把原因写进 run.error（内容保留）。
func (a *Agent) finish(
	ctx context.Context,
	res *agent.Result,
	messages []llm.Message,
	emit func(agent.Event) error,
	reason, detail string,
	hard bool,
) (agent.Result, error) {
	res.StopReason = reason
	msgs := make([]llm.Message, 0, len(messages)+1)
	msgs = append(msgs, messages...)
	msgs = append(msgs, finalInstruction(detail))
	// 收尾调用最容易撞窗口：它要把整段历史再发一次，而这时上下文通常已经最长
	msgs = a.fitContext(msgs, 0)

	var text strings.Builder
	out, provider, _, err := a.chatRound(ctx, msgs, nil, func(d llm.Delta) error {
		if d.Reasoning != "" {
			return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
		}
		if d.Content != "" {
			text.WriteString(d.Content)
			return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Content})
		}
		return nil
	})
	if err == nil {
		appendRoundContent(res, text.String())
		res.Provider = provider
		res.Model = out.Model
		res.Usage.InputTokens += out.Usage.InputTokens
		res.Usage.OutputTokens += out.Usage.OutputTokens
	}

	if strings.TrimSpace(res.Content) == "" {
		if err != nil {
			return *res, fmt.Errorf("%w：%s（收尾调用失败：%v）", stopErr(reason), detail, err)
		}
		return *res, fmt.Errorf("%w：%s", stopErr(reason), detail)
	}
	if hard {
		return *res, fmt.Errorf("%w：%s", stopErr(reason), detail)
	}
	return *res, nil
}

// stopErr 把收尾原因映射成上层可判别的错误。
func stopErr(reason string) error {
	if reason == "stalled" {
		return agent.ErrStalled
	}
	return agent.ErrBudgetExceeded
}

// finalInstruction 是收尾轮的固定指令。
func finalInstruction(detail string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: "系统提醒：本次运行需要立即收尾（" + detail + "）。\n" +
			"请停止调用工具，直接用已经获得的信息给出最终回答；" +
			"信息不足的部分明确说明缺什么，不要编造，也不要再描述你打算怎么做。",
	}
}

// appendRoundContent 拼接第 N 轮的正文本。
// 多轮之间加分隔，否则两段结论会粘成一句读不通的话。
func appendRoundContent(res *agent.Result, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if strings.TrimSpace(res.Content) != "" && !strings.HasSuffix(res.Content, "\n") {
		res.Content += "\n\n"
	}
	res.Content += text
}

// roundLimit 取「配置上限」与「调用方传入的子任务上限」中更小的一个。
func (a *Agent) roundLimit(in agent.RunInput) int {
	limit := a.cfg.LLMMaxRounds
	if limit <= 0 {
		limit = defaultRounds
	}
	if in.RoundLimit > 0 && in.RoundLimit < limit {
		limit = in.RoundLimit
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// riskOf 返回工具的风险等级（未知工具为 0）。
func riskOf(tools []tool.Tool, name string) int {
	if t, ok := tool.Find(tools, name); ok {
		return t.Risk
	}
	return 0
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
