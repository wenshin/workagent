package search

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// fakeDoer 替掉 Guard：这里要验的是「脚本 → 请求 → 映射」这条链路，
// 不是出网防护（Guard 有单独测试）。
type fakeDoer struct {
	req      *http.Request
	reqBody  string
	status   int
	respBody string
	err      error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.req = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.reqBody = string(b)
	}
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.respBody)),
	}, nil
}

func denoAvailable() bool {
	_, err := exec.LookPath("deno")
	return err == nil
}

func TestDenoAdapterTwoPhase(t *testing.T) {
	if !denoAvailable() {
		t.Skip("没装 deno，跳过脚本搜索源测试")
	}
	client := &fakeDoer{respBody: `{"code":0,"items":[
		{"t":"Go 并发","u":"https://example.com/a","s":"goroutine","d":"2026-01-01"},
		{"t":"空链接","u":"","s":"应当被过滤"}
	]}`}
	keyFunc := func(context.Context) (string, error) { return "sk-test", nil }

	provider, err := NewDeno("demo", "testdata/adapter.ts", "https://example.com/search", keyFunc, client, sandbox.Detect())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	got, err := provider.Search(context.Background(), Request{Query: "golang", Limit: 5})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应过滤掉空链接，实际 %d 条: %+v", len(got), got)
	}
	if got[0].Title != "Go 并发" || got[0].URL != "https://example.com/a" || got[0].Snippet != "goroutine" {
		t.Errorf("字段映射不对: %+v", got[0])
	}
	if got[0].PublishedAt != "2026-01-01" {
		t.Errorf("published_at 没映射上: %+v", got[0])
	}

	// 密钥只能经 stdin 进入 plan 段，最终体现在请求头上
	if auth := client.req.Header.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("Authorization 不对: %q", auth)
	}
	if client.req.URL.String() != "https://example.com/search" {
		t.Errorf("请求地址不对: %s", client.req.URL)
	}
	if !strings.Contains(client.reqBody, `"q":"golang"`) {
		t.Errorf("请求体没带上 query: %s", client.reqBody)
	}
}

func TestDenoAdapterBusinessError(t *testing.T) {
	if !denoAvailable() {
		t.Skip("没装 deno，跳过脚本搜索源测试")
	}
	client := &fakeDoer{respBody: `{"code":401,"msg":"invalid key"}`}
	keyFunc := func(context.Context) (string, error) { return "sk-test", nil }

	provider, err := NewDeno("demo", "testdata/adapter.ts", "https://example.com/search", keyFunc, client, sandbox.Detect())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, err = provider.Search(context.Background(), Request{Query: "x"})
	if err == nil {
		t.Fatal("业务错误码应该报错，而不是当成 0 条结果")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误信息应包含业务码: %v", err)
	}
}

func TestDenoRejectsBadScriptPath(t *testing.T) {
	if _, err := NewDeno("demo", "testdata/missing.ts", "", func(context.Context) (string, error) { return "", nil }, &fakeDoer{}, sandbox.NewLocal()); err == nil {
		t.Fatal("脚本不存在时应构造失败，让装配层回退")
	}
	if _, err := NewDeno("demo", "testdata/adapter.ts", "", func(context.Context) (string, error) { return "", nil }, nil, sandbox.NewLocal()); err == nil {
		t.Fatal("缺少出网通道时应构造失败")
	}
	if _, err := NewDeno("demo", "testdata/adapter.ts", "", nil, &fakeDoer{}, sandbox.NewLocal()); err == nil {
		t.Fatal("缺少密钥来源时应构造失败")
	}
}

func TestNormalizeResults(t *testing.T) {
	got := normalizeResults([]Result{
		{Title: "", URL: "https://a.example/1"},
		{Title: "B", URL: "javascript:alert(1)"},
		{Title: strings.Repeat("x", 400), URL: "https://a.example/2"},
	}, 1)
	if len(got) != 1 {
		t.Fatalf("limit 没生效或被非法 URL 绕过: %+v", got)
	}
	if got[0].Title != "https://a.example/1" {
		t.Errorf("空标题应回落成 URL: %+v", got[0])
	}
}
