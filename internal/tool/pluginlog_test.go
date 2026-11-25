package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakePluginLogs struct {
	calls   []PluginCall
	spaceID string
	slug    string
	limit   int
}

func (f *fakePluginLogs) RecentCalls(_ context.Context, spaceID, slug string, limit int) []PluginCall {
	f.spaceID, f.slug, f.limit = spaceID, slug, limit
	out := f.calls
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func TestPluginLogsFormatsCalls(t *testing.T) {
	logs := &fakePluginLogs{calls: []PluginCall{
		{
			At: time.Date(2026, 11, 24, 14, 3, 21, 0, time.Local), Tool: "dingtalk_bot.dingtalk_bot",
			OK: false, Duration: 820 * time.Millisecond, Err: "远端返回 400：invalid appkey",
		},
		{
			At: time.Date(2026, 11, 24, 14, 2, 55, 0, time.Local), Tool: "dingtalk_bot.dingtalk_bot",
			OK: true, Duration: 310 * time.Millisecond,
		},
	}}
	env := Env{SpaceID: "s1", PluginLogs: logs}

	out, err := invokePluginLogs(context.Background(), json.RawMessage(`{"slug":"dingtalk_bot"}`), env)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 空间必须来自会话，限条数要有默认值：工具不能跨空间查别人的记录。
	if logs.spaceID != "s1" || logs.limit != 20 {
		t.Fatalf("参数传递不对：space=%q limit=%d", logs.spaceID, logs.limit)
	}
	for _, want := range []string{"失败", "成功", "820ms", "invalid appkey"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	// 失败记录的错误要挂在它自己下面，不能和成功那条混在一样。
	if strings.Index(out, "失败") > strings.Index(out, "invalid appkey") {
		t.Fatalf("错误摘要应跟在失败记录之后：\n%s", out)
	}
}

func TestPluginLogsWithoutRecords(t *testing.T) {
	env := Env{SpaceID: "s1", PluginLogs: &fakePluginLogs{}}

	out, err := invokePluginLogs(context.Background(), json.RawMessage(`{"slug":"bot"}`), env)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 空结果要说明原因，别让模型以为查到了「一切正常」。
	if !strings.Contains(out, "没有") || !strings.Contains(out, "重启") {
		t.Fatalf("空结果应说明原因：\n%s", out)
	}
}

func TestPluginLogsRequiresSlug(t *testing.T) {
	env := Env{SpaceID: "s1", PluginLogs: &fakePluginLogs{}}

	if _, err := invokePluginLogs(context.Background(), json.RawMessage(`{}`), env); err == nil {
		t.Fatal("缺 slug 应报错，而不是查全部")
	}
}
