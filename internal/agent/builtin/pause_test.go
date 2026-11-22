package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// pausingProvider 第一轮就要求调用一个需要确认的工具，之后不再被问到。
type pausingProvider struct {
	call  llm.ToolCall
	chats *atomic.Int32
}

func (p pausingProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "stub", Kind: "openai", Model: "stub"}
}

func (p pausingProvider) Chat(context.Context, llm.ChatRequest, llm.DeltaFunc) (llm.Result, error) {
	// 停在审批上之后不该再问模型：多问一次就是在猜用户会怎么点
	p.chats.Add(1)
	return llm.Result{ToolCalls: []llm.ToolCall{p.call}}, nil
}

func (pausingProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

// parkedApprover 模拟会话层：登记待办、回「已暂停」。
type parkedApprover struct{ asked atomic.Int32 }

func (p *parkedApprover) Decide(context.Context, tool.ApprovalRequest) (tool.ApprovalDecision, error) {
	p.asked.Add(1)
	return tool.ApprovalDecision{}, tool.ErrApprovalParked
}

// 命中审批的那一轮要立刻结束，而不是原地等人。
//
// 三个断言分别防三种回归：调用没执行（不能因为「反正要问」就先跑一半）、
// 没产回执（半截回执会让模型以为做完了）、断点里留着这条调用（否则点同意时无事可续）。
func TestRunPausesOnParkedApproval(t *testing.T) {
	var executed atomic.Int32
	var chats atomic.Int32
	approver := &parkedApprover{}

	writeTool := tool.Tool{
		Name: "mcp_write",
		Risk: 3,
		// 安装方档位「每次调用都确认」：不必构造命令名单也能命中审批
		ApprovalLevel: tool.ApprovalAlways,
		Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
			executed.Add(1)
			return "已写入", nil
		},
	}
	call := llm.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "mcp_write", Arguments: `{"id":"1"}`},
	}

	router := llm.NewRouter(nil)
	router.Register(pausingProvider{call: call, chats: &chats})
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "stub"})

	a, err := New(agent.Deps{
		Config: &config.Config{
			LLMMaxRounds: 8, LLMMaxToolCalls: 20, LLMMaxParallel: 2, AgentStallRounds: 3,
		},
		LLM:     router,
		Invoker: tool.NewInvoker(nil, resumeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造 agent 失败: %v", err)
	}

	var last agent.ResumeState
	var haveCheckpoint bool
	var toolResults atomic.Int32
	res, err := a.Run(context.Background(), agent.RunInput{
		Tools: []tool.Tool{writeTool},
		Query: "把这条记录改一下",
		Env: tool.Env{
			UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r1",
			Approvals: approver,
		},
		Checkpoint: func(s agent.ResumeState) error {
			last, haveCheckpoint = s, true
			return nil
		},
	}, func(ev agent.Event) error {
		if ev.Type == agent.EventToolResult {
			toolResults.Add(1)
		}
		return nil
	})

	if !errors.Is(err, agent.ErrPaused) {
		t.Fatalf("应停在这一轮（ErrPaused），实际 %v", err)
	}
	if n := executed.Load(); n != 0 {
		t.Fatalf("没拿到许可的调用不能执行，实际执行 %d 次", n)
	}
	if n := toolResults.Load(); n != 0 {
		t.Fatalf("暂停的调用不该产回执，实际 %d 条", n)
	}
	if n := chats.Load(); n != 1 {
		t.Fatalf("停下来之后不该再问模型，实际问 %d 次", n)
	}
	if n := approver.asked.Load(); n != 1 {
		t.Fatalf("应只登记一次待办，实际 %d 次", n)
	}
	if !haveCheckpoint || len(last.Pending) != 1 || last.Pending[0].ID != call.ID {
		t.Fatalf("断点里要留着这条没执行的调用，实际 %+v", last.Pending)
	}
	// 挂起的调用也要交给会话层落进这一轮的消息：只有断点没有消息的话，
	// 界面上这一轮就是空的
	if len(res.Parked) != 1 || res.Parked[0].ID != call.ID || res.Parked[0].Name != "mcp_write" {
		t.Fatalf("挂起的调用要带出去，实际 %+v", res.Parked)
	}
	if res.StopReason != "awaiting_approval" {
		t.Fatalf("停止原因应标明是在等人，实际 %q", res.StopReason)
	}
}
