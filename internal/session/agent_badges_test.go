package session

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

type fakeAgentSource struct {
	enabled  []agentspec.Summary
	allNames map[string]string
}

func (f *fakeAgentSource) ListAgentPlugins(context.Context, string) ([]agentspec.Summary, error) {
	return f.enabled, nil
}

func (f *fakeAgentSource) AgentDisplayNames(context.Context, string) (map[string]string, error) {
	return f.allNames, nil
}

func (f *fakeAgentSource) ResolveAgentPlugin(context.Context, string, string) (agentspec.Resolved, error) {
	return agentspec.Resolved{}, store.ErrNotFound
}

func (f *fakeAgentSource) AgentDependencies(context.Context, string, string) ([]string, error) {
	return nil, store.ErrNotFound
}

func (f *fakeAgentSource) AgentInternals(context.Context, string, string) ([]tool.PluginEntry, error) {
	return nil, store.ErrNotFound
}

// 快照只含已启用的插件；停用/草稿的智能体要靠展示名兜底。
// 不做这一层，会话图标就会退化成英文 slug——同一张插件卡片是中文，会话里是英文。
func TestAgentBadgesFallsBackToDisplayName(t *testing.T) {
	src := &fakeAgentSource{
		enabled:  []agentspec.Summary{{ID: "reviewer", Name: "代码评审"}},
		allNames: map[string]string{"reviewer": "代码评审", "writer": "文案助手"},
	}
	svc := &Service{agentPlugins: src, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := svc.AgentBadges(context.Background(), []store.Session{
		{ID: "s1", SpaceID: "sp1", AgentID: "reviewer"},
		{ID: "s2", SpaceID: "sp1", AgentID: "writer"},
		{ID: "s3", SpaceID: "sp1"},
	})

	if name := got["sp1\x00reviewer"].Name; name != "代码评审" {
		t.Fatalf("已启用智能体应取快照名，实际 %q", name)
	}
	if name := got["sp1\x00writer"].Name; name != "文案助手" {
		t.Fatalf("停用智能体应回落到展示名，实际 %q", name)
	}
	if _, ok := got["sp1\x00"]; ok {
		t.Fatal("未绑定智能体的会话不应产生条目")
	}
}
