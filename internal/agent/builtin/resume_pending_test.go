package builtin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

type stubProvider struct {
	content string
	calls   *atomic.Int32
}

func (s stubProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "stub", Kind: "openai", Model: "stub"}
}

func (s stubProvider) Chat(_ context.Context, _ llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	s.calls.Add(1)
	return llm.Result{Content: s.content}, nil
}

func (s stubProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

type resumeRecorder struct{}

func (resumeRecorder) RecordToolCall(context.Context, store.ToolCallRecord) error { return nil }
func (resumeRecorder) AppendAuditLog(context.Context, store.AuditEntry) error     { return nil }

// 恢复时先把中断那批里没拿到回执的调用补完，再去问模型。
//
// 不补的话，模型会对着「assistant 发了 tool_call、但没有对应 tool 消息」的序列
// 重新规划，把已经执行过的副作用再做一遍。
func TestRunCompletesPendingOnResume(t *testing.T) {
	var executed atomic.Int32
	readTool := tool.Tool{
		Name: "fs_read",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
			executed.Add(1)
			return "a.md 的正文", nil
		},
	}

	var chats atomic.Int32
	router := llm.NewRouter(nil)
	router.Register(stubProvider{content: "读完了，a.md 的内容是……", calls: &chats})
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "stub"})

	a, err := New(agent.Deps{
		Config:  &config.Config{},
		LLM:     router,
		Invoker: tool.NewInvoker(nil, resumeRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造 agent 失败: %v", err)
	}

	call := llm.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.md"}`},
	}

	res, err := a.Run(context.Background(), agent.RunInput{
		Tools: []tool.Tool{readTool},
		Query: "看看 a.md",
		Env:   tool.Env{UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r2"},
		Resume: &agent.ResumeState{
			Query: "看看 a.md",
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "你是助理"},
				{Role: llm.RoleUser, Content: "看看 a.md"},
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
			},
			Pending: []agent.PendingCall{{ID: call.ID, Name: "fs_read", Args: call.Function.Arguments}},
		},
	}, func(agent.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if n := executed.Load(); n != 1 {
		t.Fatalf("半批调用应被补执行恰好一次，实际 %d", n)
	}
	if chats.Load() != 1 {
		t.Fatalf("补齐后才问模型，应只调用一次，实际 %d", chats.Load())
	}
	var sawToolStep bool
	for _, s := range res.Steps {
		if s.Type == "tool" && s.Name == "fs_read" {
			sawToolStep = true
		}
	}
	if !sawToolStep {
		t.Fatal("补执行的结果应当出现在执行轨迹里（前端要能看到）")
	}
}
