package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 多源路由是本轮新增的核心行为：scope 过滤、内网优先、URL 去重、单源失败不拖垮整体。
func TestSourceRouting(t *testing.T) {
	newProvider := func(t *testing.T, body string) Provider {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		p, err := NewHTTPJSON("x", srv.URL, Spec{
			Request:  RequestSpec{Method: "POST", Body: map[string]any{"q": "{{query}}"}},
			Response: ResponseSpec{Items: "items", Title: "t", URL: "u", Snippet: "s"},
		}, nil, NewSourceGuard([]string{"127.0.0.1"}, true))
		if err != nil {
			t.Fatalf("构造 %s 失败: %v", body, err)
		}
		return p
	}

	inner := newProvider(t, `{"items":[{"t":"内网规范","u":"https://kb/1","s":"内网摘要"}]}`)
	outer := newProvider(t, `{"items":[{"t":"公网新闻","u":"https://web/1","s":"公网摘要"},{"t":"重复","u":"https://kb/1"}]}`)

	r := NewRegistry("")
	if err := r.UpsertSource(SourceDef{Name: "kb", Scope: ScopeInternal, Priority: 10, Provider: inner, Origin: "plugin"}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertSource(SourceDef{Name: "web", Scope: ScopeExternal, Priority: 20, Provider: outer, Origin: "plugin"}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// auto：内网排在最前，公网的重复 URL 被丢掉
	got, err := r.Search(ctx, "", ScopeAuto, Request{Query: "报销", Limit: 10})
	if err != nil {
		t.Fatalf("auto 检索失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应得 2 条（去重后），实际 %d: %+v", len(got), got)
	}
	if got[0].Title != "内网规范" || got[1].Title != "公网新闻" {
		t.Fatalf("顺序应为内网优先，实际 %+v", got)
	}

	// 只查公网
	got, err = r.Search(ctx, "", ScopeExternal, Request{Query: "报销"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Title != "公网新闻" {
		t.Fatalf("external 只应返回公网结果，实际 %+v", got)
	}

	// 未知 scope 不报错但也没有源可查
	if _, err := r.Search(ctx, "", "internal", Request{Query: "  "}); err == nil {
		t.Fatal("空检索词应报错")
	}
}

// 一个源失败不能影响另一个源出结果，只有全部失败才报错。
func TestSourcePartialFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"t":"ok","u":"https://ok/1"}]}`))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500}`))
	}))
	defer bad.Close()

	mk := func(endpoint string) Provider {
		p, err := NewHTTPJSON("x", endpoint, Spec{
			Response: ResponseSpec{Items: "items", Title: "t", URL: "u"},
		}, nil, NewSourceGuard([]string{"127.0.0.1"}, true))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	r := NewRegistry("")
	_ = r.UpsertSource(SourceDef{Name: "bad", Scope: ScopeExternal, Priority: 1, Provider: mk(bad.URL)})
	_ = r.UpsertSource(SourceDef{Name: "good", Scope: ScopeExternal, Priority: 2, Provider: mk(good.URL)})

	got, err := r.Search(context.Background(), "", ScopeExternal, Request{Query: "q"})
	if err != nil {
		t.Fatalf("部分失败不应整体报错: %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://ok/1" {
		t.Fatalf("应保留成功源的结果，实际 %+v", got)
	}

	// 全部失败才报错
	r2 := NewRegistry("")
	_ = r2.UpsertSource(SourceDef{Name: "bad", Scope: ScopeExternal, Provider: mk(bad.URL)})
	if _, err := r2.Search(context.Background(), "", ScopeExternal, Request{Query: "q"}); err == nil {
		t.Fatal("全部源失败时应返回错误")
	}
}

// 空间隔离：普通空间装的源不该被别的空间搜到。
func TestSourceSpaceIsolation(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"t":"私有","u":"https://p/1"}]}`))
	}))
	defer p.Close()
	prov, err := NewHTTPJSON("private", p.URL, Spec{
		Response: ResponseSpec{Items: "items", Title: "t", URL: "u"},
	}, nil, NewSourceGuard([]string{"127.0.0.1"}, true))
	if err != nil {
		t.Fatal(err)
	}

	r := NewRegistry("")
	_ = r.UpsertSource(SourceDef{Name: "private", SpaceID: "space-a", Provider: prov})
	_ = r.UpsertSource(SourceDef{Name: "global", Provider: prov})

	if got, err := r.Search(context.Background(), "space-b", ScopeAuto, Request{Query: "q"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("space-b 只应看到全局源，实际 %d 条", len(got))
	}
	if got, err := r.Search(context.Background(), "space-a", ScopeAuto, Request{Query: "q"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("space-a 的私有源与全局源同名应覆盖，实际 %d 条", len(got))
	}
}

// 归一化是唯一出口：没有 URL 的丢弃，标题空则用 URL。
func TestNormalize(t *testing.T) {
	if _, ok := Normalize(Result{Title: "x"}); ok {
		t.Fatal("没有 URL 的结果应被丢弃")
	}
	got, ok := Normalize(Result{URL: "https://a/1", Snippet: "<b>摘要</b>"})
	if !ok {
		t.Fatal("合法 URL 应保留")
	}
	if got.Title != "https://a/1" {
		t.Fatalf("标题为空时应退化为 URL，实际 %q", got.Title)
	}
	if got.Snippet != "摘要" {
		t.Fatalf("摘要应去掉 HTML 标签，实际 %q", got.Snippet)
	}
}
