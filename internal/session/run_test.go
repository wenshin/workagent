package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/store"
)

// 连续没有事件才算卡住；touch 一下就要重新计时，否则长任务会被误杀。
func TestRunWatchdogIdleTimeout(t *testing.T) {
	fired := make(chan string, 4)
	w := newRunWatchdog(120*time.Millisecond, 0, func(reason string) {
		fired <- reason
	})
	defer w.stop()

	// 每 60ms 喂一次事件：总时长早已超过 idle，但不该触发
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.touch()
		time.Sleep(60 * time.Millisecond)
	}

	select {
	case reason := <-fired:
		t.Fatalf("持续有事件时不该触发，却收到 %q", reason)
	default:
	}

	// 停止喂事件后，idle 到点必须触发且只触发一次
	select {
	case reason := <-fired:
		if reason != ReasonIdleTimeout {
			t.Fatalf("原因应为 %q，实际 %q", ReasonIdleTimeout, reason)
		}
	case <-time.After(time.Second):
		t.Fatal("idle 到点没有触发")
	}

	select {
	case <-fired:
		t.Fatal("只该触发一次")
	default:
	}
}

// 总时长上限是最后一道保险：即使事件不断，到点也要停。
func TestRunWatchdogMaxDuration(t *testing.T) {
	fired := make(chan string, 2)
	w := newRunWatchdog(0, 100*time.Millisecond, func(reason string) {
		fired <- reason
	})
	defer w.stop()

	for i := 0; i < 5; i++ {
		w.touch()
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case reason := <-fired:
		if reason != ReasonWallClockExceeded {
			t.Fatalf("原因应为 %q，实际 %q", ReasonWallClockExceeded, reason)
		}
	case <-time.After(time.Second):
		t.Fatal("总上限到点没有触发")
	}
}

// stop 之后计时器不能再回头把已经收尾的一轮判成中断。
func TestRunWatchdogStops(t *testing.T) {
	fired := make(chan struct{}, 1)
	w := newRunWatchdog(50*time.Millisecond, 80*time.Millisecond, func(string) {
		fired <- struct{}{}
	})
	w.stop()

	select {
	case <-fired:
		t.Fatal("停止后不该再触发")
	case <-time.After(200 * time.Millisecond):
	}
}

// 断点去留是产品语义：只有系统侧中断值得「继续」。
func TestKeepResume(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
		want   bool
	}{
		{"卡死保留", store.RunCancelled, ReasonIdleTimeout, true},
		{"到点保留", store.RunCancelled, ReasonWallClockExceeded, true},
		{"非预算类失败保留", store.RunFailed, "", true},
		{"用户停止不留", store.RunCancelled, ReasonUserCancelled, false},
		{"预算用尽不留", store.RunCancelled, ReasonBudgetExceeded, false},
		{"停滞不留", store.RunCancelled, ReasonStalled, false},
		{"成功不留", store.RunSucceeded, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keepResume(c.status, c.reason); got != c.want {
				t.Fatalf("keepResume(%q, %q) = %v，期望 %v", c.status, c.reason, got, c.want)
			}
		})
	}
}

// 三种中断的含义不同，文案必须分开：混成一句会让用户以为是同一件事。
func TestUserFacingCancelError(t *testing.T) {
	if got := userFacingCancelError(ReasonIdleTimeout, nil); got != RunIdleTimeoutMessage {
		t.Fatalf("卡死文案不对: %q", got)
	}
	if got := userFacingCancelError(ReasonWallClockExceeded, nil); got != RunBudgetExceededMessage {
		t.Fatalf("到点文案不对: %q", got)
	}
	if RunIdleTimeoutMessage == RunBudgetExceededMessage {
		t.Fatal("卡住与到点必须有各自的文案")
	}
}

// 「第一个动作就要确认」的那一轮没有正文也没有已完成的工具：如果连挂起的调用都不
// 落进 parts，这条消息就是一条空产出，前端会当成空占位藏掉——用户点完同意，
// 暂停前想干什么全没了。
func TestMarshalPartsKeepsParkedCalls(t *testing.T) {
	res := agent.Result{
		Parked: []agent.PendingCall{{ID: "call-1", Name: "mcp_write", Args: `{"id":"1"}`}},
	}

	var parts []map[string]any
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", res), &parts); err != nil {
		t.Fatalf("parts 不是合法 JSON: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("挂起的调用必须留在这一轮里，实际 %d 条", len(parts))
	}
	if parts[0]["id"] != "call-1" || parts[0]["name"] != "mcp_write" {
		t.Fatalf("挂起的调用信息不对: %+v", parts[0])
	}
	if parts[0]["status"] != pausedToolStatus {
		t.Fatalf("没有回执的调用只能标成待审批，实际 %v", parts[0]["status"])
	}
	// 同一批里已经执行完的调用照旧落在前面，顺序不能被打乱
	res.Steps = []agent.Step{{Type: "tool", ID: "call-0", Name: "read_file", Status: "ok"}}
	if err := json.Unmarshal(marshalParts(discardLogger(), "s1", res), &parts); err != nil {
		t.Fatalf("parts 不是合法 JSON: %v", err)
	}
	if len(parts) != 2 || parts[0]["id"] != "call-0" || parts[1]["id"] != "call-1" {
		t.Fatalf("执行过的与挂起的要按发生顺序排列，实际 %+v", parts)
	}
}

// 续跑要接着暂停那一轮写：正文接在后面、用量相加，同一步的待审批卡被真实回执顶掉。
func TestMergeCarriedContinuesSameMessage(t *testing.T) {
	head, _ := json.Marshal([]map[string]any{
		{"type": "tool", "id": "call-1", "name": "mcp_write", "args": `{"id":"1"}`, "status": pausedToolStatus},
	})
	carry := &store.Message{
		Content:   "我先查一下库存。",
		Reasoning: "先看库存",
		Parts:     head,
		Stats:     store.MessageStats{InTokens: 10, OutTokens: 5, DurationMS: 1200},
	}
	tail, _ := json.Marshal([]map[string]any{
		{"type": "tool", "id": "call-9", "name": "mcp_write", "args": `{"id":"1"}`, "status": "ok", "result": "已写入"},
		{"type": "text", "text": "已经改好了。"},
	})

	content, reasoning, parts, stats := mergeCarried(carry,
		"已经改好了。", "", tail,
		store.MessageStats{InTokens: 3, OutTokens: 7, DurationMS: 800})

	if content != "我先查一下库存。\n\n已经改好了。" {
		t.Fatalf("正文要接着写而不是覆盖，实际 %q", content)
	}
	if reasoning != "先看库存" {
		t.Fatalf("续跑没产思考时应保留原来的，实际 %q", reasoning)
	}
	if stats.InTokens != 13 || stats.OutTokens != 12 || stats.DurationMS != 2000 {
		t.Fatalf("用量要相加，实际 %+v", stats)
	}
	var got []map[string]any
	if err := json.Unmarshal(parts, &got); err != nil {
		t.Fatalf("合并后的 parts 不合法: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("待审批卡应被同一步的真实回执顶掉，实际 %+v", got)
	}
	if got[0]["status"] != "ok" || got[1]["type"] != "text" {
		t.Fatalf("合并顺序不对: %+v", got)
	}
	// 没拿到回执的待审批卡要留着：那一步确实还没做完
	other, _ := json.Marshal([]map[string]any{{"type": "text", "text": "换个话题"}})
	_, _, parts, _ = mergeCarried(carry, "换个话题", "", other, store.MessageStats{})
	if err := json.Unmarshal(parts, &got); err != nil {
		t.Fatalf("合并后的 parts 不合法: %v", err)
	}
	if len(got) != 2 || got[0]["status"] != pausedToolStatus {
		t.Fatalf("没有被顶掉的待审批卡不能消失，实际 %+v", got)
	}
	if !strings.Contains(string(parts), "换个话题") {
		t.Fatalf("新的正文段要接在后面，实际 %s", parts)
	}
}
