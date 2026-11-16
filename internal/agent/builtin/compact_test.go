package builtin

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
)

// 构造一轮「assistant 要工具 → 工具回执」。
func toolRound(id, name, args, result string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: "查一下 " + name, ToolCalls: []llm.ToolCall{
			{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}},
		}},
		{Role: llm.RoleTool, ToolCallID: id, Content: result},
	}
}

func TestToolGroupRanges(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	msgs = append(msgs, toolRound("c1", "file_read", `{"path":"a"}`, "A")...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "继续"})
	msgs = append(msgs, toolRound("c2", "file_read", `{"path":"b"}`, "B")...)

	got := toolGroupRanges(msgs)
	if len(got) != 2 {
		t.Fatalf("应识别 2 组工具往返，实际 %d", len(got))
	}
	if got[0].start != 1 || got[0].end != 3 {
		t.Fatalf("第一组区间不对: %+v", got[0])
	}
	if got[1].start != 4 || got[1].end != 6 {
		t.Fatalf("第二组区间不对: %+v", got[1])
	}
}

// 折叠后必须仍然「每个 tool_calls 都有对应回执」，否则请求会被模型直接拒绝。
func TestCompactMessagesKeepsPairing(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, toolRound("c"+string(rune('0'+i)), "file_read",
			`{"path":"/data/very-long-path-`+string(rune('0'+i))+`"}`, strings.Repeat("结果", 50))...)
	}

	out, ok := compactMessages(msgs, 3)
	if !ok {
		t.Fatal("6 轮保留 3 轮，应该发生折叠")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("折叠后消息数应减少：%d -> %d", len(msgs), len(out))
	}
	// 系统提示词还在开头
	if out[0].Role != llm.RoleSystem || out[0].Content != "sys" {
		t.Fatalf("系统提示词被破坏了: %+v", out[0])
	}
	// 折叠记录是一条 system 消息
	digest := ""
	for _, m := range out {
		if m.Role == llm.RoleSystem && strings.Contains(m.Content, "早期执行步骤") {
			digest = m.Content
		}
	}
	if digest == "" {
		t.Fatal("缺少折叠记录")
	}
	if !strings.Contains(digest, "file_read") || !strings.Contains(digest, "very-long-path") {
		t.Fatalf("折叠记录应保留工具名与参数线索: %s", digest)
	}

	// 配对完整性：每个带 tool_calls 的 assistant，其后必须紧跟同 id 的回执
	for i, m := range out {
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for _, call := range m.ToolCalls {
			found := false
			for j := i + 1; j < len(out) && out[j].Role == llm.RoleTool; j++ {
				if out[j].ToolCallID == call.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("第 %d 条 assistant 的调用 %s 丢了回执", i, call.ID)
			}
		}
	}

	// 保留的是最近的 3 轮：最后一条回执必须还在
	last := out[len(out)-1]
	if last.Role != llm.RoleTool || last.ToolCallID != "c5" {
		t.Fatalf("最近一轮应原样保留，实际 %+v", last)
	}
}

func TestCompactMessagesNoop(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	msgs = append(msgs, toolRound("c1", "file_read", "{}", "A")...)
	if out, ok := compactMessages(msgs, compactKeepRounds); ok || len(out) != len(msgs) {
		t.Fatal("轮数没超阈值时不该折叠")
	}
}

// 折叠记录不能自己又变成一大段：条数与单条长度都要有上限；
// 也不能每折一次就多一条，否则上下文越压越长。
func TestCompactDigestBoundedAndSingle(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < 200; i++ {
		msgs = append(msgs, toolRound("c"+string(rune('0'+i%10)), "file_read",
			strings.Repeat("p", 500), strings.Repeat("结果", 400))...)
	}

	// 连续折 20 次，模拟长任务里反复超预算
	for i := 0; i < 20; i++ {
		out, ok := compactMessages(msgs, compactKeepRounds)
		if !ok {
			break
		}
		msgs = out
	}

	digests := 0
	var digest string
	for _, m := range msgs {
		if m.Role == llm.RoleSystem && strings.HasPrefix(m.Content, digestHeader) {
			digests++
			digest = m.Content
		}
	}
	if digests != 1 {
		t.Fatalf("折叠记录应始终只有一条，实际 %d 条", digests)
	}
	if n := strings.Count(digest, "\n- "); n > compactMaxBlocks+1 {
		t.Fatalf("折叠记录条数超出上限: %d", n)
	}
	if len([]rune(digest)) > 12000 {
		t.Fatalf("折叠记录过长: %d 字", len([]rune(digest)))
	}
}

// 触发点按「窗口的 60%」算，而不是等模型报错。
func TestFitContextThreshold(t *testing.T) {
	a := &Agent{cfg: &config.Config{LLMContextTokens: 1000}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, toolRound("c"+string(rune('0'+i)), "file_read", "{}", "结果")...)
	}

	if out := a.fitContext(msgs, 500); len(out) != len(msgs) {
		t.Fatal("未达预算不该折叠")
	}
	if out := a.fitContext(msgs, 700); len(out) >= len(msgs) {
		t.Fatal("超过窗口 60% 应折叠")
	}

	// 0 = 关闭轮内压缩
	a.cfg.LLMContextTokens = 0
	if out := a.fitContext(msgs, 999999); len(out) != len(msgs) {
		t.Fatal("关闭后不该折叠")
	}
}

func TestIsContextOverflow(t *testing.T) {
	cases := map[string]bool{
		"provider: 400 This model's maximum context length is 65536 tokens": true,
		"provider: 400 context_length_exceeded":                             true,
		"prompt is too long: 200000 tokens > 128000 maximum":                true,
		"provider: 400 invalid api key":                                     false,
		"context canceled":                                                  false,
	}
	for msg, want := range cases {
		if got := isContextOverflow(errors.New(msg)); got != want {
			t.Fatalf("isContextOverflow(%q) = %v，期望 %v", msg, got, want)
		}
	}
	if isContextOverflow(nil) {
		t.Fatal("nil 不该判成溢出")
	}
}
