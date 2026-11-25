package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
)

type noopRecorder struct{}

func (noopRecorder) RecordToolCall(context.Context, store.ToolCallRecord) error { return nil }
func (noopRecorder) AppendAuditLog(context.Context, store.AuditEntry) error     { return nil }

func replayFixture(t *testing.T, args string) (Tool, llm.ToolCall, *bool) {
	t.Helper()
	executed := false
	tl := Tool{
		Name: "fs_write",
		Risk: 1,
		Invoke: func(context.Context, json.RawMessage, Env) (string, error) {
			executed = true
			return "written", nil
		},
	}
	call := llm.ToolCall{
		ID:       "call-1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "fs_write", Arguments: args},
	}
	return tl, call, &executed
}

func replayEnv() Env {
	return Env{
		UserID:    "u1",
		SpaceID:   "s1",
		SpacePerm: "manage",
		RunID:     "r2",
		SessionID: "sess1",
	}
}

// 续跑回放：同样的工具与参数直接返回上次的结果，不再执行一次副作用。
func TestInvokeReplaysCachedResult(t *testing.T) {
	args := `{"path":"a.md","content":"hi"}`
	tl, call, executed := replayFixture(t, args)

	env := replayEnv()
	env.Replay = map[string]InvokeResult{
		ArgsHash(tl.Name, json.RawMessage(args)): {
			ToolName: tl.Name,
			Status:   StatusOK,
			Content:  "written",
		},
	}

	inv := NewInvoker(nil, noopRecorder{}, nil)
	got := inv.Invoke(context.Background(), tl, call, env)

	if *executed {
		t.Fatal("命中回放时不应再次执行工具（副作用不能重来）")
	}
	if got.Status != StatusOK || got.Content != "written" {
		t.Fatalf("应回放上次的结果，实际 %+v", got)
	}
	if !got.Cached {
		t.Fatal("回放的结果要标记 Cached，审计才能区分「做过」与「又做了一次」")
	}
}

// 参数不同就不算同一次调用：必须真的执行。
func TestInvokeDoesNotReplayDifferentArgs(t *testing.T) {
	args := `{"path":"b.md","content":"yo"}`
	tl, call, executed := replayFixture(t, args)

	env := replayEnv()
	env.Replay = map[string]InvokeResult{
		ArgsHash(tl.Name, json.RawMessage(`{"path":"a.md","content":"hi"}`)): {
			ToolName: tl.Name,
			Status:   StatusOK,
			Content:  "written",
		},
	}

	inv := NewInvoker(nil, noopRecorder{}, nil)
	got := inv.Invoke(context.Background(), tl, call, env)

	if !*executed {
		t.Fatal("参数不同时必须真正执行")
	}
	if got.Cached {
		t.Fatal("真正执行的结果不应标 Cached")
	}
}

// 参数顺序与空白不影响指纹：模型两次生成的 JSON 常在这两处不一样。
func TestArgsHashNormalizes(t *testing.T) {
	a := ArgsHash("fs_write", json.RawMessage(`{"a":1,"b":2}`))
	b := ArgsHash("fs_write", json.RawMessage(`{ "b": 2, "a": 1 }`))
	if a != b {
		t.Fatalf("规范化后应一致：%s vs %s", a, b)
	}
	if ArgsHash("fs_write", json.RawMessage(`{"a":1}`)) == a {
		t.Fatal("参数不同指纹应不同")
	}
}

// 返回值取决于磁盘内容的工具不参与回放：plugin_try 的 key 只有 slug + input，
// 但草稿改了。回放会把上一次的回执原样奉还（连耗时都一样），
// 作者以为「改了没生效」，实际是根本没重跑。
func TestInvokeSkipsReplayForNoReplayTools(t *testing.T) {
	args := `{"slug":"demo","input":"{}"}`
	tl, call, executed := replayFixture(t, args)
	tl.Name = "plugin_try"
	tl.NoReplay = true
	call.Function.Name = "plugin_try"

	env := replayEnv()
	env.Replay = map[string]InvokeResult{
		ArgsHash("plugin_try", json.RawMessage(args)): {
			ToolName: "plugin_try",
			Status:   StatusOK,
			Content:  "上一次的结果",
		},
	}

	got := NewInvoker(nil, noopRecorder{}, nil).Invoke(context.Background(), tl, call, env)

	if !*executed {
		t.Fatal("标记 NoReplay 的工具必须真的执行")
	}
	if got.Cached || got.Content != "written" {
		t.Fatalf("不应命中回放，实际 %+v", got)
	}
}
