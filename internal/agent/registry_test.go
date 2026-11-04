package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/config"
)

type fakeAgent struct{ name string }

func (f *fakeAgent) Name() string               { return f.name }
func (f *fakeAgent) Capabilities() Capabilities { return Capabilities{Tools: true, Streaming: true} }
func (f *fakeAgent) Ready() bool                { return true }
func (f *fakeAgent) Model() string              { return "fake-model" }
func (f *fakeAgent) Budget() map[string]any     { return map[string]any{"max_rounds": 1} }
func (f *fakeAgent) Run(context.Context, RunInput, func(Event) error) (Result, error) {
	return Result{Content: "ok"}, nil
}

func TestNewBuildsRegisteredImplementation(t *testing.T) {
	var gotDeps Deps
	Register("fake", func(d Deps) (Agent, error) {
		gotDeps = d
		return &fakeAgent{name: "fake"}, nil
	})

	sentinel := &config.Config{}
	impl, err := New("fake", Deps{Config: sentinel})
	if err != nil {
		t.Fatalf("New 失败：%v", err)
	}
	if gotDeps.Config != sentinel {
		t.Fatal("依赖没有透传给实现")
	}
	if impl.Name() != "fake" {
		t.Fatalf("实现名不符：%s", impl.Name())
	}
	if !impl.Capabilities().Tools {
		t.Fatal("能力声明丢失")
	}
	res, err := impl.Run(context.Background(), RunInput{}, nil)
	if err != nil || res.Content != "ok" {
		t.Fatalf("Run 结果不符：%+v %v", res, err)
	}
}

func TestNewUnknownImplementationListsAvailable(t *testing.T) {
	Register("fake", func(Deps) (Agent, error) { return &fakeAgent{name: "fake"}, nil })

	_, err := New("not-exist", Deps{})
	if err == nil {
		t.Fatal("未知实现应当报错")
	}
	if !strings.Contains(err.Error(), "fake") {
		t.Fatalf("错误信息应列出可用实现，实际：%v", err)
	}
}

func TestNewEmptyNameIsConfigurationError(t *testing.T) {
	_, err := New("", Deps{})
	if err == nil {
		t.Fatal("未指定实现应当报错")
	}
	if !strings.Contains(err.Error(), "AGENT_IMPL") {
		t.Fatalf("错误信息应提示 AGENT_IMPL，实际：%v", err)
	}
}
