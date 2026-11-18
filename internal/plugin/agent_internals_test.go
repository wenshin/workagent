package plugin

import (
	"strings"
	"testing"
)

// 自带件路径只能是插件目录内的规整相对路径：越界、隐藏段、清单本身都要拒绝。
// 这些路径既会拼存储 key，也会拼 plugin_read 的路径，放行一个就等于开了个口子。
func TestAgentAssetPathValidation(t *testing.T) {
	for _, p := range []string{"skills", "skills/周报.md", "tools/crm_query", "skills/口径/大客户.md"} {
		if err := validateAgentAssetPath(p); err != nil {
			t.Errorf("%q 应该合法: %v", p, err)
		}
	}
	for _, p := range []string{
		"", " skills", "skills ", "/abs", "../x", "a/../b", ".hidden/x",
		"skills/", "plugin.json", `a\b`, "a/b/c/d.md",
	} {
		if err := validateAgentAssetPath(p); err == nil {
			t.Errorf("%q 应该被拒绝", p)
		}
	}
}

func TestAgentAssetsRejectTooMany(t *testing.T) {
	skills := make([]string, maxAgentSkills+1)
	for i := range skills {
		skills[i] = "skills"
	}
	// 先撞上限，不进入重复判断
	if err := validateAgentAssets(&AgentSpec{Skills: skills}); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("超量技能应被拒绝，实际: %v", err)
	}

	tools := make([]string, maxAgentTools+1)
	for i := range tools {
		tools[i] = "tools/x"
	}
	if err := validateAgentAssets(&AgentSpec{Tools: tools}); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("超量工具应被拒绝，实际: %v", err)
	}
}

func TestAgentAssetsRejectDuplicates(t *testing.T) {
	if err := validateAgentAssets(&AgentSpec{Skills: []string{"skills", "skills"}}); err == nil {
		t.Fatal("重复的技能路径应被拒绝")
	}
	if err := validateAgentAssets(&AgentSpec{Tools: []string{"tools/x", "tools/x"}}); err == nil {
		t.Fatal("重复的工具路径应被拒绝")
	}
}

// 清单语法对不代表文件在：发布/试跑要对整份快照再校验一次。
func TestValidateAgentInternals(t *testing.T) {
	base := func() *Bundle {
		return &Bundle{
			Meta: Manifest{Kind: KindAgent, Agent: &AgentSpec{
				Skills: []string{"skills"},
				Tools:  []string{"tools/crm_query"},
			}},
			Files: map[string][]byte{
				"plugin.json":                 []byte(agentManifest("")),
				"skills/周报模板.md":              []byte("# 周报模板\n按客户维度汇总本周进展\n"),
				"tools/crm_query/plugin.json": []byte(toolManifest("")),
				"tools/crm_query/main.ts":     []byte("export default () => 1"),
			},
		}
	}

	metas, err := validateAgentInternals(base())
	if err != nil {
		t.Fatalf("合法快照不应报错: %v", err)
	}
	if len(metas) != 1 || metas[0].Slug != "crm_lookup" {
		t.Fatalf("应返回内置工具清单，实际: %+v", metas)
	}

	missingSkill := base()
	delete(missingSkill.Files, "skills/周报模板.md")
	if _, err := validateAgentInternals(missingSkill); err == nil || !strings.Contains(err.Error(), "没有任何 .md") {
		t.Fatalf("技能页缺失应报错，实际: %v", err)
	}

	missingManifest := base()
	delete(missingManifest.Files, "tools/crm_query/plugin.json")
	if _, err := validateAgentInternals(missingManifest); err == nil || !strings.Contains(err.Error(), "plugin.json") {
		t.Fatalf("工具清单缺失应报错，实际: %v", err)
	}

	wrongKind := base()
	wrongKind.Files["tools/crm_query/plugin.json"] = []byte(agentManifest(""))
	if _, err := validateAgentInternals(wrongKind); err == nil || !strings.Contains(err.Error(), "必须是 tool") {
		t.Fatalf("非工具插件应报错，实际: %v", err)
	}

	builtin := base()
	builtin.Files["tools/crm_query/plugin.json"] = []byte(`{
  "protocol": "wa-plugin/1",
  "kind": "tool",
  "slug": "file_write",
  "name": "写文件",
  "description": "平台内置",
  "capability": "http.request",
  "builtin": true,
  "input_schema": {"type": "object", "properties": {}}
}`)
	if _, err := validateAgentInternals(builtin); err == nil || !strings.Contains(err.Error(), "内置件") {
		t.Fatalf("内置件不能作为内置工具，实际: %v", err)
	}

	tooBig := base()
	tooBig.Files["skills/周报模板.md"] = []byte(strings.Repeat("x", maxAgentSkillBytes+1))
	if _, err := validateAgentInternals(tooBig); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超大技能页应报错，实际: %v", err)
	}
}

func TestAgentSkillPagesAndMeta(t *testing.T) {
	files := map[string][]byte{
		"skills/周报.md":     []byte("# 周报模板\n按客户维度汇总\n"),
		"skills/notes.txt": []byte("忽略我"),
		"skills/sub/x.md":  []byte("# 不该被取到"),
	}
	pages := agentSkillPages(files, "skills", maxAgentSkillFiles)
	if len(pages) != 1 || pages[0] != "skills/周报.md" {
		t.Fatalf("目录只取一层 .md，实际: %+v", pages)
	}
	if got := agentSkillPages(files, "skills/目录不存在", maxAgentSkillFiles); len(got) != 0 {
		t.Fatalf("目录不存在应返回空，实际: %+v", got)
	}

	name, desc := agentSkillMeta("skills/周报.md", string(files["skills/周报.md"]))
	if name != "周报模板" || desc != "按客户维度汇总" {
		t.Fatalf("标题/描述解析错误: %q / %q", name, desc)
	}

	// 没有标题时用文件名兜底，描述退回名称本身
	name, desc = agentSkillMeta("skills/无标题.md", "正文第一行\n第二行")
	if name != "无标题" || desc != "正文第一行" {
		t.Fatalf("兜底名称/描述错误: %q / %q", name, desc)
	}
}
