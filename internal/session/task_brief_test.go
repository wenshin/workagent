package session

import (
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/tool"
)

// 子会话的第一条消息就是被派发人看到的全部上下文：分段、编号与收尾都不能缺。
func TestComposeTaskBrief(t *testing.T) {
	got := composeTaskBrief(tool.TaskDispatchInput{
		Title:      "获取 2024 年订单明细",
		Background: "运营要看全年复购，口径为支付成功且未退款。",
		Done:       "已实现 SQL：\n```sql\nselect * from orders\n```",
		Checks:     []string{"口径是否用支付时间", "是否需要剔除退款单", ""},
		After:      "沉淀到知识库，方便后续复用。",
	}, "大数据分析智能体", "严小序")

	wants := []string{
		"你好，我是「大数据分析智能体」。",
		"我接到 严小序 的一个需求：获取 2024 年订单明细。",
		"运营要看全年复购，口径为支付成功且未退款。",
		"我这边已经做到：",
		"```sql",
		"辛苦帮忙确认：",
		"1. 口径是否用支付时间",
		"2. 是否需要剔除退款单",
		"确认后，我会沉淀到知识库，方便后续复用。",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("话术缺少 %q：\n%s", want, got)
		}
	}
	// 空的待确认项不能变成「3. 」
	if strings.Contains(got, "3. ") {
		t.Fatalf("空条目不该占编号：\n%s", got)
	}
	// after 自带句号时不能出现「。。」
	if strings.Contains(got, "。。") {
		t.Fatalf("收尾重复标点：\n%s", got)
	}
}

// 没有绑定智能体时回落到「XX 的助理」，不暴露 base 这种内部称呼。
func TestComposeTaskBriefWithoutAgent(t *testing.T) {
	got := composeTaskBrief(tool.TaskDispatchInput{
		Title: "确认口径",
		Brief: "这是一段旧调用写的完整说明。",
	}, "", "严小序")

	if !strings.Contains(got, "你好，我是严小序 的助理。") {
		t.Fatalf("缺少助理开场：\n%s", got)
	}
	if !strings.Contains(got, "他委托我找你确认一件事：确认口径。") {
		t.Fatalf("缺少委托说明：\n%s", got)
	}
	if !strings.Contains(got, "这是一段旧调用写的完整说明。") {
		t.Fatalf("brief 兼容分支丢了：\n%s", got)
	}
	if !strings.Contains(got, "确认后，我会把结论带回发起方会话。") {
		t.Fatalf("缺少默认收尾：\n%s", got)
	}
}

// 超长正文要截断并明确说明，否则对方会以为原文就这么短。
func TestClipTaskPart(t *testing.T) {
	long := strings.Repeat("数", taskBriefPartLimit+10)
	got := clipTaskPart(long, taskBriefPartLimit)
	if !strings.HasSuffix(got, "……（内容较长，已截断）") {
		t.Fatalf("缺少截断提示：%q", got[len(got)-30:])
	}
	if n := len([]rune(got)); n != taskBriefPartLimit+len([]rune("\n……（内容较长，已截断）")) {
		t.Fatalf("截断长度不对：%d", n)
	}
	if short := clipTaskPart("  正常内容  ", taskBriefPartLimit); short != "正常内容" {
		t.Fatalf("短内容不该被改动：%q", short)
	}
}

// 待确认项限量：太多会让人不知道先看哪个。
func TestNormalizeChecks(t *testing.T) {
	items := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		items = append(items, "检查项")
	}
	if got := normalizeChecks(items); len(got) != taskBriefCheckLimit {
		t.Fatalf("待确认项应限量到 %d，实际 %d", taskBriefCheckLimit, len(got))
	}
	if got := normalizeChecks([]string{"  ", ""}); len(got) != 0 {
		t.Fatalf("空条目应被过滤：%v", got)
	}
}
