package builtin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// truncatingProvider 首轮返回一个被 max_tokens 截断的工具调用（半截 JSON），
// 次轮把收到的消息记录下来，用来断言模型确实拿到了「怎么拆块」的指令。
type truncatingProvider struct {
	mu       sync.Mutex
	round    int
	second   []llm.Message
	finish   string
	argsJSON string
}

func (p *truncatingProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "truncating", Kind: "openai", Model: "truncating"}
}

func (p *truncatingProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func (p *truncatingProvider) Chat(_ context.Context, req llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	round := p.round
	p.round++
	if round == 1 {
		p.second = append([]llm.Message(nil), req.Messages...)
	}
	p.mu.Unlock()

	if round == 0 {
		return llm.Result{
			FinishReason: p.finish,
			ToolCalls: []llm.ToolCall{{
				ID:       "c1",
				Type:     "function",
				Function: llm.FunctionCall{Name: "fs_write", Arguments: p.argsJSON},
			}},
		}, nil
	}
	final := "我改成 12KB 一块分几次写"
	if onDelta != nil {
		_ = onDelta(llm.Delta{Content: final})
	}
	return llm.Result{Content: final, Usage: llm.Usage{OutputTokens: 10}}, nil
}

// 被截断的调用一律不执行：参数只有半截，执行它等于把半个文件写下去。
// 同时必须把「怎么拆块」告诉模型，否则它会原样重发、再撞同一个上限。
func TestTruncatedToolCallIsNotExecuted(t *testing.T) {
	provider := &truncatingProvider{
		finish:   llm.FinishLength,
		argsJSON: `{"path":"big.md","content":"# 标题\n正文写到一半`,
	}
	var executed bool
	writeTool := tool.Tool{
		Name: "fs_write",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
			executed = true
			return "written", nil
		},
	}

	router := llm.NewRouter(nil)
	router.Register(provider)
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "truncating"})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(agent.Deps{
		Config:  &config.Config{LLMMaxRounds: 4, LLMMaxToolCalls: 4, LLMMaxTokens: 4096},
		LLM:     router,
		Invoker: tool.NewInvoker(nil, resumeRecorder{}, log),
		Log:     log,
	})
	if err != nil {
		t.Fatalf("构造 agent 失败: %v", err)
	}

	res, err := a.Run(context.Background(), agent.RunInput{
		Tools: []tool.Tool{writeTool},
		Query: "把文档写到 big.md",
		Env:   tool.Env{UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r1"},
	}, func(agent.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if executed {
		t.Fatal("被截断的调用不能执行")
	}
	if !strings.Contains(res.Content, "分几次写") {
		t.Fatalf("应拿到模型的第二轮回答，实际 %q", res.Content)
	}

	var hint string
	for _, m := range provider.second {
		if m.Role == llm.RoleTool && m.ToolCallID == "c1" {
			hint = m.Content
		}
	}
	if hint == "" {
		t.Fatal("截断的调用要回填一条 tool 消息，模型才知道发生了什么")
	}
	for _, want := range []string{"截断", "12KB", "append"} {
		if !strings.Contains(hint, want) {
			t.Errorf("回填说明缺少 %q：%s", want, hint)
		}
	}
}

// 没有被截断时报文也不能被误拦：参数完整就该照常执行。
func TestCompleteToolCallStillRuns(t *testing.T) {
	provider := &truncatingProvider{
		finish:   "tool_calls",
		argsJSON: `{"path":"a.md","content":"hi"}`,
	}
	var executed bool
	writeTool := tool.Tool{
		Name: "fs_write",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
			executed = true
			return "written", nil
		},
	}

	router := llm.NewRouter(nil)
	router.Register(provider)
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "truncating"})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(agent.Deps{
		Config:  &config.Config{LLMMaxRounds: 4, LLMMaxToolCalls: 4, LLMMaxTokens: 4096},
		LLM:     router,
		Invoker: tool.NewInvoker(nil, resumeRecorder{}, log),
		Log:     log,
	})
	if err != nil {
		t.Fatalf("构造 agent 失败: %v", err)
	}

	if _, err := a.Run(context.Background(), agent.RunInput{
		Tools: []tool.Tool{writeTool},
		Query: "写 a.md",
		Env:   tool.Env{UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r1"},
	}, func(agent.Event) error { return nil }); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !executed {
		t.Fatal("参数完整的调用应正常执行")
	}
}
