package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/llm"
)

// 参数上限要跟着单次输出预算走：写死常数会跟 LLM_MAX_TOKENS 脱节。
func TestDefaultMaxArgBytesFollowsOutputBudget(t *testing.T) {
	cases := []struct {
		maxTokens int
		want      int
		why       string
	}{
		{0, 16 << 10, "配置缺失时用 16KB 兜底"},
		{4096, 16 << 10, "默认预算与 16KB 对齐"},
		{8192, 32 << 10, "调高预算，上限跟着放大"},
		{1024, 16 << 10, "预算很小时不低于 16KB"},
	}
	for _, c := range cases {
		if got := DefaultMaxArgBytes(c.maxTokens); got != c.want {
			t.Errorf("DefaultMaxArgBytes(%d) = %d，期望 %d（%s）", c.maxTokens, got, c.want, c.why)
		}
	}
}

// 超限的调用不执行，且回填的说明要能直接照着做（拆块 + append 续写），
// 只说「参数过大」模型会原样重发一遍再撞同一个上限。
func TestInvokerRejectsOversizeArgs(t *testing.T) {
	var executed bool
	tl := Tool{
		Name: "fs_write",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, Env) (string, error) {
			executed = true
			return "written", nil
		},
	}
	args := `{"path":"a.md","content":"` + strings.Repeat("x", 16<<10) + `"}`

	inv := NewInvoker(nil, noopRecorder{}, nil)
	got := inv.Invoke(context.Background(), tl, llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: tl.Name, Arguments: args},
	}, replayEnv())

	if got.Status != StatusError {
		t.Fatalf("超限应返回错误，实际 %+v", got)
	}
	if executed {
		t.Fatal("超限的调用不能执行")
	}
	for _, want := range []string{"单次上限", "append", "mode=overwrite"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("回填说明缺少 %q：%s", want, got.Content)
		}
	}
}

// 装配层按 LLM_MAX_TOKENS 放宽后，同样大的参数应当能通过校验真正执行。
func TestInvokerArgLimitIsConfigurable(t *testing.T) {
	var executed bool
	tl := Tool{
		Name: "fs_write",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, Env) (string, error) {
			executed = true
			return "written", nil
		},
	}
	args := `{"path":"a.md","content":"` + strings.Repeat("x", 20<<10) + `"}`

	inv := NewInvoker(nil, noopRecorder{}, nil)
	inv.SetMaxArgBytes(DefaultMaxArgBytes(8192))
	got := inv.Invoke(context.Background(), tl, llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: tl.Name, Arguments: args},
	}, replayEnv())

	if got.Status != StatusOK || !executed {
		t.Fatalf("32KB 上限下 20KB 参数应执行，实际 status=%s executed=%v", got.Status, executed)
	}
}
