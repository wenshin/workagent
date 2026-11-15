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
	for _, want := range []string{"plugin_search", "plugin_read", "web_search"} {
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

	// 预算只约束技能正文本身；固定基础规则大约占 1000 tokens（含检索与编程任务判断规则），
	// 因此这里给的是「正文预算 + 基础规则」的余量，微调措辞时不用同步调数字。
	if got := llm.EstimateTokens(msg.Content); got > skillDocTokenBudget+1100 {
		t.Fatalf("技能正文超预算：%d tokens", got)
	}
	for _, want := range []string{"本轮选中的技能：长技能", "已截断", "plugin_read"} {
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

// 知识沉淀必须改道 wiki_*：wiki 工具一直在工具列表里，但提示词不提它，
// 模型就会用 file_write 在当前空间自建 knowledge/ 目录——那份知识只有本空间可见。
func TestBasePromptRoutesKnowledgeToWiki(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})

	for _, want := range []string{"wiki_search", "wiki_list", "wiki_read", "wiki_write", "不要为「沉淀知识」"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("基础规则缺少知识库指引 %q", want)
		}
	}
	// 能力缺失（系统空间没初始化）时不能静默降级成写文件
	if !strings.Contains(msg.Content, "系统空间未初始化") {
		t.Fatal("基础规则缺少 wiki 不可用时的处理要求")
	}
}

// 租户与 IM 接入是「必须恒真」的部署事实，直接进系统提示词。
// 它们不做成工具：模型对「公司用什么 IM」有很强的先验，做成工具它会顺着猜。
func TestPlatformFactsInjected(t *testing.T) {
	msg := BuildSystemMessage(RunInput{Platform: PlatformFacts{
		Tenant:       "测试公司",
		IMName:       "钉钉",
		IMConfigured: true,
		UserName:     "张三",
		JobNumber:    "E001",
		SpaceName:    "研发空间",
	}})

	for _, want := range []string{
		"平台（部署事实）",
		"租户（企业）是「测试公司」",
		"IM 接入是钉钉（登录已配置）",
		"张三，工号 E001",
		"当前空间是「研发空间」",
		"不要凭常识或上下文猜测",
	} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("平台事实缺少 %q\n%s", want, msg.Content)
		}
	}
	// 信息已注入，不应再给模型一个查询同名事实的工具
	if strings.Contains(msg.Content, "platform_info") {
		t.Fatal("平台事实已注入，基础规则里不应再提 platform_info 工具")
	}
}

// 未接线或未登录时整段省略：注入空括号会被读成「租户为空」。
func TestPlatformFactsOmittedWhenUnknown(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})
	if strings.Contains(msg.Content, "平台（部署事实）") {
		t.Fatalf("没有事实时不应注入该段:\n%s", msg.Content)
	}
}

// 凭证没配齐时要说清后果，否则模型会告诉用户「直接扫码登录」。
func TestPlatformFactsReportUnconfiguredIM(t *testing.T) {
	msg := BuildSystemMessage(RunInput{Platform: PlatformFacts{IMName: "企业微信"}})
	if !strings.Contains(msg.Content, "企业微信（登录凭证未配置，用户无法登录）") {
		t.Fatalf("未配置的 IM 缺少说明:\n%s", msg.Content)
	}
}

// 部署事实属于现场，必须排在智能体设定之前：智能体不能改写「我们用什么 IM」。
func TestPlatformFactsBeforeAgentPrompt(t *testing.T) {
	msg := BuildSystemMessage(RunInput{
		AgentPrompt: "AGENT-EXTRA",
		Platform:    PlatformFacts{Tenant: "测试公司"},
	})
	facts := strings.Index(msg.Content, "平台（部署事实）")
	extra := strings.Index(msg.Content, "AGENT-EXTRA")
	if facts < 0 || extra < 0 || facts > extra {
		t.Fatalf("平台事实应排在智能体设定之前: facts=%d extra=%d", facts, extra)
	}
}
