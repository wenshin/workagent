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

// 清单要落进 parts：不落的话刷新后界面上那张进度卡就没了，
// 翻历史只剩一堆工具卡，看不出这一轮原本打算做几步。
// 位置固定在最前：先看计划再看过程，才读得懂中间为什么这么走。
func TestMarshalPartsLeadsWithPlan(t *testing.T) {
	res := agent.Result{
		Content: "做完了",
		Plan: &agent.Plan{Goal: "整理合同", Items: []agent.PlanItem{
			{Title: "读合同", Status: agent.PlanDone},
			{Title: "写摘要", Status: agent.PlanDoing},
		}},
		Steps:    []agent.Step{{Type: "tool", ID: "c1", Name: "fs_read", Status: "ok"}},
		Timeline: []agent.Segment{{Type: "tool", ToolID: "c1"}, {Type: "text", Text: "做完了"}},
	}
	var got []map[string]any
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", res), &got); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if len(got) < 2 || got[0]["type"] != "plan" {
		t.Fatalf("清单应排在最前，实际 %+v", got)
	}
	if got[0]["goal"] != "整理合同" {
		t.Errorf("目标丢了: %+v", got[0])
	}
	items, _ := got[0]["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("清单条数不对: %+v", got[0]["items"])
	}

	// 只有清单、没有工具时也要落：否则占位消息会被当成空占位藏掉
	onlyPlan := agent.Result{Content: "我先列个计划", Plan: res.Plan}
	var planOnly []map[string]any
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", onlyPlan), &planOnly); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if len(planOnly) != 1 || planOnly[0]["type"] != "plan" {
		t.Fatalf("只有清单时应落一条清单段，实际 %+v", planOnly)
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
