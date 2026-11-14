package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/workagent/auto-agent/internal/golden"
)

func TestEvalDraftWithoutFileIsNoop(t *testing.T) {
	m := &Manager{}
	report, err := m.evalDraft(context.Background(), &Bundle{Files: map[string][]byte{}}, "space", "demo", "u1")
	if err != nil {
		t.Fatalf("没有 evals.json 时不应报错：%v", err)
	}
	if report.Present || !report.OK() {
		t.Fatalf("没有 evals.json 时应视为「未声明用例」：%+v", report)
	}
}

func TestEvalDraftRejectsBrokenFile(t *testing.T) {
	// 用例文件写坏必须拦住：否则门禁会静默失效，作者以为跑过了
	m := &Manager{}
	b := &Bundle{Files: map[string][]byte{
		golden.FileName: []byte(`{"version":1,"cases":[]}`),
	}}
	_, err := m.evalDraft(context.Background(), b, "space", "demo", "u1")
	if err == nil {
		t.Fatal("空用例集应拒绝发布")
	}
	var evalErr *EvalFailedError
	if !errors.As(err, &evalErr) {
		t.Fatalf("错误类型应为 EvalFailedError，实际 %T", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatal("EvalFailedError 必须能被 errors.Is(ErrConflict) 命中，HTTP 层才会返回 409")
	}
}
