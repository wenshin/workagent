package ctxmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

func testManager(maxRunes int) *Manager {
	return New(nil, nil, nil, Budget{Window: 8, MaxRunes: maxRunes, TopK: 3, SummaryRunes: 200})
}

// 最新一条自己就超预算时，必须截断保留，而不是整条丢掉。
// 丢掉等于「用户刚发的内容模型一个字都没看到」，而且不报错。
func TestBuildKeepsOversizeLatestMessage(t *testing.T) {
	m := testManager(100)
	long := strings.Repeat("很长的内容", 500) // 2500 字，远超 100 字预算
	msgs, err := m.Build(context.Background(), Input{
		History: []store.Message{{Role: "user", Content: long}},
		Query:   long,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应保留 1 条消息，实际 %d 条", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("角色不对: %s", msgs[0].Role)
	}
	runes := []rune(msgs[0].Content)
	if len(runes) > 101 { // 100 字 + 省略号
		t.Fatalf("截断后仍然过长: %d 字", len(runes))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(msgs[0].Content, "…")) {
		t.Fatal("截断应保留头部原文")
	}
}

// 正常预算下按「从新到旧」装满，旧的被丢掉。
func TestBuildKeepsNewestWithinBudget(t *testing.T) {
	m := testManager(10)
	history := []store.Message{
		{Role: "user", Content: strings.Repeat("旧", 6)},
		{Role: "assistant", Content: strings.Repeat("中", 6)},
		{Role: "user", Content: strings.Repeat("新", 6)},
	}
	msgs, err := m.Build(context.Background(), Input{History: history})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("10 字预算只装得下 1 条，实际 %d 条", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].Content, "新") {
		t.Fatalf("应保留最新一条，实际: %s", msgs[0].Content)
	}
}

// 没超预算时原样返回，顺序不变。
func TestBuildKeepsOrder(t *testing.T) {
	m := testManager(1000)
	history := []store.Message{
		{Role: "user", Content: "一"},
		{Role: "assistant", Content: "二"},
		{Role: "user", Content: "三"},
	}
	msgs, err := m.Build(context.Background(), Input{History: history})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("应保留 3 条，实际 %d", len(msgs))
	}
	for i, want := range []string{"一", "二", "三"} {
		if msgs[i].Content != want {
			t.Fatalf("第 %d 条顺序不对: %s", i, msgs[i].Content)
		}
	}
}

// 只改窗口条数时，不该把其他预算一起清成 0。
func TestNewFillsDefaultsPerField(t *testing.T) {
	m := New(nil, nil, nil, Budget{Window: 4})
	def := DefaultBudget()
	if m.budget.MaxRunes != def.MaxRunes || m.budget.SummaryRunes != def.SummaryRunes {
		t.Fatalf("缺省值没补上: %+v", m.budget)
	}
	if m.budget.Window != 4 {
		t.Fatalf("显式配置的窗口被覆盖了: %d", m.budget.Window)
	}
}
