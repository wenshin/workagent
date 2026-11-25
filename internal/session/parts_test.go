package session

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
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

// 思考耗时随 parts 落库：前端折叠标题读的就是 reasoning 段的 duration_ms，
// 字段名或省略规则一变，界面上那段耗时就不显示了。
func TestMarshalPartsKeepsReasoningDuration(t *testing.T) {
	res := agent.Result{
		Content: "结论",
		Steps:   []agent.Step{{Type: "tool", ID: "c1", Name: "fs_list", Status: "ok"}},
		Timeline: []agent.Segment{
			{Type: "reasoning", Text: "先看目录", DurationMS: 3200},
			{Type: "tool", ToolID: "c1"},
			{Type: "text", Text: "结论"},
		},
	}
	var got []map[string]any
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", res), &got); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if got[0]["duration_ms"] != float64(3200) {
		t.Fatalf("思考段应带耗时 3200ms，实际 %+v", got[0])
	}
	// 正文段不写耗时：整轮耗时在 message.duration_ms，这里再写一份就是两个口径
	if _, ok := got[2]["duration_ms"]; ok {
		t.Fatalf("正文段不该带耗时，实际 %+v", got[2])
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

// 待审批卡与真实回执是同一个 id：续跑补齐之后只该剩回执那一张。
//
// 这就是「审批通过了，卡片还显示待审批」的根因用例：参数超过轨迹上限时，
// 待审批卡写的是原文、回执写的是截断后的串，按参数比较永远比不上。
func TestMergePartsDropsParkedCardByCallID(t *testing.T) {
	long := strings.Repeat("x", agent.MaxToolArgBytes+2048)
	parked := toolParts(t, agent.Step{
		Type: "tool", ID: "call_1", Name: "shell_exec",
		Args: `{"command":"` + long + `"}`, Status: pausedToolStatus,
	})
	receipt := toolParts(t, agent.Step{
		Type: "tool", ID: "call_1", Name: "shell_exec",
		Args: agent.ClampToolArgs(`{"command":"` + long + `"}`), Status: "ok",
	})

	got := mergedParts(t, parked, receipt)
	if len(got) != 1 || got[0]["status"] != "ok" {
		t.Fatalf("待审批卡应被同一次调用的回执顶掉，实际 %+v", got)
	}
}

// 模型重新生成了调用（id 变了）时，按工具名 + 参数认，同样要顶掉。
func TestMergePartsDropsParkedCardBySignature(t *testing.T) {
	args := `{"command":"ls -la"}`
	parked := toolParts(t, agent.Step{
		Type: "tool", ID: "call_a", Name: "shell_exec", Args: args, Status: pausedToolStatus,
	})
	receipt := toolParts(t, agent.Step{
		Type: "tool", ID: "call_b", Name: "shell_exec", Args: args, Status: "ok",
	})

	got := mergedParts(t, parked, receipt)
	if len(got) != 1 || got[0]["status"] != "ok" {
		t.Fatalf("同名同参数的回执应顶掉待审批卡，实际 %+v", got)
	}
}

// 换了一条命令还是待审批（第二次暂停）：这次没拿到回执，卡片必须留着。
func TestMergePartsKeepsParkedCardWithoutReceipt(t *testing.T) {
	parked := toolParts(t, agent.Step{
		Type: "tool", ID: "call_a", Name: "shell_exec",
		Args: `{"command":"ls"}`, Status: pausedToolStatus,
	})
	next := toolParts(t, agent.Step{
		Type: "tool", ID: "call_b", Name: "shell_exec",
		Args: `{"command":"cat a.md"}`, Status: pausedToolStatus,
	})

	got := mergedParts(t, parked, next)
	if len(got) != 2 {
		t.Fatalf("没拿到回执的待审批卡不能丢，实际 %+v", got)
	}
	if got[0]["id"] != "call_a" || got[1]["status"] != pausedToolStatus {
		t.Fatalf("顺序或状态不对：%+v", got)
	}
}

// 待审批卡落库时就走截断：与回执同一份参数，历史里不会出现两种长度。
func TestMarshalPartsClampsParkedArgs(t *testing.T) {
	long := strings.Repeat("x", agent.MaxToolArgBytes+1024)
	parts := marshalParts(discardLogger(), "s1", agent.Result{
		Parked: []agent.PendingCall{{ID: "c1", Name: "shell_exec", Args: `{"command":"` + long + `"}`}},
	})
	var got []map[string]any
	if err := json.Unmarshal(parts, &got); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	args, _ := got[0]["args"].(string)
	if len(args) > agent.MaxToolArgBytes+len("…（已截断）")*3 {
		t.Fatalf("待审批卡的参数应走同一份截断，实际 %d 字节", len(args))
	}
}

// toolParts 把一条 step 序列化成 parts 数组的一元素版本。
func toolParts(t *testing.T, st agent.Step) []byte {
	t.Helper()
	data, err := json.Marshal([]agent.Step{st})
	if err != nil {
		t.Fatalf("序列化 step 失败: %v", err)
	}
	return data
}

// mergedParts 合并两段 parts 并解析结果。
func mergedParts(t *testing.T, head, tail []byte) []map[string]any {
	t.Helper()
	var got []map[string]any
	if err := json.Unmarshal(mergeParts(head, tail), &got); err != nil {
		t.Fatalf("解析合并结果失败: %v", err)
	}
	return got
}
