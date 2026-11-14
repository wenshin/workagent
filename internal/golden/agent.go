package golden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
	agentbuiltin "github.com/workagent/auto-agent/internal/agent/builtin"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/tool"
)

// Options 是跑 agent 类用例时的 harness 配置。
// 用例本身只描述「输入与期望」，预算这类平台参数由跑用例的一方决定，
// 否则每条用例都要重复写一遍 max_rounds / max_tool_calls。
type Options struct {
	// MaxRounds 是轮次上限，默认 8
	MaxRounds int
	// MaxToolCalls 是工具调用总次数上限，默认 20
	MaxToolCalls int
	// StallRounds 是连续多少轮无新信息判定停滞，默认 2
	StallRounds int
	// MaxDepth 是派发深度上限，默认 2
	MaxDepth int
	// Dispatch 控制是否暴露 agent_dispatch；nil 表示默认开启
	Dispatch *bool
	// WallClock 是单次运行的墙钟预算，默认 30s
	WallClock time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxRounds <= 0 {
		o.MaxRounds = 8
	}
	if o.MaxToolCalls <= 0 {
		o.MaxToolCalls = 20
	}
	if o.StallRounds <= 0 {
		o.StallRounds = 2
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 2
	}
	if o.WallClock <= 0 {
		o.WallClock = 30 * time.Second
	}
	return o
}

// dispatchEnabled 解析三态开关：nil 视为开启。
func dispatchEnabled(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// CaseResult 是一条用例的执行结论。
type CaseResult struct {
	Name     string
	Passed   bool
	Failures []string
	// Detail 是观察到的事实（停止原因、轮次、工具序列），失败时粘贴进报告
	Detail string
}

// RunSuite 跑一个用例集里所有 agent 类用例。
func RunSuite(s Suite, opts Options) []CaseResult {
	cases := s.CasesByKind(KindAgent)
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		out = append(out, RunAgentCase(c, opts))
	}
	return out
}

// RunAgentCase 用脚本化模型跑一条 agent 用例，返回是否通过及失败原因。
func RunAgentCase(c Case, opts Options) CaseResult {
	opts = opts.withDefaults()
	if l := c.Limits; l != nil {
		if l.MaxRounds > 0 {
			opts.MaxRounds = l.MaxRounds
		}
		if l.MaxToolCalls > 0 {
			opts.MaxToolCalls = l.MaxToolCalls
		}
		if l.StallRounds > 0 {
			opts.StallRounds = l.StallRounds
		}
		if l.MaxDepth > 0 {
			opts.MaxDepth = l.MaxDepth
		}
		if l.Dispatch != nil {
			opts.Dispatch = l.Dispatch
		}
	}
	res := CaseResult{Name: c.Name}

	provider := &scriptProvider{turns: c.Script}
	router := llm.NewRouter(newDiscardLogger())
	router.Register(provider)
	router.SetRoute(llm.RouteChat, llm.Route{Primary: provider.Info().Name})

	cfg := &config.Config{
		LLMMaxRounds:      opts.MaxRounds,
		LLMMaxToolCalls:   opts.MaxToolCalls,
		LLMMaxParallel:    4,
		LLMMaxTokens:      4096,
		LLMMaxWallClock:   opts.WallClock,
		LLMMaxTotalTokens: 0,
		AgentStallRounds:  opts.StallRounds,
		AgentMaxDepth:     opts.MaxDepth,
		AgentDispatch:     dispatchEnabled(opts.Dispatch),
	}
	impl, err := agentbuiltin.New(agent.Deps{
		Config:  cfg,
		LLM:     router,
		Invoker: tool.NewInvoker(nil, nil, newDiscardLogger()),
		Log:     newDiscardLogger(),
	})
	if err != nil {
		return failed(res, fmt.Sprintf("构造 harness 失败：%v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.WallClock+5*time.Second)
	defer cancel()

	runRes, runErr := impl.Run(ctx, agent.RunInput{
		Env: tool.Env{
			// 用例跑在「有 manage 权限的空间」里：要测的是循环行为，不是授权
			UserID: "golden", SpaceID: "golden", SessionID: "golden", RunID: "golden",
			SpacePerm: "manage",
		},
		Query:         c.Query,
		Tools:         buildTools(c.Tools),
		AllowDispatch: dispatchEnabled(opts.Dispatch),
		// 发布门禁会把插件正文注入进来，用例因此能验证「按这份说明能不能做对」
		ActiveSkill: c.InjectSkill,
		AgentPrompt: c.InjectPrompt,
	}, nil)

	observed := observed{
		stop:     stopReason(runErr, runRes.StopReason),
		rounds:   provider.rounds(),
		steps:    runRes.Steps,
		content:  runRes.Content,
		blocked:  agentbuiltin.CountBlocked(runRes.Steps),
		executed: executedTools(runRes.Steps),
		runErr:   runErr,
	}
	res.Detail = observed.describe()

	if runErr != nil && !errors.Is(runErr, agent.ErrStalled) && !errors.Is(runErr, agent.ErrBudgetExceeded) {
		return failed(res, fmt.Sprintf("运行失败：%v", runErr))
	}
	res.Failures = append(res.Failures, checkAgentExpectation(c.Expect, observed)...)
	res.Passed = len(res.Failures) == 0
	return res
}

// observed 是一条用例跑完后的可观测事实。
type observed struct {
	stop     string
	rounds   int
	steps    []agent.Step
	content  string
	blocked  int
	executed []string
	runErr   error
}

func (o observed) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "停止原因=%s 模型调用=%d 工具执行=%d 拦截=%d 序列=%v",
		o.stop, o.rounds, len(o.executed), o.blocked, o.executed)
	for _, s := range o.steps {
		fmt.Fprintf(&b, "\n    步骤 %s status=%s result=%q", s.Name, s.Status, truncateRunes(s.Result, 120))
	}
	if o.runErr != nil {
		fmt.Fprintf(&b, " 错误=%v", o.runErr)
	}
	if len(o.content) > 0 {
		fmt.Fprintf(&b, " 结论=%q", truncateRunes(o.content, 80))
	}
	return b.String()
}

// checkAgentExpectation 逐条比对断言，返回失败描述。
func checkAgentExpectation(e Expect, o observed) []string {
	var failures []string
	if e.StopReason != "" && e.StopReason != o.stop {
		failures = append(failures, fmt.Sprintf("停止原因应为 %q，实际 %q", e.StopReason, o.stop))
	}
	for _, want := range e.ContentContains {
		if !strings.Contains(o.content, want) {
			failures = append(failures, fmt.Sprintf("结论缺少 %q", want))
		}
	}
	for _, banned := range e.ContentNotContains {
		if strings.Contains(o.content, banned) {
			failures = append(failures, fmt.Sprintf("结论不应包含 %q", banned))
		}
	}
	if len(e.ToolSequence) > 0 && !equalStrings(e.ToolSequence, o.executed) {
		failures = append(failures, fmt.Sprintf("工具序列应为 %v，实际 %v", e.ToolSequence, o.executed))
	}
	if e.MaxRounds > 0 && o.rounds > e.MaxRounds {
		failures = append(failures, fmt.Sprintf("模型调用次数 %d 超过上限 %d", o.rounds, e.MaxRounds))
	}
	if e.MaxToolCalls > 0 && len(o.executed) > e.MaxToolCalls {
		failures = append(failures, fmt.Sprintf("工具执行 %d 次超过上限 %d", len(o.executed), e.MaxToolCalls))
	}
	if o.blocked < e.BlockedAtLeast {
		failures = append(failures, fmt.Sprintf("应至少拦截 %d 次重复调用，实际 %d", e.BlockedAtLeast, o.blocked))
	}
	return failures
}

// stopReason 统一「错误即停止原因」的映射：
// 预算与停滞会带错误返回（保留已生成内容），正常作答返回 nil。
func stopReason(runErr error, resStop string) string {
	switch {
	case errors.Is(runErr, agent.ErrStalled):
		return "stalled"
	case errors.Is(runErr, agent.ErrBudgetExceeded):
		return "budget"
	case resStop != "":
		return resStop
	case runErr != nil:
		return "error"
	default:
		return "done"
	}
}

// executedTools 取出真正执行过的工具名（不含被守卫拦截的重复调用）。
func executedTools(steps []agent.Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.Type != "tool" {
			continue
		}
		if strings.HasPrefix(s.Result, agentbuiltin.DuplicateBlockMarker) {
			continue
		}
		out = append(out, s.Name)
	}
	return out
}

func buildTools(specs []ToolSpec) []tool.Tool {
	out := make([]tool.Tool, 0, len(specs))
	for _, s := range specs {
		spec := s
		risk := spec.Risk
		if risk <= 0 {
			risk = policy.RiskReadOnly
		}
		out = append(out, tool.Tool{
			Name:        spec.Name,
			Capability:  spec.Name,
			Description: "黄金用例工具桩：" + spec.Name,
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
			Risk:        risk,
			// 用例声明的工具由用例自己"绑定"，测的是循环而不是授权
			HasBinding:  true,
			SideEffects: policy.SideEffectsReadOnly,
			Runtime:     tool.RuntimeBuiltin,
			Invoke: func(context.Context, json.RawMessage, tool.Env) (string, error) {
				if spec.Error != "" {
					return "", errors.New(spec.Error)
				}
				return spec.Result, nil
			},
		})
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func failed(r CaseResult, reason string) CaseResult {
	r.Passed = false
	r.Failures = append(r.Failures, reason)
	return r
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptProvider 是按脚本依次返回结果的模型桩。
// 它记录每一次请求，用例因此可以断言「轮次」这个最容易失守的预算。
type scriptProvider struct {
	mu    sync.Mutex
	turns []Turn
	idx   int
	reqs  int
}

func (p *scriptProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{
		Name:         "script",
		Kind:         "openai",
		Model:        "script",
		Capabilities: []llm.Capability{llm.CapTool},
	}
}

func (p *scriptProvider) roundCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reqs
}

func (p *scriptProvider) rounds() int { return p.roundCount() }

func (p *scriptProvider) Chat(ctx context.Context, _ llm.ChatRequest, onDelta llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.reqs++
	if p.idx >= len(p.turns) {
		return llm.Result{}, fmt.Errorf("黄金用例脚本已用完：第 %d 次模型调用没有对应轮次", p.reqs)
	}
	turn := p.turns[p.idx]
	p.idx++

	if turn.Error != "" {
		return llm.Result{}, &llm.ProviderError{Provider: "script", Status: 500, Message: turn.Error}
	}
	if onDelta != nil && turn.Content != "" {
		if err := onDelta(llm.Delta{Content: turn.Content}); err != nil {
			return llm.Result{}, err
		}
	}

	out := llm.Result{Content: turn.Content, Model: "script"}
	for i, call := range turn.ToolCalls {
		args := strings.TrimSpace(string(call.Args))
		if args == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
			ID:   fmt.Sprintf("call_%d_%d", p.reqs, i),
			Type: "function",
			Function: llm.FunctionCall{
				Name:      call.Name,
				Arguments: args,
			},
		})
	}
	if turn.Usage != nil {
		out.Usage = llm.Usage{InputTokens: turn.Usage.InputTokens, OutputTokens: turn.Usage.OutputTokens}
	}
	return out, nil
}

func (p *scriptProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, llm.ErrUnsupportedProvider
}
