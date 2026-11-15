package space

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// 停用是运行期状态：plugins/<slug>/ 副本还在磁盘上，索引照样会把它算进来。
// 合并时必须按停用集合摘掉，否则「停用」对靠索引进检索池的技能/组件等于没生效。
func TestWithoutDisabledDropsPluginEntries(t *testing.T) {
	s := &Service{log: slog.Default()}
	s.SetDisabledPluginSource(func(ctx context.Context, spaceID string) (map[string]bool, error) {
		return map[string]bool{"builtin_plugin_dev": true, "tool_web_search": true}, nil
	})

	in := IndexBundle{
		Skills: []IndexEntry{
			{Name: "插件开发", Path: "plugins/builtin_plugin_dev/SKILL.md"},
			{Name: "文件操作", Path: "plugins/builtin_files/SKILL.md"},
		},
		Tools: []IndexEntry{
			{Name: "web_search", Path: "published/tools/tool_web_search/1.0.0/plugin.json"},
			{Name: "file_read", Path: "published/tools/tool_file_read/1.0.0/plugin.json"},
		},
		MCPS: []IndexEntry{{Name: "自建 MCP", Path: "plugins/my_mcp/plugin.json"}},
		Components: []IndexEntry{
			{Name: "组件", Path: "published/components/my_ui/2.0.0/ui.json"},
		},
	}

	out := s.withoutDisabled(context.Background(), "sp1", in)
	if len(out.Skills) != 1 || out.Skills[0].Name != "文件操作" {
		t.Fatalf("技能没按停用摘掉: %+v", out.Skills)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "file_read" {
		t.Fatalf("工具没按停用摘掉: %+v", out.Tools)
	}
	if len(out.MCPS) != 1 {
		t.Fatalf("未停用的条目不该动: %+v", out.MCPS)
	}
	if len(out.Components) != 1 {
		t.Fatalf("未停用的组件不该动: %+v", out.Components)
	}
}

func TestWithoutDisabledDegradesOnError(t *testing.T) {
	s := &Service{log: slog.Default()}
	s.SetDisabledPluginSource(func(ctx context.Context, spaceID string) (map[string]bool, error) {
		return nil, errors.New("db down")
	})
	in := IndexBundle{Skills: []IndexEntry{{Name: "a", Path: "plugins/a/SKILL.md"}}}
	if out := s.withoutDisabled(context.Background(), "sp1", in); len(out.Skills) != 1 {
		t.Fatalf("查询失败时应当原样返回，而不是把资源清空: %+v", out.Skills)
	}
}
