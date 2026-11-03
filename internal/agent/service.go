package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// 流式事件类型，与方案 §7.3 的运行流示例逐字对齐。
const (
	EventRunStarted   = "run.started"
	EventMessageDelta = "message.delta"
	EventToolCall     = "tool.call"
	EventToolResult   = "tool.result"
	EventApprovalReq  = "approval.req"
	EventMessageDone  = "message.done"
	EventRunFinished  = "run.finished"
)

// 单轮对话内的容量上限，避免模型输出把上下文撑爆。
const (
	maxToolArgBytes  = 8 * 1024
	maxToolFeedBytes = 4 * 1024
)

// Event 是 Agent 运行过程中推送给前端的增量（§7.3）。
type Event struct {
	Type string
	// RunID 由会话层在 run.started 中填充
	RunID string
	// Role 是 message.delta 的角色，固定为 assistant
	Role string
	// Delta 是 message.delta 的文本；Reasoning 为 true 时表示思考过程
	Delta     string
	Reasoning bool
	Tool      *ToolState
	Approval  *ApprovalState
}

// ApprovalState 是一条审批请求（§5.7.5 / §7.3 approval.req）。
type ApprovalState struct {
	ID       string `json:"approval_id"`
	CallID   string `json:"call_id"`
	Tool     string `json:"tool"`
	Reason   string `json:"reason,omitempty"`
	TimeoutS int    `json:"timeout_s"`
}

// ToolState 是一次工具调用的完整状态。
type ToolState struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Args       string `json:"args"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status"`
	Risk       int    `json:"risk,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Payload 返回 §7.3 的字段名，SSE 与 checkpoint 共用，避免两处命名漂移。
func (t *ToolState) Payload() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	payload := map[string]any{
		"call_id": t.ID,
		"tool":    t.Name,
		"args":    t.Args,
		"status":  t.Status,
		"risk":    t.Risk,
	}
	if t.Result != "" {
		payload["summary"] = t.Result
	}
	if t.DurationMS > 0 {
		payload["duration_ms"] = t.DurationMS
	}
	return payload
}

// Step 是落库的一条执行轨迹，用于历史消息还原。
type Step struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Args       string `json:"args,omitempty"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status,omitempty"`
	Risk       int    `json:"risk,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// RunInput 是一次 Agent 运行的输入。
type RunInput struct {
	// Env 决定工具能看到的资源范围与权限
	Env tool.Env
	// History 是已组装好的上下文（含 system prompt 之外的历史消息）
	History []llm.Message
	// Query 是用户本轮输入，用于工具相关性裁剪
	Query string
	// Tools 非空时直接使用（测试用）；为空则按注册表加载
	Tools []tool.Tool
	// Skills 是当前空间 + 系统空间合并后的技能索引，注入系统提示词的「可用技能」段
	Skills []tool.SourceEntry
	// ActiveSkill 是会话绑定的技能，正文直接进系统提示词
	ActiveSkill *ActiveSkill
}

// ActiveSkill 是会话选中的技能。
type ActiveSkill struct {
	Name string
	Path string
	Doc  string
}

// Result 是一次 Agent 运行的产出。
type Result struct {
	Content   string
	Reasoning string
	Steps     []Step
	// NeedApproval 表示本次执行中至少有一次工具调用被策略要求审批
	NeedApproval bool
	Provider     string
	Model        string
	Usage        llm.Usage
}

// Service 编排「模型 → 工具 → 模型」的循环（§5.7.2 主循环、§5.7.7 ToolInvoker）。
type Service struct {
	cfg      *config.Config
	llm      *llm.Router
	registry *tool.Registry
	invoker  *tool.Invoker
	log      *slog.Logger
}

// NewService 构造 Agent 服务。
func NewService(cfg *config.Config, router *llm.Router, registry *tool.Registry, invoker *tool.Invoker, log *slog.Logger) *Service {
	return &Service{cfg: cfg, llm: router, registry: registry, invoker: invoker, log: log}
}

// Ready 判断模型路由是否可用。
func (s *Service) Ready() bool { return s.llm != nil && s.llm.Ready() }

// Budget 返回本次执行的预算上限，写入 run.budget（§5.7.2 Token 预算、§6 run）。
func (s *Service) Budget() map[string]any {
	return map[string]any{
		"max_rounds":       s.cfg.LLMMaxRounds,
		"max_tool_calls":   s.cfg.LLMMaxToolCalls,
		"max_wall_clock_s": int(s.cfg.LLMMaxWallClock.Seconds()),
		"max_tokens":       s.cfg.LLMMaxTokens,
	}
}

// Model 返回当前路由命中的模型名。
func (s *Service) Model() string {
	if s.llm == nil {
		return ""
	}
	return s.llm.Model(llm.RouteChat)
}

// Run 执行一次带工具调用的对话，过程中通过 emit 推送增量事件。
func (s *Service) Run(ctx context.Context, in RunInput, emit func(Event) error) (Result, error) {
	if !s.Ready() {
		return Result{}, fmt.Errorf("模型服务未配置：请设置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
	}

	tools := in.Tools
	if len(tools) == 0 && s.registry != nil {
		loaded, err := s.registry.Load(ctx, in.Env, in.Query)
		if err != nil {
			return Result{}, fmt.Errorf("加载工具失败：%w", err)
		}
		tools = loaded
	}
	defs := tool.Definitions(tools)

	messages := make([]llm.Message, 0, len(in.History)+2)
	messages = append(messages, systemMessage(in))
	messages = append(messages, in.History...)

	var res Result
	var reasoning strings.Builder

	deadline := time.Now().Add(s.cfg.LLMMaxWallClock)
	toolCalls := 0

	for round := 0; round < s.cfg.LLMMaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("本轮对话超过时间预算（%s）", s.cfg.LLMMaxWallClock)
		}

		out, provider, chatErr := s.llm.Chat(ctx, llm.RouteChat, llm.ChatRequest{Messages: messages, Tools: defs}, func(d llm.Delta) error {
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				return emit(Event{Type: EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
			}
			if d.Content != "" {
				res.Content += d.Content
				return emit(Event{Type: EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Content})
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

		if toolCalls+len(out.ToolCalls) > s.cfg.LLMMaxToolCalls {
			return res, fmt.Errorf("工具调用次数超过上限（%d）", s.cfg.LLMMaxToolCalls)
		}
		toolCalls += len(out.ToolCalls)

		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   out.Content,
			ToolCalls: out.ToolCalls,
		})

		for _, call := range out.ToolCalls {
			state := &ToolState{
				ID:     call.ID,
				Name:   call.Function.Name,
				Args:   truncate(call.Function.Arguments, maxToolArgBytes),
				Status: "running",
			}
			if t, ok := tool.Find(tools, call.Function.Name); ok {
				state.Risk = t.Risk
			}
			if err := emit(Event{Type: EventToolCall, Tool: state}); err != nil {
				return res, err
			}
		}

		// 同一轮的多个调用并行执行，但事件与消息拼接保持确定顺序
		results := make([]tool.InvokeResult, len(out.ToolCalls))
		sem := make(chan struct{}, maxParallel(s.cfg.LLMMaxParallel))
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
				results[i] = s.invoker.Invoke(ctx, t, call, in.Env)
			}(i, call)
		}
		wg.Wait()

		for i, call := range out.ToolCalls {
			r := results[i]
			state := &ToolState{
				ID:         call.ID,
				Name:       call.Function.Name,
				Args:       truncate(call.Function.Arguments, maxToolArgBytes),
				Result:     truncate(r.Content, 8*1024),
				Status:     r.Status,
				Risk:       r.Risk,
				DurationMS: r.LatencyMS,
			}
			if err := emit(Event{Type: EventToolResult, Tool: state}); err != nil {
				return res, err
			}
			// 策略判定需要审批时额外推一条审批请求（§5.7.5），前端据此挂起并提示
			if state.Status == tool.StatusApproval {
				res.NeedApproval = true
				if err := emit(Event{Type: EventApprovalReq, Approval: &ApprovalState{
					ID:       approvalID(call.ID),
					CallID:   call.ID,
					Tool:     state.Name,
					Reason:   r.Content,
					TimeoutS: approvalTimeoutS,
				}}); err != nil {
					return res, err
				}
			}

			res.Steps = append(res.Steps, Step{
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
				Content:    truncate(r.Content, maxToolFeedBytes),
			})
		}
	}

	res.Reasoning = reasoning.String()
	return res, nil
}

func systemMessage(in RunInput) llm.Message {
	const prompt = `你是 WorkAgent 工作台里的智能助手，帮助用户在「空间」中完成工作。

要求：
- 用中文回答，简洁直接，先给结论再展开。
- 需要事实依据时先调用工具（时间、空间列表、文件列表等），不要凭空编造。
- 能力源（skills / tools / mcps）来自「当前空间 + 系统空间」，同名时当前空间优先：需要了解平台有哪些能力时先调用 global_source_list，再用 global_source_read 读取具体文件（如 SKILL.md、工具定义 JSON）。
- 文件能力只作用于当前空间的文件根目录：file_list 列出目录与文件，file_read 读取正文，file_write 写入 md / json / txt / yaml。修改已有文件前先用 file_read 读一遍，避免覆盖用户内容；不要尝试读写空间之外的路径。
- 用户要「做一个技能」时，先用 skill_create 建技能（生成 .meta.json 与 SKILL.md），再用 file_write 完善技能正文；只写 SKILL.md 不建目录不会生成技能。
- 工具返回「需要审批」或「策略拒绝」时，如实告知用户原因，不要重试同一工具。
- 不清楚的信息要明确说明，而不是猜测。
- 代码用 Markdown 代码块输出，并标注语言。`

	var b strings.Builder
	b.WriteString(prompt)

	// 方案一：技能索引常驻（名称 + 描述 + 路径），正文按需读取，避免一次性塞满上下文
	if len(in.Skills) > 0 {
		b.WriteString("\n\n## 可用技能\n")
		b.WriteString("以下是当前空间与系统空间的技能索引（同名时当前空间优先）。任务与某个技能相关时，先用 global_source_read 读取它的 SKILL.md 再按步骤执行；不要凭名称猜测内容。\n")
		for i, s := range in.Skills {
			if i >= maxSkillIndexItems {
				b.WriteString("- 其余技能可用 global_source_list 查看\n")
				break
			}
			where := "当前空间"
			if s.Source == "system" {
				where = "系统空间"
			}
			b.WriteString("- " + s.Name + "（" + where + "）：" + truncate(s.Description, 80) + " → " + s.Path + "\n")
		}
	}

	// 方案二：会话选中的技能，正文直接注入本轮
	if in.ActiveSkill != nil && strings.TrimSpace(in.ActiveSkill.Doc) != "" {
		b.WriteString("\n\n## 本轮选中的技能：")
		b.WriteString(in.ActiveSkill.Name)
		b.WriteString("\n用户已为本会话选择该技能，优先按下面的步骤执行；如果与当前任务无关，说明原因后按常规处理。\n\n")
		b.WriteString(in.ActiveSkill.Doc)
		b.WriteString("\n")
	}

	return llm.Message{Role: llm.RoleSystem, Content: b.String()}
}

// maxSkillIndexItems 限制注入的技能条数，避免索引过长挤占上下文。
const maxSkillIndexItems = 30

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…（已截断）"
}

func maxParallel(configured int) int {
	if configured <= 0 {
		return 4
	}
	return configured
}

// approvalTimeoutS 是审批请求的默认超时（§7.3 approval.req）。
const approvalTimeoutS = 300

func approvalID(callID string) string {
	if callID == "" {
		return "ap_unknown"
	}
	return "ap_" + callID
}
