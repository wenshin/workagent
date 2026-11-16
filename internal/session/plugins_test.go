package session

import (
	"testing"

	"github.com/workagent/auto-agent/internal/tool"
)

// 跨空间借用按「plugins/<slug>/」前缀路由，路径解析错了会读到别的插件目录。
func TestPluginSlugOf(t *testing.T) {
	cases := []struct {
		rel  string
		want string
	}{
		{"plugins/analyzer/SKILL.md", "analyzer"},
		{"plugins/analyzer/docs/guide.md", "analyzer"},
		{"plugins/analyzer", ""},
		{"plugins", ""},
		{"notes/a.md", ""},
		{"published/skill/analyzer/1.0.0/SKILL.md", ""},
	}
	for _, c := range cases {
		if got := pluginSlugOf(c.rel); got != c.want {
			t.Fatalf("pluginSlugOf(%q) = %q，期望 %q", c.rel, got, c.want)
		}
	}
}

// extraBySpace 只放行真正的跨空间引用；同空间引用不该重复合并目录。
func TestPluginLoaderExtraBySpace(t *testing.T) {
	l := pluginLoader{
		spaceID: "space-b",
		extraRefs: []tool.PluginRef{
			{SpaceID: "space-a", Slug: "analyzer"},
			{SpaceID: "space-a", Slug: "charts"},
			{SpaceID: "space-b", Slug: "local"},
			{SpaceID: "", Slug: "broken"},
		},
	}
	got := l.extraBySpace()
	if len(got) != 1 || !got["space-a"]["analyzer"] || !got["space-a"]["charts"] {
		t.Fatalf("借用来源聚合不正确: %+v", got)
	}
	if got["space-b"] != nil {
		t.Fatalf("同空间引用不该进入借用池: %+v", got)
	}
}
