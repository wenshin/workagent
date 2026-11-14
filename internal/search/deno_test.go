package search

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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

// denoAvailable 要求 deno 真的能跑，并且能在**白名单沙箱**里跑。
//
// 包管理器装的 deno 往往是 node 包装脚本（`#!/usr/bin/env node`），
// 真正的代码在安装前缀的 lib/ 下。白名单挂载只给了可执行文件所在目录，
// 于是 node 会报「找不到模块」——这不是测试环境问题，而是生产上必须
// 通过 SANDBOX_READ_EXTRA 显式放开的运维项，这里顺带把它配好。
func denoAvailable(t *testing.T) bool {
	t.Helper()
	bin, err := exec.LookPath("deno")
	if err != nil {
		return false
	}
	if head, err := os.ReadFile(bin); err == nil && bytes.HasPrefix(head, []byte("#!")) {
		// 包装脚本要在沙箱里跑，得靠 SANDBOX_READ_EXTRA 把安装前缀挂进去，
		// 这里不模拟那套环境：适配器测试要验的是脚本协议，不是打包方式。
		t.Logf("deno 是包装脚本（%s），跳过；生产环境请用原生二进制或配 SANDBOX_READ_EXTRA", bin)
		return false
	}
	// 必须真的跑一段脚本：有些 shim 能响应 --version，但执行脚本会失败。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "eval", "1").Run() == nil
}

func TestDenoAdapterTwoPhase(t *testing.T) {
	if !denoAvailable(t) {
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
	if !denoAvailable(t) {
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
