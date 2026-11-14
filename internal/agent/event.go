package agent

import (
	"time"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// 流式事件类型，与方案 §7.3 的运行流示例逐字对齐。
// 所有实现必须只产出这些事件：SSE 契约与 run_step checkpoint 都依赖它（AGENTS「运行流与 checkpoint」）。
const (
	EventRunStarted   = "run.started"
	EventMessageDelta = "message.delta"
	EventToolCall     = "tool.call"
	EventToolResult   = "tool.result"
	EventApprovalReq  = "approval.req"
	EventMessageDone  = "message.done"
	EventRunFinished  = "run.finished"
)

// ApprovalTimeoutS 是审批请求的默认超时（§7.3 approval.req）。
const ApprovalTimeoutS = 300

// Event 是 Agent 运行过程中推送给前端的增量（§7.3）。
type Event struct {
	Type string
	// RunID 由会话层在 run.started 中填充
	RunID string
	// Role 是 message.delta 的角色，固定为 assistant
	Role string
	// Delta 是 message.delta 的文本；Reasoning 为 true 时表示思考过程
	Delta     string
	Reasoning bool
	Tool      *ToolState
	Approval  *ApprovalState
}

// ApprovalState 是一条审批请求（§5.7.5 / §7.3 approval.req）。
type ApprovalState struct {
	ID       string `json:"approval_id"`
	CallID   string `json:"call_id"`
	Tool     string `json:"tool"`
	Reason   string `json:"reason,omitempty"`
	TimeoutS int    `json:"timeout_s"`
}

// ApprovalID 由工具调用 ID 派生稳定的审批 ID，跨事件与 checkpoint 保持一致。
func ApprovalID(callID string) string {
	if callID == "" {
		return "ap_unknown"
	}
	return "ap_" + callID
}

// ToolState 是一次工具调用的完整状态。
type ToolState struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Args       string `json:"args"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status"`
	Risk       int    `json:"risk,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Payload 返回 §7.3 的字段名，SSE 与 checkpoint 共用，避免两处命名漂移。
func (t *ToolState) Payload() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	payload := map[string]any{
		"call_id": t.ID,
		"tool":    t.Name,
		"args":    t.Args,
		"status":  t.Status,
		"risk":    t.Risk,
	}
	if t.Result != "" {
		payload["summary"] = t.Result
	}
	if t.DurationMS > 0 {
		payload["duration_ms"] = t.DurationMS
	}
	return payload
}

// Step 是落库的一条执行轨迹，用于历史消息还原。
type Step struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Args       string `json:"args,omitempty"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status,omitempty"`
	Risk       int    `json:"risk,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// RunInput 是一次 Agent 运行的输入。
type RunInput struct {
	// Env 决定工具能看到的资源范围与权限
	Env tool.Env
	// History 是已组装好的上下文（含 system prompt 之外的历史消息）
	History []llm.Message
	// Query 是用户本轮输入，用于工具相关性裁剪
	Query string
	// Tools 非空时直接使用（测试用）；为空则按注册表加载
	Tools []tool.Tool
	// ActiveSkill 是会话绑定的技能，正文直接进系统提示词
	ActiveSkill *ActiveSkill
	// Dir 是会话所在目录（相对空间 files/），空表示挂在空间根。
	// 会话从某个目录下开出来时，模型需要知道自己的「工作现场」，
	// 否则会当成全局会话、凭空猜文件位置。
	Dir string
	// DirPlugin 非空表示该目录本身就是一个插件（plugins/<slug>），
	// 用户默认想围绕这个插件展开。
	DirPlugin *DirPlugin
	// AgentPrompt 是会话绑定的智能体追加提示词，只能追加在平台基础规则之后
	AgentPrompt string
	// Platform 是本轮已确认的平台部署事实（租户、IM、当前用户与空间），
	// 直接注入系统提示词。做成工具的话模型会凭先验抢答「我们用钉钉吧」，
	// 而这类事实必须恒真，不能放在概率路径上。
	Platform PlatformFacts

	// Depth 是嵌套深度：0 表示用户发起的一轮，>0 表示由 agent_dispatch 派生的子 Agent。
	// 实现必须据此限制派发层级，否则子 Agent 可以无限递归。
	Depth int
	// RoundLimit 非零时覆盖实现自身的轮次上限，用于把父任务的剩余预算传给子 Agent。
	RoundLimit int
	// Deadline 非零时作为本次运行的截止时间（父任务预算继承）；
	// 实现取它与自身墙钟预算中更早的一个。
	Deadline time.Time
	// AllowDispatch 表示本轮允许把 agent_dispatch 暴露给模型。
	AllowDispatch bool
	// SubAgentPrompt 是子 Agent 专属的追加提示词（由派发方给出，同样只能追加）。
	SubAgentPrompt string
}

// ActiveSkill 是会话选中的技能。
type ActiveSkill struct {
	Name string
	Path string
	Doc  string
}

// PlatformFacts 是本轮已知的平台部署事实，只保留「回答用户问题需要」的字段。
//
// 这些字段短、稳定、必须恒真，所以进系统提示词而不是做成工具：工具要多一轮模型
// 调用才生效，而模型对「公司用什么 IM」有很强的先验，往往会顺着猜而不去查。
type PlatformFacts struct {
	// Tenant 是租户（企业）名称，为空表示未知，提示词里就不写这一句
	Tenant string
	// IMName 是 IM 展示名（钉钉 / 企业微信 / 飞书），空表示身份接入未接线
	IMName string
	// IMConfigured 表示该 IM 的凭证是否配置齐；false 时登录不可用
	IMConfigured bool
	// UserName 是当前登录用户姓名，JobNumber 是工号
	UserName  string
	JobNumber string
	// SpaceName 是当前会话所在空间名称
	SpaceName string
}

// DirPlugin 是会话所在目录对应插件的画像，只取系统提示词需要的展示字段。
type DirPlugin struct {
	Slug        string
	Name        string
	Kind        string // 中文类型名，如「技能」
	Description string
}

// Result 是一次 Agent 运行的产出。
type Result struct {
	Content   string
	Reasoning string
	Steps     []Step
	// NeedApproval 表示本次执行中至少有一次工具调用被策略要求审批
	NeedApproval bool
	Provider     string
	Model        string
	Usage        llm.Usage
	// StopReason 是循环的收尾原因：done（正常作答）/ budget（预算用尽，已强制收尾）
	// / stalled（判定再走一步也不会有新进展）。写入 run.budget 与日志，便于事后归因。
	StopReason string
}
