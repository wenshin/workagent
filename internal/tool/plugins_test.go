package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakePlugins 是用于检索测试的插件目录。
type fakePlugins struct {
	entries []PluginEntry
}

func (f fakePlugins) Plugins(context.Context) ([]PluginEntry, error) { return f.entries, nil }

func (f fakePlugins) ReadPlugin(_ context.Context, rel string) (string, error) {
	return "正文：" + rel, nil
}

func (f fakePlugins) RefreshIndex(context.Context) error { return nil }

func searchEnv(entries ...PluginEntry) Env {
	return Env{Plugins: fakePlugins{entries: entries}}
}

func invokeSearch(t *testing.T, env Env, args string) pluginSearchResult {
	t.Helper()
	out, err := pluginSearch().Invoke(context.Background(), json.RawMessage(args), env)
	if err != nil {
		t.Fatalf("检索失败：%v", err)
	}
	var res pluginSearchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("结果不是合法 JSON：%v\n%s", err, out)
	}
	return res
}

func TestPluginSearchRanksNameMatchesFirst(t *testing.T) {
	env := searchEnv(
		PluginEntry{Kind: "skills", Name: "周报生成", Description: "汇总进展", Path: "周报生成/SKILL.md", Source: "current"},
		PluginEntry{Kind: "skills", Name: "文件与目录操作", Description: "支持把内容写进周报需要的模板", Path: "skills/文件与目录操作/SKILL.md", Source: "system"},
	)
	res := invokeSearch(t, env, `{"query":"周报"}`)
	if res.Matched != 2 {
		t.Fatalf("应命中 2 条，得到 %d", res.Matched)
	}
	if res.Items[0].Name != "周报生成" {
		t.Fatalf("名称命中应排第一，实际 %s", res.Items[0].Name)
	}
}

func TestPluginSearchFiltersKind(t *testing.T) {
	env := searchEnv(
		PluginEntry{Kind: "skills", Name: "pdf 生成", Description: "生成 pdf", Path: "pdf/SKILL.md", Source: "current"},
		PluginEntry{Kind: "tools", Name: "pdf_export", Description: "导出 pdf", Path: "tools/pdf_export.json", Source: "system"},
	)
	res := invokeSearch(t, env, `{"query":"pdf","kind":"tools"}`)
	if res.Matched != 1 || res.Items[0].Kind != "tools" {
		t.Fatalf("kind 过滤失效：%+v", res)
	}
}

func TestPluginSearchWarnsOnDuplicateName(t *testing.T) {
	env := searchEnv(
		PluginEntry{Kind: "skills", Name: "周报生成", Description: "开发版", Path: "周报生成/SKILL.md", Source: "current"},
		PluginEntry{Kind: "skills", Name: "周报生成", Description: "系统版", Path: "skills/周报生成/SKILL.md", Source: "system"},
	)
	res := invokeSearch(t, env, `{"query":"周报"}`)
	if !strings.Contains(res.Hint, "同名") {
		t.Fatalf("同名插件应提示用户确认，实际 hint=%q", res.Hint)
	}
}

func TestPluginSearchRequiresQuery(t *testing.T) {
	if _, err := pluginSearch().Invoke(context.Background(), json.RawMessage(`{}`), searchEnv()); err == nil {
		t.Fatal("空 query 应报错")
	}
}

func TestPluginReadSupportsPaging(t *testing.T) {
	env := searchEnv()
	long := strings.Repeat("技能正文行。\n", 2000)
	env.Plugins = fakePlugins{} // ReadPlugin 由 fake 固定返回短文本，这里直接验证分页工具函数
	text := sliceBytes(long, 0, 100, 16*1024, 24*1024)
	if !strings.HasSuffix(text, "（已截断，可用 offset 继续读取）") {
		t.Fatalf("应提示分页：%q", text)
	}
	if got := sliceBytes(long, len(long), 100, 16*1024, 24*1024); got != "" {
		t.Fatalf("越界应返回空串，得到 %q", got)
	}
}
