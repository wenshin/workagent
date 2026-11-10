package search

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

// 声明式配置的价值就是「换个字段路径就能接新源」，这里用两家真实结构做对照。
func TestHTTPJSONParseDifferentShapes(t *testing.T) {
	bochaSpec := Spec{Response: ResponseSpec{
		Items: PathList{"data.webPages.value"}, Title: PathList{"name"}, URL: PathList{"url"},
		Snippet:   PathList{"summary"},
		ErrorCode: "code", ErrorMessage: "msg",
	}}
	p := &HTTPJSON{name: "bocha", spec: bochaSpec}
	bochaBody := `{"code":200,"data":{"webPages":{"value":[
		{"name":"<b>标题</b>","url":"https://a.example/1","summary":"摘要一","datePublished":"2024-05-01"},
		{"name":"无链接","url":""}
	]}}}`
	got, err := p.parse([]byte(bochaBody), 200)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("空 url 应被丢弃，期望 1 条，实际 %d 条", len(got))
	}
	if got[0].Title != "标题" || got[0].Snippet != "摘要一" || got[0].Source != "bocha" {
		t.Fatalf("字段映射不对: %+v", got[0])
	}

	tavilySpec := Spec{Response: ResponseSpec{
		Items: PathList{"results"}, Title: PathList{"title"}, URL: PathList{"url"},
		Snippet: PathList{"content"},
	}}
	p2 := &HTTPJSON{name: "tavily", spec: tavilySpec}
	tavilyBody := `{"results":[{"title":"T","url":"https://b.example/2","content":"C"}]}`
	got2, err := p2.parse([]byte(tavilyBody), 200)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got2) != 1 || got2[0].Title != "T" || got2[0].Snippet != "C" {
		t.Fatalf("tavily 映射不对: %+v", got2)
	}
}

func TestHTTPJSONParseBusinessError(t *testing.T) {
	p := &HTTPJSON{name: "x", spec: Spec{Response: ResponseSpec{
		Items: PathList{"data.value"}, URL: PathList{"url"}, ErrorCode: "code", ErrorMessage: "msg",
	}}}
	if _, err := p.parse([]byte(`{"code":401,"msg":"invalid api key"}`), 200); err == nil {
		t.Fatal("业务错误码非 0 时应报错")
	}
}

func TestHTTPJSONParseMissingItems(t *testing.T) {
	p := &HTTPJSON{name: "x", spec: Spec{Response: ResponseSpec{Items: PathList{"data.value"}, URL: PathList{"url"}}}}
	if _, err := p.parse([]byte(`{"data":{}}`), 200); err == nil {
		t.Fatal("取不到结果数组时应报错，而不是返回空结果冒充「没搜到」")
	}
}

func TestHTTPJSONBuildRequestBody(t *testing.T) {
	h := &HTTPJSON{
		name:     "bocha",
		endpoint: "https://api.example.com/v1/search",
		spec: Spec{Request: RequestSpec{
			Method:  "POST",
			Auth:    &AuthSpec{In: "header", Name: "Authorization", Prefix: "Bearer "},
			Body:    map[string]any{"query": "{{query}}", "count": "{{limit}}", "summary": true},
			Headers: map[string]string{"X-Trace": "{{query}}"},
		}},
	}
	req, err := h.buildRequest(context.Background(), Request{Query: "量子计算", Limit: 5}, "sk-1")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-1" {
		t.Fatalf("密钥注入不对: %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("应默认 JSON Content-Type，实际 %q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("X-Trace") != "量子计算" {
		t.Fatalf("header 模板未替换: %q", req.Header.Get("X-Trace"))
	}
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if body["query"] != "量子计算" {
		t.Fatalf("query 未替换: %v", body["query"])
	}
	if n, ok := body["count"].(float64); !ok || n != 5 {
		t.Fatalf("{{limit}} 应保持数字类型，实际 %#v", body["count"])
	}
}

func TestHTTPJSONBuildRequestGETQueryAuth(t *testing.T) {
	h := &HTTPJSON{
		name:     "serper",
		endpoint: "https://api.example.com/search",
		spec: Spec{Request: RequestSpec{
			Method: "GET",
			Auth:   &AuthSpec{In: "query", Name: "key"},
			Params: map[string]string{"q": "{{query}}", "num": "{{limit}}"},
		}},
	}
	req, err := h.buildRequest(context.Background(), Request{Query: "golang 泛型", Limit: 8}, "k-9")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	q := req.URL.Query()
	if q.Get("q") != "golang 泛型" || q.Get("num") != "8" || q.Get("key") != "k-9" {
		t.Fatalf("查询参数不对: %s", req.URL.RawQuery)
	}
}

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"缺 items", Spec{Response: ResponseSpec{URL: PathList{"url"}}}, false},
		{"缺 url", Spec{Response: ResponseSpec{Items: PathList{"results"}}}, false},
		{"认证方式写错", Spec{Request: RequestSpec{Auth: &AuthSpec{In: "cookie", Name: "k"}},
			Response: ResponseSpec{Items: PathList{"results"}, URL: PathList{"url"}}}, false},
		{"method 写错", Spec{Request: RequestSpec{Method: "PUT"},
			Response: ResponseSpec{Items: PathList{"results"}, URL: PathList{"url"}}}, false},
		{"正常", Spec{Response: ResponseSpec{Items: PathList{"results"}, URL: PathList{"url"}}}, true},
	}
	for _, c := range cases {
		err := c.spec.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: 期望通过，实际 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: 期望报错，实际通过了", c.name)
		}
	}
}

func TestLookup(t *testing.T) {
	root := map[string]any{"data": map[string]any{"value": []any{
		map[string]any{"title": "a"},
		map[string]any{"title": "b"},
	}}}
	if got := text(lookup(root, "data.value[1].title")); got != "b" {
		t.Fatalf("下标取值不对: %q", got)
	}
	if got := lookup(root, "data.missing.title"); got != nil {
		t.Fatalf("不存在的路径应返回 nil，实际 %v", got)
	}
	if got := lookup(root, "data.value[9].title"); got != nil {
		t.Fatalf("越界应返回 nil，实际 %v", got)
	}
}

// 摘要字段在哪家 API 里叫什么都不稳定（博查开了 summary 就把正文放 summary，
// snippet 常常是空的）：数组路径按顺序回退，避免空摘要静默丢掉。
func TestHTTPJSONSnippetFallback(t *testing.T) {
	p := &HTTPJSON{name: "x", spec: Spec{Response: ResponseSpec{
		Items:   PathList{"data.webPages.value"},
		URL:     PathList{"url"},
		Snippet: PathList{"summary", "snippet"},
	}}}
	body := `{"data":{"webPages":{"value":[
		{"url":"https://a.example/1","summary":"","snippet":"只有 snippet"},
		{"url":"https://a.example/2","summary":"只有 summary","snippet":""}
	]}}}`
	got, err := p.parse([]byte(body), 200)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 条，实际 %d 条", len(got))
	}
	if got[0].Snippet != "只有 snippet" || got[1].Snippet != "只有 summary" {
		t.Fatalf("回退没生效: %q / %q", got[0].Snippet, got[1].Snippet)
	}
}

// 老清单里写的是字符串，新清单可以写数组：两种写法都必须能解析。
func TestPathListUnmarshal(t *testing.T) {
	var one Spec
	if err := json.Unmarshal([]byte(`{"response":{"items":"a.b","url":"u","snippet":"summary"}}`), &one); err != nil {
		t.Fatalf("字符串写法解析失败: %v", err)
	}
	if len(one.Response.Snippet) != 1 || one.Response.Snippet[0] != "summary" {
		t.Fatalf("字符串写法解析结果不对: %+v", one.Response.Snippet)
	}

	var many Spec
	raw := `{"response":{"items":["a.b","c.d"],"url":"u","snippet":["summary","snippet"],"published_at":""}}`
	if err := json.Unmarshal([]byte(raw), &many); err != nil {
		t.Fatalf("数组写法解析失败: %v", err)
	}
	if len(many.Response.Items) != 2 || len(many.Response.Snippet) != 2 {
		t.Fatalf("数组写法解析结果不对: %+v", many.Response)
	}
	if len(many.Response.PublishedAt) != 0 {
		t.Fatalf("空字符串应视为不取该字段，实际 %+v", many.Response.PublishedAt)
	}
}
