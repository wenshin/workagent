package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
)

// 流式事件类型，与前端 SSE 事件名保持一致。
const (
	EventReasoning    = "reasoning"
	EventText         = "text"
	EventToolStart    = "tool_start"
	EventToolResult   = "tool_result"
	EventDone         = "done"
	EventError        = "error"
)

// 单轮对话内的容量上限，避免模型输出把上下文撑爆。
const (
	maxToolArgBytes    = 8 * 1024
	maxToolResultBytes = 8 * 1024
	maxToolFeedBytes   = 4 * 1024
)

// Event 是 Agent 运行过程中推送给前端的增量。
type Event struct {
	Type  string
	Delta string
	Tool  *ToolState
}

// ToolState 是一次工具调用的完整状态。
type ToolState struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Args       string `json:"args"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
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
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// RunInput 是一次 Agent 运行的输入。
type RunInput struct {
	Scope   Scope
	History []llm.Message
}

// Result 是一次 Agent 运行的产出。
type Result struct {
	Content   string
	Reasoning string
	Steps     []Step
}

// Service 编排「模型 → 工具 → 模型」的循环。
type Service struct {
	cfg   *config.Config
	llm   *llm.Client
	store store.Store
	log   *slog.Logger
}

// NewService 构造 Agent 服务。
func NewService(cfg *config.Config, client *llm.Client, st store.Store, log *slog.Logger) *Service {
	return &Service{cfg: cfg, llm: client, store: st, log: log}
}

// Ready 判断模型是否可用。
func (s *Service) Ready() bool { return s.cfg.LLMReady() }

// Model 返回当前模型名，用于前端提示。
func (s *Service) Model() string { return s.cfg.LLMModel }

// Run 执行一次带工具调用的对话，过程中通过 emit 推送增量事件。
func (s *Service) Run(ctx context.Context, in RunInput, emit func(Event) error) (Result, error) {
	if !s.Ready() {
		return Result{}, fmt.Errorf("模型服务未配置：请在 .env 中设置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
	}

	tools := s.buildTools()
	defs := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, t.Definition())
	}

	messages := make([]llm.Message, 0, len(in.History)+2)
	messages = append(messages, systemMessage())
	messages = append(messages, in.History...)

	var res Result
	var reasoning strings.Builder

	for round := 0; round < s.cfg.LLMMaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		out, err := s.llm.Chat(ctx, llm.ChatRequest{Messages: messages, Tools: defs}, func(d llm.Delta) error {
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				return emit(Event{Type: EventReasoning, Delta: d.Reasoning})
			}
			if d.Content != "" {
				res.Content += d.Content
				return emit(Event{Type: EventText, Delta: d.Content})
			}
			return nil
		})
		if err != nil {
			return res, err
		}

		if len(out.ToolCalls) == 0 {
			res.Reasoning = reasoning.String()
			return res, nil
		}

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
			if err := emit(Event{Type: EventToolStart, Tool: state}); err != nil {
				return res, err
			}

			start := time.Now()
			result, runErr := s.execTool(ctx, tools, call, in.Scope)
			state.DurationMS = time.Since(start).Milliseconds()
			state.Status = "ok"
			if runErr != nil {
				state.Status = "error"
				result = runErr.Error()
			}
			state.Result = truncate(result, maxToolResultBytes)

			if err := emit(Event{Type: EventToolResult, Tool: state}); err != nil {
				return res, err
			}

			res.Steps = append(res.Steps, Step{
				Type:       "tool",
				ID:         state.ID,
				Name:       state.Name,
				Args:       state.Args,
				Result:     state.Result,
				Status:     state.Status,
				DurationMS: state.DurationMS,
			})

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Content:    truncate(result, maxToolFeedBytes),
			})
		}
	}

	// 轮次用尽：把已有的正文与轨迹返回，前端会提示模型未收敛
	res.Reasoning = reasoning.String()
	return res, nil
}

func (s *Service) execTool(ctx context.Context, tools []Tool, call llm.ToolCall, scope Scope) (string, error) {
	tool, ok := findTool(tools, call.Function.Name)
	if !ok {
		return "", fmt.Errorf("未知工具：%s", call.Function.Name)
	}

	args := json.RawMessage(call.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if len(args) > maxToolArgBytes {
		return "", fmt.Errorf("工具参数过大")
	}

	out, err := tool.Run(ctx, args, scope)
	if err != nil {
		s.log.Warn("工具执行失败", "tool", tool.Name, "err", err)
		return "", err
	}
	return out, nil
}

func systemMessage() llm.Message {
	const prompt = `你是 WorkAgent 工作台里的智能助手，帮助用户在「空间」中完成工作。

要求：
- 用中文回答，简洁直接，先给结论再展开。
- 可以使用工具查询当前时间、用户的空间列表和当前空间的会话列表；需要事实依据时先调用工具，不要凭空编造数据。
- 不清楚的信息要明确说明，而不是猜测。
- 代码用 Markdown 代码块输出，并标注语言。
- 不执行任何写操作：当前版本的工具都是只读的。`

	return llm.Message{Role: llm.RoleSystem, Content: prompt}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…（已截断）"
}
