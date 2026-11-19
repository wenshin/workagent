// Package direct 是无工具的直答实现：一次模型调用直接产出回复（§5.7.2 主循环的退化形态）。
//
// 用途：
//   - 轻量路径：不加载工具 schema、不做循环，首字更快、token 更省；
//   - 对照实现：验证 Agent 契约可替换（AGENT_IMPL=direct），作为接入新编排实现的基准；
//   - 兜底：工具链路异常时仍能完成对话。
//
// 边界：不加载工具、不产生 tool.call / approval.req；事件协议与内置实现完全一致，
// 上层（SSE 契约、checkpoint、预算、上下文压缩）无需为它做任何特判。
package direct

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
)

// Name 是实现名，对应 AGENT_IMPL=direct。
const Name = "direct"

func init() { agent.Register(Name, New) }

// Agent 是直答实现，满足 agent.Agent。
type Agent struct {
	cfg *config.Config
	llm *llm.Router
	log *slog.Logger
}

// New 构造直答实现。
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
	return &Agent{cfg: d.Config, llm: d.LLM, log: d.Log}, nil
}

// Name 返回实现名。
func (a *Agent) Name() string { return Name }

// Capabilities 声明直答实现具备的能力：无工具、无审批。
func (a *Agent) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Tools:         false,
		ParallelTools: false,
		Streaming:     true,
		Reasoning:     true,
		Approval:      false,
	}
}

// Ready 判断模型路由是否可用。
func (a *Agent) Ready() bool { return a.llm != nil && a.llm.Ready() }

// Model 返回当前路由命中的模型名。
func (a *Agent) Model() string { return a.llm.Model(llm.RouteChat) }

// Budget 返回预算上限：单轮、不允许工具调用。
func (a *Agent) Budget() map[string]any {
	return map[string]any{
		"max_rounds":       1,
		"max_tool_calls":   0,
		"max_wall_clock_s": int(a.cfg.LLMMaxWallClock.Seconds()),
		"max_tokens":       a.cfg.LLMMaxTokens,
	}
}

// Run 执行一次直答，流式推送增量。
func (a *Agent) Run(ctx context.Context, in agent.RunInput, emit func(agent.Event) error) (agent.Result, error) {
	var res agent.Result
	if !a.Ready() {
		return res, fmt.Errorf("模型服务未配置：请设置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
	}
	if emit == nil {
		emit = func(agent.Event) error { return nil }
	}

	messages := make([]llm.Message, 0, len(in.History)+1)
	messages = append(messages, agent.BuildSystemMessage(in))
	messages = append(messages, in.History...)

	var content, reasoning strings.Builder

	out, provider, err := a.llm.Chat(ctx, llm.RouteChat, llm.ChatRequest{Messages: messages}, func(d llm.Delta) error {
		if d.Reasoning != "" {
			reasoning.WriteString(d.Reasoning)
			return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Reasoning, Reasoning: true})
		}
		if d.Content != "" {
			content.WriteString(d.Content)
			return emit(agent.Event{Type: agent.EventMessageDelta, Role: llm.RoleAssistant, Delta: d.Content})
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	res.Provider = provider
	res.Model = out.Model
	res.Usage = out.Usage
	// 直答实现永远落在问答档：它不加载工具，也不存在「任务 / 编排」的可能。
	res.Mode = agent.ModeAnswer
	res.Content = content.String()
	res.Reasoning = reasoning.String()
	// Provider 可能只返回聚合结果而没有流式增量，补齐兜底
	if res.Content == "" {
		res.Content = out.Content
	}
	if res.Reasoning == "" {
		res.Reasoning = out.Reasoning
	}

	if len(out.ToolCalls) > 0 {
		a.log.Warn("直答实现不执行工具调用，已忽略本轮 tool_calls",
			"impl", Name, "model", res.Model, "tool_calls", len(out.ToolCalls))
	}
	if res.Content == "" {
		return res, fmt.Errorf("模型没有返回内容，请重试")
	}
	return res, nil
}
