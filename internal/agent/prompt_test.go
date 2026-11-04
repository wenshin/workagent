package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

func TestSystemMessageSkillIndexIsBudgeted(t *testing.T) {
	skills := make([]tool.SourceEntry, 0, 200)
	for i := 0; i < 200; i++ {
		skills = append(skills, tool.SourceEntry{
			Kind:        "skills",
			Name:        fmt.Sprintf("技能%d", i),
			Description: strings.Repeat("很长的技能描述", 20),
			Path:        fmt.Sprintf("技能%d/SKILL.md", i),
			Source:      "current",
		})
	}

	msg := BuildSystemMessage(RunInput{Skills: skills})
	if got := llm.EstimateTokens(msg.Content); got > skillIndexTokenBudget+300 {
		t.Fatalf("技能索引超预算：%d tokens", got)
	}
	if !strings.Contains(msg.Content, "resource_search") {
		t.Fatal("索引截断后应提示用 resource_search 检索")
	}
}

func TestSystemMessageSkillDocIsTruncatedWithNotice(t *testing.T) {
	doc := strings.Repeat("这是技能正文里的一步操作说明。\n", 2000)
	msg := BuildSystemMessage(RunInput{ActiveSkill: &ActiveSkill{
		Name: "长技能",
		Path: "长技能/SKILL.md",
		Doc:  doc,
	}})

	if got := llm.EstimateTokens(msg.Content); got > skillDocTokenBudget+800 {
		t.Fatalf("技能正文超预算：%d tokens", got)
	}
	for _, want := range []string{"本轮选中的技能：长技能", "已截断", "resource_read"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("缺少 %q", want)
		}
	}
}
