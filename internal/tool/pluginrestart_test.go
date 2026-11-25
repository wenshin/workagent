package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeRestarter 记录调用参数，供断言「空间与用户来自 Env，不由模型指定」。
type fakeRestarter struct {
	userID, spaceID, slug string
	err                   error
	calls                 int
}

func (f *fakeRestarter) Restart(_ context.Context, userID, spaceID, slug string) error {
	f.calls++
	f.userID, f.spaceID, f.slug = userID, spaceID, slug
	return f.err
}

func TestPluginRestartUsesEnvScope(t *testing.T) {
	fake := &fakeRestarter{}
	env := Env{UserID: "u1", SpaceID: "sp1", PluginRestart: fake}

	out, err := pluginRestart().Invoke(context.Background(), json.RawMessage(`{"slug":"dingtalk_bot"}`), env)
	if err != nil {
		t.Fatalf("重启失败: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("应调用一次重启，实际 %d", fake.calls)
	}
	// 空间与用户只能来自会话上下文：模型传不了别的空间
	if fake.userID != "u1" || fake.spaceID != "sp1" || fake.slug != "dingtalk_bot" {
		t.Fatalf("参数不符: user=%s space=%s slug=%s", fake.userID, fake.spaceID, fake.slug)
	}
	if !strings.Contains(out, "dingtalk_bot") {
		t.Fatalf("回执应带上 slug: %s", out)
	}
}

func TestPluginRestartErrors(t *testing.T) {
	tl := pluginRestart()

	// 没接插件服务时明确报不可用，而不是静默成功
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{"slug":"x"}`), Env{}); err == nil {
		t.Fatal("未注入重启能力时应失败")
	}
	// slug 必填
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{"slug":"  "}`),
		Env{PluginRestart: &fakeRestarter{}}); err == nil {
		t.Fatal("空 slug 应失败")
	}
	// 插件层错误原样带回：未启用 / 不存在由调用方决定怎么展示
	fake := &fakeRestarter{err: errors.New("未在本空间启用")}
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{"slug":"x"}`),
		Env{PluginRestart: fake}); err == nil {
		t.Fatal("重启失败应把错误带回模型")
	}
}
