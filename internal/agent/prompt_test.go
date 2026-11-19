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
	// 索引删除后，判断规则必须写进系统提示词，否则模型不知道该检索。
	// 外部检索不点 web_search：说明它在工具描述里，基础规则只说「实时信息必须查过再答」。
	for _, want := range []string{"plugin_search", "plugin_read", "必须查过再答"} {
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

// 智能体自带技能只给索引（名称 + 路径 + 一句说明），正文按需 plugin_read；
// 全量注入会把每一轮上下文都撑爆。
func TestAgentSkillsIndexedNotInlined(t *testing.T) {
	msg := BuildSystemMessage(RunInput{AgentSkills: []SkillRef{{
		Name:        "周报模板",
		Description: "按客户维度汇总本周进展",
		Path:        "plugins/sales_assistant/skills/周报模板.md",
	}}})

	for _, want := range []string{"本智能体内置技能", "周报模板", "按客户维度汇总本周进展", "plugins/sales_assistant/skills/周报模板.md"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("缺少内置技能索引 %q", want)
		}
	}
	if strings.Contains(BuildSystemMessage(RunInput{}).Content, "本智能体内置技能") {
		t.Fatal("没有内置技能时不应出现这一段")
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

	// 预算只约束技能正文本身；固定基础规则大约占 1350 tokens（含检索、知识库按
	// 智能体分区、默认在对话里回答与编程任务判断规则），因此这里给的是「正文预算
	// + 基础规则」的余量，微调措辞时不用同步调数字。基础规则成规模增长时要重新
	// 审视预算分配，而不是继续抬这里。
	if got := llm.EstimateTokens(msg.Content); got > skillDocTokenBudget+1400 {
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

	for _, want := range []string{"当前会话的目录", "docs/合同", "真要产出文件时放到这个目录"} {
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

// 工具用法不再常驻基础规则：file_* / wiki_* / shell_exec / web_search / plan_* 的
// 使用约束写在各自的工具描述里（能力面裁剪时规则跟着工具一起消失，不会出现
// 「模型看得见 12KB 分块的规则，却看不到 file_write 这个工具」）。
// 那些描述必须自带关键约束，由 internal/tool 的 TestToolDescriptionsCarryUsageRules 守。

// 普通问答必须在对话里回答：模型手里有 file_write，提示词不说清就会把
// 每个回答都落成 md 文件，用户在会话里只看到「已写入 xxx.md」。
func TestBasePromptAnswersInChatByDefault(t *testing.T) {
	msg := BuildSystemMessage(RunInput{})

	// 只认策略本身，不认工具名：工具名写进基础规则，等于假设这些工具一定在场，
	// 而能力面是按插件安装与智能体裁剪的。
	for _, want := range []string{"默认在对话里回答", "不要产出文件"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("基础规则缺少默认在对话里回答的约束 %q", want)
		}
	}
	// 目录上下文不能把「新建文件」写成默认动作
	dirMsg := BuildSystemMessage(RunInput{Dir: "docs/合同"})
	if !strings.Contains(dirMsg.Content, "不要为了用上这个目录而建文件") {
		t.Fatalf("目录上下文缺少「不为了用上目录而建文件」的约束:\n%s", dirMsg.Content)
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
