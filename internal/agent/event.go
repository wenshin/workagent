package agent

import (
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
	// Skills 是当前空间 + 系统空间合并后的技能索引，注入系统提示词的「可用技能」段
	Skills []tool.SourceEntry
	// ActiveSkill 是会话绑定的技能，正文直接进系统提示词
	ActiveSkill *ActiveSkill
	// AgentPrompt 是会话绑定的智能体追加提示词，只能追加在平台基础规则之后
	AgentPrompt string
}

// ActiveSkill 是会话选中的技能。
type ActiveSkill struct {
	Name string
	Path string
	Doc  string
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
}
