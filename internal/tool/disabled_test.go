package tool

import (
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

// 停用内置工具必须真的从工具列表里消失：内置工具的执行体是 Go 代码，
// 安装记录只是元数据，漏了这一步「停用」就只是个标签。
func TestDisabledToolNames(t *testing.T) {
	off := disabledToolNames([]store.PluginInstall{
		{Slug: "tool_web_search", Status: store.PluginInstallDisabled},
		{Slug: "tool_file_read", Status: store.PluginInstallEnabled},
		{Slug: "builtin_files", Status: store.PluginInstallDisabled}, // 非 tool_*：不是工具
	})
	if !off["web_search"] {
		t.Fatalf("停用的工具没被识别: %v", off)
	}
	if off["file_read"] {
		t.Fatalf("启用的工具被误判成停用: %v", off)
	}
	if len(off) != 1 {
		t.Fatalf("只应摘掉一个工具，得到 %v", off)
	}
}

func TestWithoutTools(t *testing.T) {
	tools := withoutTools(builtinTools(), map[string]bool{"web_search": true})
	for _, item := range tools {
		if item.Name == "web_search" {
			t.Fatal("被停用的 web_search 仍在列表里")
		}
	}
	if len(tools) != len(builtinTools())-1 {
		t.Fatalf("工具数量不对: %d", len(tools))
	}
	// 没有停用项时应当原样返回，避免每次 Load 都多分配一份
	all := builtinTools()
	if out := withoutTools(all, nil); len(out) != len(all) {
		t.Fatalf("空停用集不该改变结果: %d", len(out))
	}
}
