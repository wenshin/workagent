package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func wikiEnv(t *testing.T) Env {
	t.Helper()
	env := newTestEnv(t)
	env.WikiSpaceID = "sys1"
	return env
}

func TestWikiPathRules(t *testing.T) {
	valid := []string{"topics/报销制度.md", "log.md", "a/b/c.md"}
	for _, p := range valid {
		if _, err := wikiRel(p, false); err != nil {
			t.Errorf("%q 应合法，却被拒绝: %v", p, err)
		}
	}

	invalid := []string{"", "../x.md", "/etc/passwd", ".hidden.md", "a.png", "a/b/c/d.md", "a/../../b.md"}
	for _, p := range invalid {
		if _, err := wikiRel(p, false); err == nil {
			t.Errorf("%q 应被拒绝", p)
		}
	}

	// 目录参数允许空（wiki 根）且不要求 .md
	if rel, err := wikiRel("", true); err != nil || rel != "" {
		t.Fatalf("空目录应为 wiki 根: rel=%q err=%v", rel, err)
	}
}

func TestWikiUnavailableWithoutHost(t *testing.T) {
	env := newTestEnv(t) // 没有 WikiSpaceID
	_, err := wikiRead().Invoke(context.Background(), json.RawMessage(`{"path":"a.md"}`), env)
	if err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("缺少宿主空间时应报不可用，实际: %v", err)
	}
}

func TestWikiWriteReadVersion(t *testing.T) {
	env := wikiEnv(t)

	out := callTool(t, wikiWrite(), env, `{"path":"topics/报销制度.md","content":"# 报销制度\n餐补 80 元"}`)
	if !strings.Contains(out, "version=") {
		t.Fatalf("写入应返回版本: %s", out)
	}

	read := callTool(t, wikiRead(), env, `{"path":"topics/报销制度.md"}`)
	if !strings.Contains(read, "餐补 80 元") || !strings.Contains(read, "version: ") {
		t.Fatalf("读取内容不符: %s", read)
	}

	// 分页读取也要给出同一版本（版本基于全文而不是片段）
	page := callTool(t, wikiRead(), env, `{"path":"topics/报销制度.md","offset":6,"limit":4}`)
	if !strings.Contains(page, "range: 6-10") {
		t.Fatalf("分段读取缺少范围信息: %s", page)
	}
}

func TestWikiOverwriteVersionConflict(t *testing.T) {
	env := wikiEnv(t)
	callTool(t, wikiWrite(), env, `{"path":"a.md","content":"v1"}`)

	// 不带版本：视为「我知道自己在覆盖」，放行
	callTool(t, wikiWrite(), env, `{"path":"a.md","content":"v2"}`)

	// 带过期版本：必须拒绝，并提示重读
	_, err := wikiWrite().Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","content":"v3","if_version":"deadbeefdeadbeef"}`), env)
	if err == nil || !strings.Contains(err.Error(), "写入冲突") {
		t.Fatalf("过期版本应拒绝，实际: %v", err)
	}

	// 带正确版本：放行
	read := callTool(t, wikiRead(), env, `{"path":"a.md"}`)
	version := ""
	for _, line := range strings.Split(read, "\n") {
		if v, ok := strings.CutPrefix(line, "version: "); ok {
			version = v
		}
	}
	if version == "" {
		t.Fatalf("读取未返回版本: %s", read)
	}
	callTool(t, wikiWrite(), env, `{"path":"a.md","content":"v4","if_version":"`+version+`"}`)
}

func TestWikiCreateRefusesExisting(t *testing.T) {
	env := wikiEnv(t)
	callTool(t, wikiWrite(), env, `{"path":"a.md","content":"x"}`)
	_, err := wikiWrite().Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","content":"y","mode":"create"}`), env)
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("create 已存在应拒绝，实际: %v", err)
	}
}

func TestWikiAppendAccumulates(t *testing.T) {
	env := wikiEnv(t)
	callTool(t, wikiWrite(), env, `{"path":"log/2026-11-10.md","content":"- 09:00 张三反馈"}`)
	callTool(t, wikiWrite(), env, `{"path":"log/2026-11-10.md","content":"- 10:00 李四补充","mode":"append"}`)

	read := callTool(t, wikiRead(), env, `{"path":"log/2026-11-10.md"}`)
	if !strings.Contains(read, "09:00") || !strings.Contains(read, "10:00") {
		t.Fatalf("追加后应保留两条: %s", read)
	}
	if strings.Count(read, "\n") < 2 {
		t.Fatalf("追加应换行而不粘连: %s", read)
	}
}

func TestWikiListAndSearch(t *testing.T) {
	env := wikiEnv(t)
	callTool(t, wikiWrite(), env, `{"path":"topics/报销制度.md","content":"# 报销\n餐补 80 元"}`)
	callTool(t, wikiWrite(), env, `{"path":"topics/请假制度.md","content":"# 请假\n年假 5 天"}`)
	callTool(t, wikiWrite(), env, `{"path":"log/2026-11-10.md","content":"餐补问题已修"}`)

	list := callTool(t, wikiList(), env, `{}`)
	if !strings.Contains(list, "topics/报销制度.md") || !strings.Contains(list, "log/2026-11-10.md") {
		t.Fatalf("列表应包含所有页面: %s", list)
	}

	scoped := callTool(t, wikiList(), env, `{"prefix":"topics"}`)
	if strings.Contains(scoped, "log/") || !strings.Contains(scoped, "请假制度") {
		t.Fatalf("按前缀列表过滤有误: %s", scoped)
	}

	hits := callTool(t, wikiSearch(), env, `{"query":"餐补"}`)
	if !strings.Contains(hits, "topics/报销制度.md") || !strings.Contains(hits, "log/2026-11-10.md") {
		t.Fatalf("搜索应命中两个页面: %s", hits)
	}

	miss := callTool(t, wikiSearch(), env, `{"query":"不存在的词"}`)
	if !strings.Contains(miss, "没有页面命中") {
		t.Fatalf("无命中应给出提示: %s", miss)
	}
}

func TestWikiReadMissing(t *testing.T) {
	env := wikiEnv(t)
	_, err := wikiRead().Invoke(context.Background(), json.RawMessage(`{"path":"nope.md"}`), env)
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("读取不存在的页面应报错，实际: %v", err)
	}
}

func TestFileToolsRejectWikiPath(t *testing.T) {
	env := newTestEnv(t)
	if _, err := fileWrite().Invoke(context.Background(),
		json.RawMessage(`{"path":"wiki/a.md","content":"x"}`), env); err == nil || !strings.Contains(err.Error(), "wiki_") {
		t.Fatalf("file_write 写 wiki/ 应改道提示，实际: %v", err)
	}
	if _, err := fileRead().Invoke(context.Background(),
		json.RawMessage(`{"path":"wiki/a.md"}`), env); err == nil || !strings.Contains(err.Error(), "wiki_") {
		t.Fatalf("file_read 读 wiki/ 应改道提示，实际: %v", err)
	}
	if _, err := fileList().Invoke(context.Background(),
		json.RawMessage(`{"path":"wiki"}`), env); err == nil || !strings.Contains(err.Error(), "wiki_") {
		t.Fatalf("file_list 列 wiki/ 应改道提示，实际: %v", err)
	}
}
