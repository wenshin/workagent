package session

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/workagent/auto-agent/internal/agent"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 历史回放要按真实顺序还原：思考 → 工具 → 思考 → 工具 → 正文。
// 只存「思考一段 + 工具列表」的话，排障时看不出结论是在哪一步之后得出的。
func TestMarshalPartsKeepsTimelineOrder(t *testing.T) {
	res := agent.Result{
		Content: "结论",
		Steps: []agent.Step{
			{Type: "tool", ID: "c1", Name: "fs_list", Args: "{}", Status: "ok"},
			{Type: "tool", ID: "c2", Name: "fs_read", Args: `{"path":"a.md"}`, Status: "ok"},
		},
		Timeline: []agent.Segment{
			{Type: "reasoning", Text: "先看目录"},
			{Type: "tool", ToolID: "c1"},
			{Type: "reasoning", Text: "再读文件"},
			{Type: "tool", ToolID: "c2"},
			{Type: "text", Text: "结论"},
		},
	}

	parts := marshalParts(discardLogger(), "s1", res)
	var got []map[string]any
	if err := json.Unmarshal(parts, &got); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	want := []string{"reasoning", "tool", "reasoning", "tool", "text"}
	if len(got) != len(want) {
		t.Fatalf("时间线段数应为 %d，实际 %d: %s", len(want), len(got), parts)
	}
	for i, typ := range want {
		if got[i]["type"] != typ {
			t.Fatalf("第 %d 段应为 %s，实际 %v", i, typ, got[i]["type"])
		}
	}
	// 工具段仍是完整步骤：参数与回执不能只剩一个 id
	if got[1]["name"] != "fs_list" || got[1]["id"] != "c1" {
		t.Fatalf("工具段应带上完整步骤: %+v", got[1])
	}
}

// 没有工具调用时正文位置是固定的，不写时间线，避免把整段正文复制进 parts。
func TestMarshalPartsSkipsTimelineWithoutTools(t *testing.T) {
	res := agent.Result{
		Content:  "直接回答",
		Timeline: []agent.Segment{{Type: "reasoning", Text: "想一想"}, {Type: "text", Text: "直接回答"}},
	}
	if parts := string(marshalParts(discardLogger(), "s1", res)); parts != "[]" {
		t.Fatalf("没有工具调用时应落空数组，实际 %s", parts)
	}
}

// 时间线缺失（第三方实现）时工具轨迹不能丢，否则历史里只剩正文。
func TestMarshalPartsKeepsUnplacedTools(t *testing.T) {
	res := agent.Result{
		Steps: []agent.Step{{Type: "tool", ID: "c1", Name: "fs_list", Status: "ok"}},
	}
	var got []map[string]any
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", res), &got); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if len(got) != 1 || got[0]["id"] != "c1" {
		t.Fatalf("应保留未进时间线的工具段，实际 %+v", got)
	}
}
