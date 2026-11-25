package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/tool"
)

// dispatchToolName 是派生子 Agent 的工具名。
//
// 它是「工具」而不是「编排图」：主 Agent 决定什么时候把一件事整体交出去，
// 子 Agent 用同一套工具链、同一套策略与审计，只是上下文与预算被隔离。
// 这样做的收益是可测的：子任务的中间输出不占主上下文，主 Agent 只收结论。
const dispatchToolName = "agent_dispatch"

// 子 Agent 的默认与最大轮次。子任务应当比主任务更窄，给它 20 轮只会放大跑偏的代价。
const (
	defaultSubAgentRounds = 3
	maxSubAgentTaskRunes  = 4000
	// 子 Agent 结论回填给主 Agent 的上限：结论应当自包含，而不是把整段过程搬回来
	maxSubAgentResultBytes = 4 * 1024
)

// subAgentPrompt 追加给子 Agent 的行为约束，与平台基础规则叠加（只能追加）。
const subAgentPrompt = `你是被主 Agent 派发的子 Agent，只负责完成指派给你的一个子任务。

要求：
- 直接做，不要复述背景；主 Agent 看不到你的中间过程，结论必须自包含。
- 不要扩大范围，也不要猜测指派之外的目标；缺少必要信息时说明缺什么，而不是编造。
- 结束时说明：做了什么、产物在哪里（文件路径 / 数据 / 结论）、还有什么没做完。`

// canDispatch 判断本轮是否应该把 agent_dispatch 暴露给模型。
func (a *Agent) canDispatch(in agent.RunInput) bool {
	if !a.cfg.AgentDispatch || !in.AllowDispatch {
		return false
	}
	return in.Depth < a.maxDepth()
}

// dispatchPressureHint 是「主上下文已经接近窗口」时注入的拆解提醒的固定前缀。
// 它同时是去重依据：同一次运行（含续跑）只提醒一次，避免每轮重复注入。
const dispatchPressureHint = "[上下文提醒]"

// needDispatchHint 判断是否该提醒「把剩下的读与筛拆给子 Agent」。
//
// 判据与轮内折叠一致：上一轮真实的 input_tokens 达到上下文预算（窗口 60%）。
// 两件事的分工是：折叠是补救（把已发生的膨胀压回去），派发是源头治理
// （不让主上下文继续长大）。只折叠不派发，压完还会再涨回去。
func (a *Agent) needDispatchHint(in agent.RunInput, lastInputTokens int, messages []llm.Message) bool {
	if lastInputTokens <= 0 || !a.canDispatch(in) {
		return false
	}
	limit := a.contextBudget()
	if limit <= 0 || lastInputTokens < limit {
		return false
	}
	for _, m := range messages {
		if strings.HasPrefix(m.Content, dispatchPressureHint) {
			return false
		}
	}
	return true
}

// dispatchPressureText 是拆解提醒的正文：说清「现在多大、接下来该怎么做」。
func dispatchPressureText(inputTokens, limit int) string {
	return fmt.Sprintf(
		"%s 主上下文已接近窗口上限（上一轮输入约 %d token，折叠阈值 %d）。\n"+
			"从现在起不要把更多材料整段读进主上下文：把剩余的检索、多文件读取、长日志筛选取证"+
			"拆成 agent_dispatch 子任务，任务描述要自包含（子 Agent 看不到本会话历史），"+
			"只要它回结论、关键片段与产物路径。\n"+
			"主上下文只保留这些结论；信息已经够的部分直接收尾作答，不要为补齐细节继续扩大读取。",
		dispatchPressureHint, inputTokens, limit)
}

// maxDepth 返回允许的最大派发深度；配置缺失时用 2（主 → 子 → 孙）。
func (a *Agent) maxDepth() int {
	if a.cfg.AgentMaxDepth > 0 {
		return a.cfg.AgentMaxDepth
	}
	return 2
}

// dispatchTool 构造派发工具。它闭包住父运行的工具集合与预算，
// 因此子 Agent 不可能拿到父 Agent 没有的工具。
func (a *Agent) dispatchTool(parent agent.RunInput, parentTools []tool.Tool) tool.Tool {
	return tool.Tool{
		Name:       dispatchToolName,
		Capability: "agent.dispatch",
		Description: "把一个边界清晰的子任务交给独立的子 Agent 完成，只拿回结论。" +
			"适合：需要读很多文件/检索大量材料但主流程只需要结论、需要并行推进的多条独立线索、" +
			"或需要一套不同工具组合的子任务。主上下文已经装了大量材料（多文件、长日志、成片检索结果）时，" +
			"必须用它对剩余的读取与筛选做隔离，只把结论与产物路径收回主上下文。" +
			"不适合：一句话就能答的问题、需要与用户来回确认的动作。" +
			"子 Agent 看不到当前对话历史，任务描述必须自包含。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "自包含的子任务描述：要查什么、产出什么格式、边界在哪里"},
    "tools": {"type": "array", "items": {"type": "string"}, "description": "只允许子 Agent 使用的工具名；省略表示继承父 Agent 的全部工具"},
    "max_rounds": {"type": "integer", "description": "子 Agent 的轮次上限，默认 3，不能超过父 Agent 的配置上限"},
    "context": {"type": "string", "description": "必要的背景材料（子 Agent 看不到父会话历史）"}
  },
  "required": ["task"],
  "additionalProperties": false
}`),
		// 子 Agent 的每一次实际动作仍会各自经过 Invoker 的策略判定与审计，
		// 派发本身不放大权限，因此按「只读但消耗资源」处理（L2）。
		Risk:        policy.RiskReadCost,
		SideEffects: policy.SideEffectsReadOnly,
		// 派发本身不放大权限：真正的判定发生在子 Agent 的每一次工具调用上。
		Runtime: tool.RuntimeBuiltin,
		Invoke: func(ctx context.Context, args json.RawMessage, env tool.Env) (string, error) {
			return a.runSubAgent(ctx, env, parent, parentTools, args)
		},
	}
}

// runSubAgent 是 dispatchTool 的执行体。
func (a *Agent) runSubAgent(
	ctx context.Context,
	_ tool.Env,
	parent agent.RunInput,
	parentTools []tool.Tool,
	args json.RawMessage,
) (string, error) {
	var in struct {
		Task      string   `json:"task"`
		Tools     []string `json:"tools"`
		MaxRounds int      `json:"max_rounds"`
		Context   string   `json:"context"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return "", fmt.Errorf("task 不能为空：子 Agent 看不到父会话历史，任务必须自包含")
	}
	if len([]rune(task)) > maxSubAgentTaskRunes {
		return "", fmt.Errorf("task 过长（上限 %d 字），请先把它拆成更小的子任务", maxSubAgentTaskRunes)
	}
	if ctxText := strings.TrimSpace(in.Context); ctxText != "" {
		task += "\n\n背景材料：\n" + ctxText
	}

	depth := parent.Depth + 1
	if depth > a.maxDepth() {
		return "", fmt.Errorf("已达到派发深度上限（%d 层），请自己完成这个子任务", a.maxDepth())
	}

	subTools, err := a.subToolSet(parentTools, in.Tools)
	if err != nil {
		return "", err
	}
	// 没有工具的子 Agent 只能空转：宁可当场报错，也不要派一个只能瞎猜的子任务出去
	if len(subTools) == 0 {
		return "", fmt.Errorf("没有可用的工具可以交给子 Agent：先给父 Agent 绑定工具，或省掉 tools 参数")
	}

	rounds := in.MaxRounds
	if rounds <= 0 {
		rounds = defaultSubAgentRounds
	}
	if rounds > a.cfg.LLMMaxRounds {
		rounds = a.cfg.LLMMaxRounds
	}
	if parent.RoundLimit > 0 && rounds > parent.RoundLimit {
		rounds = parent.RoundLimit
	}
	// 子任务的剩余预算不能超过父任务的剩余预算，否则「一次派发」会变成预算放大器
	rounds = minPositive(rounds, parentRemainingRounds(parent, a.cfg.LLMMaxRounds))

	res, runErr := a.Run(ctx, agent.RunInput{
		Env:            parent.Env,
		Query:          task,
		Tools:          subTools,
		Depth:          depth,
		RoundLimit:     rounds,
		Deadline:       parent.Deadline,
		AllowDispatch:  depth < a.maxDepth(),
		SubAgentPrompt: subAgentPrompt,
		Dir:            parent.Dir,
		DirPlugin:      parent.DirPlugin,
		ActiveSkill:    nil, // 技能属于父会话的上下文，不传给子 Agent
		AgentPrompt:    parent.AgentPrompt,
		// 部署事实是环境，不是父会话的上下文：子 Agent 同样需要知道租户与 IM
		Platform: parent.Platform,
		// 「现在」也一起继承：子任务与父任务几乎是同一时刻发起的，
		// 各自再取一次服务器时间会与父级给出的日期打架
		Clock:   parent.Clock,
		History: nil,
	}, nil)
	if runErr != nil && res.Content == "" {
		return "", fmt.Errorf("子 Agent 未产出结果：%w", runErr)
	}

	content := res.Content
	truncated := false
	if len(content) > maxSubAgentResultBytes {
		content = safeTruncate(content, maxSubAgentResultBytes)
		truncated = true
	}
	payload := map[string]any{
		"status":     "ok",
		"stop":       res.StopReason,
		"tool_calls": len(res.Steps),
		"result":     content,
	}
	if truncated {
		payload["truncated"] = true
	}
	if runErr != nil {
		// 子 Agent 被预算或停滞收尾，但已经产出了可用结论：把结论带回去，
		// 同时把收尾原因显式告诉主 Agent，避免它以为任务已经完整完成。
		payload["status"] = "partial"
		payload["reason"] = runErr.Error()
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// subToolSet 按名字挑出子 Agent 可用的工具。空表示继承父 Agent 的全部工具。
func (a *Agent) subToolSet(parentTools []tool.Tool, names []string) ([]tool.Tool, error) {
	if len(names) == 0 {
		out := make([]tool.Tool, 0, len(parentTools))
		for _, t := range parentTools {
			if t.Name == dispatchToolName {
				continue
			}
			out = append(out, t)
		}
		return out, nil
	}
	available := make(map[string]struct{}, len(parentTools))
	for _, t := range parentTools {
		available[t.Name] = struct{}{}
	}
	out := make([]tool.Tool, 0, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		t, ok := tool.Find(parentTools, name)
		if !ok || name == dispatchToolName {
			missing = append(missing, name)
			continue
		}
		out = append(out, t)
	}
	if len(missing) > 0 {
		// 不给「悄悄少一个工具」的结果：子 Agent 会以为自己能力齐全
		return nil, fmt.Errorf("子 Agent 不可用这些工具：%s（父 Agent 没有，或不允许派发自身）",
			strings.Join(missing, ", "))
	}
	return out, nil
}

// parentRemainingRounds 估算父 Agent 还剩多少轮可用，作为子任务轮次的硬上限。
func parentRemainingRounds(parent agent.RunInput, configured int) int {
	limit := configured
	if parent.RoundLimit > 0 && parent.RoundLimit < limit {
		limit = parent.RoundLimit
	}
	if limit <= 0 {
		return defaultSubAgentRounds
	}
	return limit
}

func minPositive(a, b int) int {
	if b <= 0 {
		return a
	}
	if a <= 0 || b < a {
		return b
	}
	return a
}

// safeTruncate 按字节截断但保证 UTF-8 完整，避免把中文切成乱码。
func safeTruncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if idx := strings.LastIndexFunc(cut, func(r rune) bool { return r == '\uFFFD' }); idx >= 0 {
		cut = cut[:idx]
	}
	return strings.ToValidUTF8(cut, "") + "…（子 Agent 结论已截断）"
}
