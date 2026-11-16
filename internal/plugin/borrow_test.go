package plugin

import (
	"testing"

	"github.com/workagent/auto-agent/internal/tool"
)

// 借用缓存按「会话空间 + 引用集合」为键：refs 顺序不同必须命中同一条缓存。
func TestBorrowKey(t *testing.T) {
	a := borrowKey("space-b", []tool.PluginRef{
		{SpaceID: "space-a", Slug: "analyzer"},
		{SpaceID: "space-a", Slug: "charts"},
	})
	b := borrowKey("space-b", []tool.PluginRef{
		{SpaceID: "space-a", Slug: "charts"},
		{SpaceID: "space-a", Slug: "analyzer"},
	})
	if a != b {
		t.Fatalf("引用顺序不该影响缓存键: %q != %q", a, b)
	}

	if borrowKey("space-c", []tool.PluginRef{
		{SpaceID: "space-a", Slug: "analyzer"},
		{SpaceID: "space-a", Slug: "charts"},
	}) == a {
		t.Fatal("不同会话空间必须分开缓存")
	}
}
