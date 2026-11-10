package agent

import (
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/llm"
)

func TestSystemMessageHasNoSkillIndex(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})

	if strings.Contains(msg.Content, "可用技能（索引") {
		t.Fatal("平台不再常驻技能索引，系统提示词里不应出现索引段")
	}
	// 索引删除后，判断规则必须写进系统提示词，否则模型不知道该检索
	for _, want := range []string{"resource_search", "resource_read", "web_search"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("系统提示词缺少检索规则 %q", want)
		}
	}
}

func TestAgentPromptAppendedAfterBaseRules(t *testing.T) {
	msg := BuildSystemMessage(RunInput{AgentPrompt: "AGENT-EXTRA"})

	base := strings.Index(msg.Content, "你是 WorkAgent")
	extra := strings.Index(msg.Content, "AGENT-EXTRA")
	if base < 0 || extra < 0 {
		t.Fatalf("基础规则或智能体设定缺失: base=%d extra=%d", base, extra)
	}
	if extra < base {
		t.Fatal("智能体设定不能出现在基础规则之前")
	}
	// 结构上声明优先级，避免配置里写「忽略以上要求」就改掉平台约束
	if !strings.Contains(msg.Content, "以上面的要求为准") {
		t.Fatal("缺少优先级声明")
	}
}

func TestAgentPromptBeforeActiveSkill(t *testing.T) {
	msg := BuildSystemMessage(RunInput{
		AgentPrompt: "AGENT-EXTRA",
		ActiveSkill: &ActiveSkill{Name: "写周报", Path: "写周报/SKILL.md", Doc: "SKILL-DOC"},
	})

	extra := strings.Index(msg.Content, "AGENT-EXTRA")
	doc := strings.Index(msg.Content, "SKILL-DOC")
	if extra < 0 || doc < 0 || doc < extra {
		t.Fatalf("顺序应为 基础规则 → 智能体设定 → 技能正文: extra=%d doc=%d", extra, doc)
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

func TestDirContextMentionsDirectory(t *testing.T) {
	msg := BuildSystemMessage(RunInput{Dir: "docs/合同"})

	for _, want := range []string{"当前会话的目录", "docs/合同", "新建文件也放在这里"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("普通目录上下文缺少 %q", want)
		}
	}
	if strings.Contains(msg.Content, "plugin_try") {
		t.Fatal("普通目录不应提插件试跑")
	}
}

func TestDirPluginContextAnchorsSession(t *testing.T) {
	msg := BuildSystemMessage(RunInput{
		Dir:         "plugins/order_lookup",
		AgentPrompt: "AGENT-EXTRA",
		DirPlugin: &DirPlugin{
			Slug:        "order_lookup",
			Name:        "订单查询",
			Kind:        "工具插件",
			Description: "按订单号查询状态与金额",
		},
	})

	for _, want := range []string{
		"当前会话的目录",
		"插件「订单查询」（工具插件）",
		"plugins/order_lookup",
		"按订单号查询状态与金额",
		"plugin_try",
	} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("插件目录上下文缺少 %q", want)
		}
	}
	// 目录是现场事实，必须排在角色设定之前
	if dir, extra := strings.Index(msg.Content, "当前会话的目录"), strings.Index(msg.Content, "AGENT-EXTRA"); dir < 0 || extra < 0 || dir > extra {
		t.Fatalf("目录上下文应排在智能体设定之前: dir=%d extra=%d", dir, extra)
	}
}

func TestDirPluginContextFallsBackToSlug(t *testing.T) {
	msg := BuildSystemMessage(RunInput{
		Dir:       "plugins/draft_thing/index.ts",
		DirPlugin: &DirPlugin{Slug: "draft_thing"},
	})

	// 清单缺失或解析失败时，至少要说清「这是个插件目录」而不是普通目录
	if !strings.Contains(msg.Content, "插件「draft_thing」") {
		t.Fatalf("缺少按 slug 兜底的插件名: %s", msg.Content)
	}
}

func TestNoDirContextWhenRoot(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})
	if strings.Contains(msg.Content, "当前会话的目录") {
		t.Fatal("空间根会话不应出现目录上下文")
	}
}

// 基础规则必须自带「不索要明文凭证」的硬约束：技能文档可能没被检索到，
// 但模型一旦开口要 Key，明文就会进消息表与上下文。
func TestBasePromptForbidsAskingForPlaintextSecrets(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})

	for _, want := range []string{"不要向用户索要明文", "secret_request", "已经泄露"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("基础规则缺少凭证约束 %q", want)
		}
	}
}
