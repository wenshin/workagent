package builtin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// scriptedProvider 按轮次吐增量与工具调用，用来验证时间线的交错顺序。
type scriptedProvider struct {
	mu    sync.Mutex
	round int
}

func (p *scriptedProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "scripted", Kind: "openai", Model: "scripted"}
}

func (p *scriptedProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func (p *scriptedProvider) Chat(_ context.Context, _ llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	round := p.round
	p.round++
	p.mu.Unlock()
	switch round {
	case 0:
		_ = onDelta(llm.Delta{Reasoning: "先看看文件"})
		return llm.Result{ToolCalls: []llm.ToolCall{{
			ID:       "c1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.md"}`},
		}}}, nil
	case 1:
		_ = onDelta(llm.Delta{Reasoning: "拿到了"})
		_ = onDelta(llm.Delta{Content: "结论是 A"})
		return llm.Result{}, nil
	}
	return llm.Result{}, nil
}

// 时间线要保住真实顺序：思考 → 工具 → 思考 → 正文。
// 只把思考拼成一整段、工具另存一列，历史回放就会变成「思考全在顶、工具全在中」。
func TestRunRecordsTimelineOrder(t *testing.T) {
	readTool := tool.Tool{
		Name: "fs_read",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
			return "a.md 正文", nil
		},
	}
	router := llm.NewRouter(nil)
	router.Register(&scriptedProvider{})
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "scripted"})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(agent.Deps{
		Config:  &config.Config{LLMMaxRounds: 4, LLMMaxToolCalls: 4},
		LLM:     router,
		Invoker: tool.NewInvoker(nil, resumeRecorder{}, log),
		Log:     log,
	})
	if err != nil {
		t.Fatalf("构造 agent 失败: %v", err)
	}

	res, err := a.Run(context.Background(), agent.RunInput{
		Tools: []tool.Tool{readTool},
		Query: "看看 a.md",
		Env:   tool.Env{UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r1"},
	}, func(agent.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	got := make([]string, 0, len(res.Timeline))
	for _, seg := range res.Timeline {
		switch seg.Type {
		case "tool":
			got = append(got, "tool:"+seg.ToolID)
		default:
			got = append(got, seg.Type+":"+seg.Text)
		}
	}
	want := []string{"reasoning:先看看文件", "tool:c1", "reasoning:拿到了", "text:结论是 A"}
	if len(got) != len(want) {
		t.Fatalf("时间线应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 段应为 %q，实际 %q（完整时间线 %v）", i, want[i], got[i], got)
		}
	}
}
