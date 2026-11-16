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

// fakeProvider 是脚本化模型：前 toolRounds 次返回一次工具调用，之后给结论。
type fakeProvider struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
	toolLeft int
	// inputTokens 每轮回报的输入规模：用它模拟「上下文已经很长」
	inputTokens int
}

func (p *fakeProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "fake", Kind: "openai", Model: "fake-model"}
}

func (p *fakeProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, llm.ErrUnsupportedProvider
}

func (p *fakeProvider) Chat(_ context.Context, req llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	left := p.toolLeft
	if left > 0 {
		p.toolLeft--
	}
	p.mu.Unlock()

	res := llm.Result{Model: "fake-model", Usage: llm.Usage{InputTokens: p.inputTokens, OutputTokens: 10}}
	if left > 0 {
		// 每轮参数都不同：重复调用会被进度守卫拦掉，这里要的是「一直有活干」
		res.ToolCalls = []llm.ToolCall{{
			ID:       "call-" + string(rune('0'+left)),
			Type:     "function",
			Function: llm.FunctionCall{Name: "probe", Arguments: `{"q":"` + string(rune('a'+left)) + `"}`},
		}}
		return res, nil
	}
	res.Content = "结论"
	if onDelta != nil {
		_ = onDelta(llm.Delta{Content: "结论"})
	}
	return res, nil
}

type fakeRouter = llm.Router

func newFakeRouter(p llm.Provider) *fakeRouter {
	r := llm.NewRouter(nil)
	r.Register(p)
	r.SetRoute(llm.RouteChat, llm.Route{Primary: "fake", Model: "fake-model"})
	return r
}

// 一轮跑久了，上下文必须自己收口：超过窗口比例后，较早的工具往返被折成进展记录，
// 最近几轮仍保留原始回执。
func TestRunCompactsContextMidway(t *testing.T) {
	p := &fakeProvider{toolLeft: 6, inputTokens: 1000} // 窗口 100 → 预算 60，必然触发
	a := &Agent{
		cfg: &config.Config{
			LLMMaxRounds:     12,
			LLMMaxToolCalls:  100,
			LLMMaxParallel:   2,
			AgentStallRounds: 50, // 本用例只关心上下文，不让停滞守卫提前收尾
			LLMContextTokens: 100,
		},
		llm: newFakeRouter(p),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	in := agent.RunInput{
		Tools: []tool.Tool{{
			Name:        "probe",
			Capability:  "test.probe",
			Description: "探针",
			Risk:        1,
			Invoke:      func(context.Context, json.RawMessage, tool.Env) (string, error) { return "ok", nil },
		}},
	}
	in.Query = "跑个长任务"

	res, err := a.Run(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !strings.Contains(res.Content, "结论") {
		t.Fatalf("没有拿到结论: %q", res.Content)
	}
	if len(p.requests) < 5 {
		t.Fatalf("应至少跑 5 轮，实际 %d", len(p.requests))
	}
	// 第一条请求里还没有可折叠的内容
	if hasDigest(p.requests[0].Messages) {
		t.Fatal("第一轮不该出现折叠记录")
	}
	// 后面的请求必须出现折叠记录，而且工具回执数量受控
	last := p.requests[len(p.requests)-1].Messages
	if !hasDigest(last) {
		t.Fatal("长任务后来的请求里应出现折叠记录")
	}
	if n := countDigest(last); n != 1 {
		t.Fatalf("折叠记录应只有一条，实际 %d 条", n)
	}
	if n := countRole(last, llm.RoleTool); n > compactKeepRounds+1 {
		t.Fatalf("保留的原始回执过多: %d", n)
	}
	// 折叠记录本身要说明「折了几轮」
	for _, m := range last {
		if m.Role == llm.RoleSystem && strings.Contains(m.Content, "早期执行步骤") {
			if !strings.Contains(m.Content, "已折叠的") {
				t.Fatalf("折叠记录缺少统计: %s", m.Content)
			}
		}
	}
}

func hasDigest(msgs []llm.Message) bool { return countDigest(msgs) > 0 }

func countDigest(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleSystem && strings.HasPrefix(m.Content, digestHeader) {
			n++
		}
	}
	return n
}

func countRole(msgs []llm.Message, role string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}
