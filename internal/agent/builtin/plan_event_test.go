package builtin

import (
	"context"
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

// planProvider 两轮都调 plan_write：第一轮建清单，第二轮把第一步标成完成。
// 用来验证「清单变了就推一条 plan.updated」而不是每轮都推同一份。
type planProvider struct {
	mu    sync.Mutex
	round int
}

func (p *planProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "plan", Kind: "openai", Model: "plan"}
}

func (p *planProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func (p *planProvider) Chat(_ context.Context, _ llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	round := p.round
	p.round++
	p.mu.Unlock()
	switch round {
	case 0:
		return llm.Result{ToolCalls: []llm.ToolCall{{
			ID:   "p1",
			Type: "function",
			Function: llm.FunctionCall{Name: planWriteToolName, Arguments: `{"goal":"整理合同","items":[` +
				`{"title":"读合同","status":"doing"},{"title":"写摘要","status":"pending"}]}`},
		}}}, nil
	case 1:
		return llm.Result{ToolCalls: []llm.ToolCall{{
			ID:   "p2",
			Type: "function",
			Function: llm.FunctionCall{Name: planWriteToolName, Arguments: `{"goal":"整理合同","items":[` +
				`{"title":"读合同","status":"done"},{"title":"写摘要","status":"doing"}]}`},
		}}}, nil
	}
	_ = onDelta(llm.Delta{Content: "已按清单完成"})
	return llm.Result{}, nil
}

// 清单变化要单独成事件：前端据此画进度卡片，模型也才能在中途看到「做到哪了」。
// 只改内部状态不推事件的话，界面上的清单会一直停在第一次写入的样子。
func TestRunEmitsPlanUpdated(t *testing.T) {
	router := llm.NewRouter(nil)
	router.Register(&planProvider{})
	router.SetRoute(llm.RouteChat, llm.Route{Primary: "plan"})

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

	var planEvents []agent.Plan
	res, err := a.Run(context.Background(), agent.RunInput{
		Tools: nil,
		Query: "整理这份合同",
		Env:   tool.Env{UserID: "u1", SpaceID: "s1", SpacePerm: "manage", RunID: "r1"},
	}, func(ev agent.Event) error {
		if ev.Type == agent.EventPlanUpdated && ev.Plan != nil {
			planEvents = append(planEvents, *ev.Plan)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if len(planEvents) != 2 {
		t.Fatalf("清单改两次就该推两条 plan.updated，实际 %d 条", len(planEvents))
	}
	last := planEvents[len(planEvents)-1]
	if len(last.Items) != 2 || last.Items[0].Status != agent.PlanDone {
		t.Fatalf("最后一条事件应带最新状态，实际 %+v", last.Items)
	}
	// 结果里也要带最终清单：会话层靠它把清单写进 message.parts
	if res.Plan.Empty() || len(res.Plan.Items) != 2 {
		t.Fatalf("Result.Plan 应是最终清单，实际 %+v", res.Plan)
	}
	if res.Plan.Goal != "整理合同" {
		t.Errorf("目标丢了: %q", res.Plan.Goal)
	}
}

// planProvider 的每轮工具回执都会进上下文：回执里再贴一份完整清单，
// 就等于同一份清单在上下文里存两遍（压缩后还只剩半截）。回执只给摘要。
func TestPlanWriteReplyIsSummary(t *testing.T) {
	state := newPlanState(nil)
	out, err := writePlan(state, []byte(`{"goal":"整理合同","items":[` +
		`{"title":"读合同","status":"done"},{"title":"写摘要","status":"doing"}]}`))
	if err != nil {
		t.Fatalf("writePlan 失败: %v", err)
	}
	if !strings.Contains(out, "1/2 完成") {
		t.Errorf("回执应带进度，实际 %q", out)
	}
	if !strings.Contains(out, "当前：写摘要") {
		t.Errorf("回执应指出正在做的那一步，实际 %q", out)
	}
	if !strings.Contains(out, "plan_read") {
		t.Errorf("回执应告诉模型去哪读完整清单，实际 %q", out)
	}
	// 不逐条回显：标题出现在回执里说明又在上下文里抄了一份
	if strings.Contains(out, "读合同") || strings.Contains(out, "写摘要（") {
		t.Errorf("回执不应回显整份清单，实际 %q", out)
	}
}
