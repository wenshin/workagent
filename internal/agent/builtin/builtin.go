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
	"encoding/json"
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
func (a *Agent) Run(ctx context.Context, in agent.RunInput, emit func(agent.Event) error) (res agent.Result, err error) {
	// 模式只升不降：这一轮只要出现过编排动作就算编排档，否则事后统计会把
	// 「派了子 agent、又在本轮收尾」算成问答档。defer 统一回填，各条 return 不用各写一遍。
	mode := agent.ModeAnswer
	defer func() {
		if res.Mode == "" {
			res.Mode = mode
		}
	}()
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
	// 任务清单只在顶层会话暴露：子 Agent 的边界由派发方给定，再建清单只会放大跑偏。
	// 状态从上一个 checkpoint 恢复，续跑时接着走已完成的步骤。
	planSeed := in.Plan
	if in.Resume != nil && in.Resume.Plan != nil {
		planSeed = in.Resume.Plan
	}
	plan := newPlanState(planSeed)
	if in.Depth == 0 {
		tools = append(tools, a.planTools(plan)...)
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

	// checkpoint 记的是「已经走到哪」：消息序列 + 预算计数 + 还没拿到回执的调用。
	//
	// pending 非空表示这一批工具执行到一半：恢复器要先补齐它们再问模型，
	// 否则模型对着「3 个 tool_call 只有 1 条回执」的消息会重新规划。
	checkpoint := func(round int, pending []agent.PendingCall) {
		if in.Checkpoint == nil {
			return
		}
		state := agent.ResumeState{
			Messages:    messages,
			Round:       round + 1,
			ToolCalls:   toolCalls,
			TotalTokens: totalTokens,
			Query:       in.Query,
			Pending:     pending,
			Plan:        plan.snapshot(),
		}
		_ = in.Checkpoint(state)
	}

	// 续跑先补完中断时的半批调用，再进主循环。
	//
	// 已完成的调用会命中 Invoker 的回放缓存（同工具同参数），不会再产生一次副作用；
	// 没完成的这次才真正执行——包括重新走审批，但已批准的结论会按参数复用。
	if in.Resume != nil && len(in.Resume.Pending) > 0 {
		for _, p := range in.Resume.Pending {
			call := llm.ToolCall{
				ID:       p.ID,
				Type:     "function",
				Function: llm.FunctionCall{Name: p.Name, Arguments: p.Args},
			}
			t, ok := tool.Find(tools, p.Name)
			var r tool.InvokeResult
			switch {
			case !ok:
				r = tool.InvokeResult{ToolName: p.Name, Status: tool.StatusError,
					Content: fmt.Sprintf("未知工具：%s", p.Name)}
			case a.invoker == nil:
				r = tool.InvokeResult{ToolName: p.Name, Status: tool.StatusError,
					Content: "工具执行器未配置，无法执行工具"}
			default:
				r = a.invoker.Invoke(ctx, t, call, in.Env)
				toolCalls++
			}
			state := &agent.ToolState{
				ID:         p.ID,
				Name:       p.Name,
				Args:       agent.TruncateText(p.Args, maxToolArgBytes),
				Result:     agent.TruncateText(r.Content, 8*1024),
				Status:     r.Status,
				Risk:       r.Risk,
				DurationMS: r.LatencyMS,
			}
			if err := emit(agent.Event{Type: agent.EventToolResult, Tool: state}); err != nil {
				return res, err
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
			res.Timeline = append(res.Timeline, agent.Segment{Type: "tool", ToolID: state.ID})
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: p.ID,
				Content:    agent.TruncateText(r.Content, resultBudget(tools, p.Name)),
			})
		}
		// 补齐后这一轮才算真正完成
		checkpoint(roundStart-1, nil)
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
				res.AppendSegment("reasoning", d.Reasoning)
				return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
			}
			if d.Content != "" {
				roundText.WriteString(d.Content)
				res.AppendSegment("text", d.Content)
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

		// 「已经决定要调这些」先落一次：中断落在这批中途时，恢复器才知道要补哪几个。
		// 这一条不做的话，快照停在上一轮结束，模型会重新规划、把副作用再做一遍。
		unfinished := make([]agent.PendingCall, len(out.ToolCalls))
		for i, call := range out.ToolCalls {
			unfinished[i] = agent.PendingCall{
				ID:   call.ID,
				Name: call.Function.Name,
				Args: call.Function.Arguments,
			}
		}
		checkpoint(round, unfinished)

		for _, call := range out.ToolCalls {
			state := &agent.ToolState{
				ID:     call.ID,
				Name:   call.Function.Name,
				Args:   agent.TruncateText(call.Function.Arguments, maxToolArgBytes),
				Status: "running",
			}
			if t, ok := tool.Find(tools, call.Function.Name); ok {
				// running 态与最终回执用同一套计算，否则徽标会在中途跳变
				state.Risk = t.EffectiveRisk(json.RawMessage(call.Function.Arguments))
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

		// 被单次输出上限截断的调用不能执行：参数只是半截 JSON，
		// 拿去执行要么报「参数不是合法 JSON」，要么把半个文件写下去。
		// 回填的说明必须写清「怎么切」，否则模型会原样重发一遍再撞同一个上限。
		if out.FinishReason == llm.FinishLength {
			for _, call := range out.ToolCalls {
				if json.Valid([]byte(call.Function.Arguments)) {
					continue
				}
				rejected[call.ID] = truncatedCallHint(a.cfg.LLMMaxTokens)
			}
		}

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
					Risk:     riskOf(tools, call.Function.Name, json.RawMessage(call.Function.Arguments)),
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
			// 审批事件由 Invoker 在调用中途经 Approver 推（含命令原文与动作类别），
			// 这里不再重复推送：命中审批时 Invoker 会先阻塞，拿到决策后才产出这条结果。

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
			res.Timeline = append(res.Timeline, agent.Segment{Type: "tool", ToolID: state.ID})

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Content:    agent.TruncateText(r.Content, resultBudget(tools, call.Function.Name)),
			})
			lastToolMsg = len(messages) - 1

			// 每拿到一条回执就前进一步：中断时已经做完的调用不必重来
			unfinished = dropPending(unfinished, call.ID)
			checkpoint(round, unfinished)
			records = append(records, callRecord{
				Name:   call.Function.Name,
				Args:   call.Function.Arguments,
				Status: r.Status,
				Output: r.Content,
			})
		}

		// 按这一轮实际调用的工具抬升档位：写操作算任务档，清单与派发算编排档。
		// 只记录事实，不据此增删工具——收窄工具面会让模型连「想升档」都表达不出来。
		for _, call := range out.ToolCalls {
			mode = raiseMode(mode, modeOfTool(call.Function.Name))
		}

		// 本轮没有新信息时提醒模型换方法，而不是等它自己发现
		if hint := guard.afterRound(records); hint != "" && lastToolMsg >= 0 {
			messages[lastToolMsg].Content = hint + "\n\n" + messages[lastToolMsg].Content
		}
		checkpoint(round, nil)
	}

	// 轮次用尽：不再静默返回半截内容，而是逼一次结论
	return a.finish(ctx, &res, messages, emit, "budget",
		fmt.Sprintf("已用完 %d 轮工具调用", maxRounds), true)
}

// modeOfTool 把一次工具调用映射到它代表的档位。
//
// 只看会造成外部状态变化、或提升编排复杂度的工具：读取类工具三个档都可能出现，
// 拿它判档会把「问答时查了个文件」误记成任务档。
func modeOfTool(name string) string {
	switch name {
	case planWriteToolName, dispatchToolName:
		return agent.ModeOrchestrate
	case "file_write", "wiki_write", "shell_exec":
		return agent.ModeExec
	}
	return ""
}

// raiseMode 取两个档位里更高的那个：模式在一轮内只升不降。
func raiseMode(cur, next string) string {
	if modeRank(next) > modeRank(cur) {
		return next
	}
	return cur
}

func modeRank(mode string) int {
	switch mode {
	case agent.ModeOrchestrate:
		return 3
	case agent.ModeExec:
		return 2
	case agent.ModeAnswer:
		return 1
	}
	return 0
}

// dropPending 把一条已经拿到回执的调用从待办里移掉。
func dropPending(list []agent.PendingCall, id string) []agent.PendingCall {
	for i, p := range list {
		if p.ID == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
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
			res.AppendSegment("reasoning", d.Reasoning)
			return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
		}
		if d.Content != "" {
			text.WriteString(d.Content)
			res.AppendSegment("text", d.Content)
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

// riskOf 返回这一次调用的展示等级（未知工具为 0）。
func riskOf(tools []tool.Tool, name string, args json.RawMessage) int {
	if t, ok := tool.Find(tools, name); ok {
		return t.EffectiveRisk(args)
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

// truncatedCallHint 说明「这次调用为什么作废、下次怎么发」。
//
// 建议的单块大小按单次输出预算折算（留 25% 余量给路径、mode 与转义），
// 与 Invoker 的参数上限同源：模型照着这个数切，就不会再撞同一个上限。
func truncatedCallHint(maxTokens int) string {
	chunk := 8 // 未知配置时按 8KB 给：默认 4096 输出预算下的保守值
	if maxTokens > 0 {
		if kb := maxTokens * 3 / 1024; kb > 0 {
			chunk = kb
		}
	}
	budget := "单次响应上限"
	if maxTokens > 0 {
		budget = fmt.Sprintf("单次响应上限 LLM_MAX_TOKENS=%d", maxTokens)
	}
	return fmt.Sprintf(
		"本次调用的参数被%s截断（模型这次没说完），没有执行。"+
			"请把内容拆小后重试：写文件时每块不超过 %dKB，第一块用 mode=overwrite（新文件用 create），"+
			"其余各块用 mode=append 追加到同一路径；其它工具请缩窄参数范围（减少条目、缩短正文、分批处理）。",
		budget, chunk)
}

func maxParallel(configured int) int {
	if configured <= 0 {
		return 4
	}
	return configured
}
