package tool

import "testing"

// 智能体自带工具只给绑定它的会话：按 agent_id 精确过滤，
// 不能靠「装没装」判断——它们本来就没有安装记录。
func TestWithoutOtherAgents(t *testing.T) {
	tools := []Tool{
		{Name: "file_read"},
		{Name: "sales_assistant__crm_query", Agent: "sales_assistant"},
		{Name: "reviewer__lint", Agent: "reviewer"},
	}

	got := withoutOtherAgents(tools, "sales_assistant")
	if len(got) != 2 || got[0].Name != "file_read" || got[1].Name != "sales_assistant__crm_query" {
		t.Fatalf("只应保留空间级与自己的工具，实际: %+v", got)
	}

	// 未绑定智能体（历史会话）拿不到任何自带工具
	if got := withoutOtherAgents(tools, ""); len(got) != 1 || got[0].Name != "file_read" {
		t.Fatalf("空 agent_id 不应看到自带工具，实际: %+v", got)
	}

	// 父子不互相继承：reviewer 看不到 sales_assistant 的工具
	if got := withoutOtherAgents(tools, "reviewer"); len(got) != 2 || got[1].Agent != "reviewer" {
		t.Fatalf("不应看到别的智能体的工具，实际: %+v", got)
	}
}

// 没有自带工具时保持原切片，避免每轮都白复制一遍。
func TestWithoutOtherAgentsKeepsSliceWhenNothingScoped(t *testing.T) {
	tools := []Tool{{Name: "file_read"}, {Name: "shell_exec"}}
	if got := withoutOtherAgents(tools, "reviewer"); len(got) != 2 {
		t.Fatalf("不应改变工具集合，实际: %+v", got)
	}
}
