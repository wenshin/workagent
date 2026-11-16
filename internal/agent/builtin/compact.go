package builtin

import (
	"context"
	"strconv"
	"strings"

	"github.com/workagent/auto-agent/internal/llm"
)

// 轮内上下文压缩（§5.7.2 的轮内部分）。
//
// 跨轮压缩由 ctxmgr 在调用前完成，管的是「上一次对话到现在」。但一轮长任务里，
// messages 会随每次工具回执线性增长，而且每一轮都把整份重发：轮次上限放开到 40 之后，
// 这条路径足以把上下文顶到模型窗口之外。撞上去不是降级，是模型直接报错、整轮失败。
//
// 这里的取舍是「丢过程、留结论」：较早的工具往返折成一段可读的进展记录，
// 保留系统提示词、最近几轮的原始回执、以及每一轮已经说给用户的话。
//
// 判决与摘要都不调模型——压缩必须确定性、零失败、零成本，否则它自己就成了新的故障点。
const (
	// compactKeepRounds 是压缩时保留完整工具往返的最近轮数。
	// 3 轮的原始回执足够模型接着做事；更早的已经体现在进展记录里。
	compactKeepRounds = 3
	// compactEmergencyRounds 是「已经撞了窗口」后的紧急档：只留最近一轮。
	compactEmergencyRounds = 1
	// compactNoteRunes 是折叠记录里「助手当时在干什么」的长度
	compactNoteRunes = 200
	// compactResultRunes 是折叠记录里单条工具结果的长度
	compactResultRunes = 160
	// compactMaxBlocks 是折叠记录最多保留的步骤数，避免它自己又变成一大段
	compactMaxBlocks = 40
	// compactArgRunes 是折叠记录里工具参数的长度
	compactArgRunes = 120
)

// msgRange 是 messages 里一段连续区间 [start, end)。
type msgRange struct{ start, end int }

// chatRound 发起一轮模型调用；上下文溢出时折叠一次再重试。
//
// 只有「一个增量都没吐过」才重试：吐过字再重放，用户会看到答案重复一遍，
// 而且已经发出去的事件无法撤回。
func (a *Agent) chatRound(
	ctx context.Context,
	messages []llm.Message,
	defs []llm.Tool,
	onDelta func(llm.Delta) error,
) (llm.Result, string, []llm.Message, error) {
	emitted := 0
	wrap := func(d llm.Delta) error {
		if d.Reasoning != "" || d.Content != "" {
			emitted++
		}
		return onDelta(d)
	}

	res, provider, err := a.llm.Chat(ctx, llm.RouteChat, llm.ChatRequest{Messages: messages, Tools: defs}, wrap)
	if err == nil || emitted > 0 || !isContextOverflow(err) {
		return res, provider, messages, err
	}

	compact, ok := compactMessages(messages, compactEmergencyRounds)
	if !ok {
		return res, provider, messages, err
	}
	a.log.Warn("模型上下文溢出，折叠后重试",
		"err", err, "messages_before", len(messages), "messages_after", len(compact))
	res, provider, err = a.llm.Chat(ctx, llm.RouteChat, llm.ChatRequest{Messages: compact, Tools: defs}, wrap)
	return res, provider, compact, err
}

// fitContext 在每次模型调用前检查上下文规模，必要时折叠较早的工具往返。
//
// 判据优先用上一轮真实的 input_tokens（模型自己报的，比估算准），
// 拿不到时再按字数估。两者都不可用时宁可不动——压错比不压更糟。
func (a *Agent) fitContext(messages []llm.Message, lastInputTokens int) []llm.Message {
	limit := a.contextBudget()
	if limit <= 0 {
		return messages
	}
	if lastInputTokens <= 0 {
		lastInputTokens = estimateMessages(messages)
	}
	if lastInputTokens < limit {
		return messages
	}

	out, ok := compactMessages(messages, compactKeepRounds)
	if !ok {
		return messages
	}
	a.log.Info("上下文接近窗口上限，折叠较早的工具往返",
		"input_tokens", lastInputTokens, "limit", limit,
		"messages_before", len(messages), "messages_after", len(out))
	return out
}

// contextBudget 返回单次模型调用的输入 token 预算。
//
// 取窗口的 60%：留出输出、系统提示词增长与估算误差的余量。
// 超过它就该折叠，而不是等模型报「context length exceeded」再抢救。
func (a *Agent) contextBudget() int {
	window := a.cfg.LLMContextTokens
	if window <= 0 {
		return 0
	}
	return window * 6 / 10
}

// compactMessages 把较早的工具往返折进一条系统消息。
//
// 只折叠「带 tool_calls 的 assistant + 其后的 tool 回执」这一整组：拆开会让
// 调用与回执配对不上，OpenAI 与 Anthropic 都会直接拒绝整个请求。
func compactMessages(messages []llm.Message, keepRounds int) ([]llm.Message, bool) {
	if keepRounds < 0 {
		keepRounds = 0
	}
	groups := toolGroupRanges(messages)
	if len(groups) <= keepRounds {
		return messages, false
	}

	folded := groups[:len(groups)-keepRounds]

	// 上一次折叠留下的记录要并进来，而不是再插一条：
	// 每折一次就多一条 system 消息的话，上下文反而越压越长
	prior, priorAt := takeDigest(messages)
	lines := append(prior, digestLines(messages, folded)...)
	omitted := false
	if len(lines) > compactMaxBlocks {
		// 超了就丢最早的：离当前越近的步骤越有用
		lines = lines[len(lines)-compactMaxBlocks:]
		omitted = true
	}
	digest := buildDigest(lines, omitted)

	out := make([]llm.Message, 0, len(messages)+1)
	next, inserted := 0, false
	for i := 0; i < len(messages); {
		switch {
		case i == priorAt:
			// 旧摘要被新摘要取代
			out = append(out, llm.Message{Role: llm.RoleSystem, Content: digest})
			inserted = true
			i++
		case next < len(folded) && i == folded[next].start:
			if !inserted {
				out = append(out, llm.Message{Role: llm.RoleSystem, Content: digest})
				inserted = true
			}
			i = folded[next].end
			next++
		default:
			out = append(out, messages[i])
			i++
		}
	}
	if !inserted {
		out = append(out, llm.Message{Role: llm.RoleSystem, Content: digest})
	}
	return out, true
}

// digestHeader 是折叠记录的首行，也是「这是一条折叠记录」的识别依据。
const digestHeader = "【早期执行步骤（为节省上下文已折叠）】"

// takeDigest 取出已有的折叠记录（最多一条）及其位置。
func takeDigest(messages []llm.Message) ([]string, int) {
	for i, m := range messages {
		if m.Role != llm.RoleSystem || !strings.HasPrefix(m.Content, digestHeader) {
			continue
		}
		var lines []string
		for _, line := range strings.Split(m.Content, "\n") {
			if strings.HasPrefix(line, "- ") {
				lines = append(lines, line)
			}
		}
		return lines, i
	}
	return nil, -1
}

// buildDigest 拼出折叠记录正文。
func buildDigest(lines []string, omitted bool) string {
	var b strings.Builder
	b.WriteString(digestHeader + "\n")
	b.WriteString("只保留结论与关键结果；需要原文请重新调用对应工具读取。\n")
	if omitted {
		b.WriteString("- （更早的步骤已省略）\n")
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	b.WriteString("（以上为已折叠的 " + strconv.Itoa(len(lines)) + " 步操作）")
	return b.String()
}

// toolGroupRanges 找出每一组「assistant(tool_calls) + 其后的 tool 回执」。
func toolGroupRanges(messages []llm.Message) []msgRange {
	var out []msgRange
	for i := 0; i < len(messages); i++ {
		if messages[i].Role != llm.RoleAssistant || len(messages[i].ToolCalls) == 0 {
			continue
		}
		j := i + 1
		for j < len(messages) && messages[j].Role == llm.RoleTool {
			j++
		}
		out = append(out, msgRange{start: i, end: j})
		i = j - 1
	}
	return out
}

// digestLines 把被折叠的几轮写成几行「做了什么、看到什么」。
//
// 记的是线索不是全文：折叠的目的是给模型足够的方向感，
// 让它知道自己已经走到哪、哪些路已经试过，需要细节时它会重新调用工具。
func digestLines(messages []llm.Message, folded []msgRange) []string {
	lines := make([]string, 0, len(folded))
	for _, g := range folded {
		if note := oneLine(messages[g.start].Content); note != "" {
			lines = append(lines, "- 当时在做："+clip(note, compactNoteRunes))
		}
		for _, call := range messages[g.start].ToolCalls {
			line := "- " + call.Function.Name + "(" + clip(oneLine(call.Function.Arguments), compactArgRunes) + ")"
			if res := stripGuardHint(toolResult(messages, g, call.ID)); res != "" {
				line += " → " + clip(oneLine(res), compactResultRunes)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// toolResult 在折叠组内按 id 找对应的工具回执。
func toolResult(messages []llm.Message, g msgRange, callID string) string {
	for i := g.start + 1; i < g.end; i++ {
		if messages[i].Role == llm.RoleTool && messages[i].ToolCallID == callID {
			return messages[i].Content
		}
	}
	return ""
}

// estimateMessages 在模型没有回报 usage 时估算输入规模。
func estimateMessages(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += llm.EstimateTokens(m.Content)
		for _, call := range m.ToolCalls {
			total += llm.EstimateTokens(call.Function.Name) + llm.EstimateTokens(call.Function.Arguments)
		}
		// 每条消息的角色、分隔符与结构开销
		total += 4
	}
	return total
}

// isContextOverflow 判断错误是不是「上下文超了模型窗口」。
//
// 各家措辞不同，而且都是 400 里的一句话，没有结构化字段可依赖，
// 只能认这些特征串。认错方向的代价是多重试一次，比直接失败小。
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context length", "context_length", "maximum context",
		"context window", "too many tokens", "prompt is too long",
		"input is too long", "reduce the length", "maximum token",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// oneLine 把多行文本压成一行，避免折叠记录自己撑满上下文。
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// stripGuardHint 去掉进度守卫贴在回执前面的提醒。
//
// 那几句是给「当时那个模型」看的，不是这次运行的结果；留在折叠记录里既占额度，
// 又会让后来读到它的人（模型或人）以为工具真的返回了这句话。
func stripGuardHint(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[进度提醒]") {
		return s
	}
	if idx := strings.Index(s, "\n\n"); idx >= 0 {
		return strings.TrimSpace(s[idx+2:])
	}
	return ""
}

// clip 按字截断并加省略号。
func clip(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
