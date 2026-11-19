package session

import (
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/cache"
)

// 帧列表被条数上限裁掉开头之后，缺口必须用快照补齐：
// 否则客户端从 after_offset=0 重连时只能看到后半段思考。
func TestRecoverPrefixFillsReasoningGap(t *testing.T) {
	st := cache.RunState{Reasoning: "思考很长的前半段", Content: "正文", Offset: 11}
	// 首帧 offset=8、长度 1，说明它从第 7 个字符开始：0..7 是缺口
	frames := []RunFrame{{Offset: 8, Event: delta("段")}}
	gap := frameGap(frames, st.Offset)
	if gap != 7 {
		t.Fatalf("缺口边界 = %d，期望 7", gap)
	}

	out := recoverPrefix(st, 0, gap)
	if len(out) != 1 {
		t.Fatalf("缺口全在思考里，应当只补一帧，实际 %d 帧", len(out))
	}
	if !out[0].Event.Reasoning {
		t.Fatal("补的应当是思考帧")
	}
	if want := "思考很长的前半"; out[0].Event.Delta != want {
		t.Fatalf("补齐内容 = %q，期望 %q", out[0].Event.Delta, want)
	}
	if out[0].Offset != 7 {
		t.Fatalf("补齐帧 offset = %d，期望 7（与首帧 8 相接）", out[0].Offset)
	}
}

// 缺口跨过思考末尾时，要按 offset 切成「思考尾段 + 正文前缀」两帧。
func TestRecoverPrefixSplitsReasoningAndContent(t *testing.T) {
	st := cache.RunState{Reasoning: "abc", Content: "defgh", Offset: 8}
	out := recoverPrefix(st, 0, 8)
	if len(out) != 2 {
		t.Fatalf("缺口跨段应当补两帧，实际 %d 帧", len(out))
	}
	if !out[0].Event.Reasoning || out[0].Event.Delta != "abc" || out[0].Offset != 3 {
		t.Fatalf("第一帧应当是完整思考，实际 %+v", out[0])
	}
	if out[1].Event.Reasoning || out[1].Event.Delta != "defgh" || out[1].Offset != 8 {
		t.Fatalf("第二帧应当是正文，实际 %+v", out[1])
	}
}

// 客户端已经拿到缺口之后的内容：不要重复补。
func TestRecoverPrefixSkipsCoveredRange(t *testing.T) {
	st := cache.RunState{Reasoning: "abcdef", Offset: 6}
	if out := recoverPrefix(st, 6, 6); len(out) != 0 {
		t.Fatalf("after 已到缺口边界，不应补帧，实际 %+v", out)
	}
	if out := recoverPrefix(st, 4, 0); len(out) != 0 {
		t.Fatalf("没有缺口时不应补帧，实际 %+v", out)
	}
}

// 快照比帧旧（秒级写入）：缺口上界只能取快照已有的字符数，不能越界。
func TestRecoverPrefixCapsAtSnapshotLength(t *testing.T) {
	st := cache.RunState{Reasoning: "ab", Offset: 2}
	out := recoverPrefix(st, 0, 10)
	if len(out) != 1 || out[0].Event.Delta != "ab" || out[0].Offset != 2 {
		t.Fatalf("应当只补快照里有的内容，实际 %+v", out)
	}
}

// 缓存里一帧都没有（已结束或被整段裁掉）时，整轮内容都按快照补。
func TestFrameGapFallsBackToSnapshotOffset(t *testing.T) {
	if gap := frameGap(nil, 42); gap != 42 {
		t.Fatalf("无帧时缺口边界应取快照 offset，实际 %d", gap)
	}
	// 非增量帧（工具 / 审批）不改变 offset，直接以它为界
	frames := []RunFrame{{Offset: 42, Event: agent.Event{Type: agent.EventToolCall}}}
	if gap := frameGap(frames, 42); gap != 42 {
		t.Fatalf("非增量帧的缺口边界 = %d，期望 42", gap)
	}
}
