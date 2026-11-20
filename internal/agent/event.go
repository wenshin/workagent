package agent

import (
	"fmt"
	"strings"
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
	ID     string `json:"approval_id"`
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Kind 是动作类别（net.write / shell），前端据此显示图标与文案。
	Kind string `json:"kind,omitempty"`
	// Detail 是命令原文或入参摘要，审批卡片要让人看清「确认什么」。
	Detail   string `json:"detail,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Risk     int    `json:"risk,omitempty"`
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

// Segment 是消息时间线上的一段：思考、正文或一次工具调用。
//
// 落库时按顺序写进 message.parts，历史消息据此还原「先想一段 → 调工具 → 再说一句」
// 的原始顺序。只靠 reasoning + parts（纯工具列表）两个字段拼不回时间位置，
// 排障时看到的就是「思考一堆在顶、工具一堆在中、正文在底」。
type Segment struct {
	// Type 取 reasoning / text / tool
	Type string
	// Text 是 reasoning / text 段的正文
	Text string
	// ToolID 是 tool 段对应的调用 id，指向 Result.Steps 里的同 id 步骤
	ToolID string
}

// AppendSegment 把一段思考 / 正文接到时间线末尾。
//
// 增量是一个 token 一个到达的：每次新开一段会让历史回放出现几十个折叠块，
// 所以同类型连续到达时续写同一段；只有工具调用插进来才真正开始新的一段。
func (r *Result) AppendSegment(typ, text string) {
	if text == "" {
		return
	}
	n := len(r.Timeline)
	if n > 0 && r.Timeline[n-1].Type == typ {
		r.Timeline[n-1].Text += text
		return
	}
	r.Timeline = append(r.Timeline, Segment{Type: typ, Text: text})
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

// 三档行为模式：问答 / 任务 / 编排。
//
// 模式不是权限：它只如实记录这一轮走到了哪一档，用于事后归因（多少任务被误判成
// 编排、多少降级发生在第几轮）与前端展示。判定由模型按提示词完成，平台不替它选。
const (
	ModeAnswer      = "answer"
	ModeExec        = "exec"
	ModeOrchestrate = "orchestrate"
)

// 清单条目的状态。
const (
	PlanPending = "pending"
	PlanDoing   = "doing"
	PlanDone    = "done"
	// PlanBlocked 表示这一步卡住了（缺信息、缺权限、等外部确认）。
	// 它与 pending 的区别是「再等也不会有进展」，收尾时必须单独说明。
	PlanBlocked = "blocked"
)

// PlanItem 是任务清单里的一条。
type PlanItem struct {
	Title  string `json:"title"`
	Status string `json:"status,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Plan 是复杂任务的任务清单。
//
// 它随 checkpoint 落库（ResumeState.Plan），所以断点续跑后模型能接着走，而不是
// 重新规划、把已经做完的步骤再做一遍。不建独立表：清单的生命周期就是一次 run，
// 跟快照放在一起才不会出现「run 结束了清单还挂着待办」。
type Plan struct {
	Goal  string     `json:"goal,omitempty"`
	Items []PlanItem `json:"items"`
}

// Empty 报告清单是否为空。
func (p *Plan) Empty() bool { return p == nil || len(p.Items) == 0 }

// Normalize 收敛模型给的值：多余空白、大小写、未知状态都改成可读的取值，
// 否则清单里会出现提示词、前端与统计三边都读不懂的状态。
func (p *Plan) Normalize() {
	if p == nil {
		return
	}
	p.Goal = strings.TrimSpace(p.Goal)
	for i := range p.Items {
		it := &p.Items[i]
		it.Title = strings.TrimSpace(it.Title)
		it.Note = strings.TrimSpace(it.Note)
		switch strings.ToLower(it.Status) {
		case PlanDoing:
			it.Status = PlanDoing
		case PlanDone:
			it.Status = PlanDone
		case PlanBlocked:
			it.Status = PlanBlocked
		default:
			it.Status = PlanPending
		}
	}
}

// Render 把清单写成模型可读的文本。
//
// 提示词与工具回执共用这一份：两处各写一遍格式，最后一定会漂移成
// 「提示词里是 [x]，回执里是 done」，模型照着旧格式继续写。
func (p *Plan) Render() string {
	if p.Empty() {
		return ""
	}
	var b strings.Builder
	if goal := strings.TrimSpace(p.Goal); goal != "" {
		b.WriteString("目标：")
		b.WriteString(goal)
		b.WriteString("\n")
	}
	done := 0
	for i, it := range p.Items {
		mark := "[ ]"
		switch it.Status {
		case PlanDone:
			mark = "[x]"
			done++
		case PlanDoing:
			mark = "[~]"
		case PlanBlocked:
			mark = "[!]"
		}
		fmt.Fprintf(&b, "%s %d. %s", mark, i+1, strings.TrimSpace(it.Title))
		if note := strings.TrimSpace(it.Note); note != "" {
			b.WriteString("（")
			b.WriteString(note)
			b.WriteString("）")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "进度：%d/%d 完成。", done, len(p.Items))
	return strings.TrimSpace(b.String())
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
	// AgentSkills 是智能体自带的技能页（agent.skills）：名称 + 路径直接写进系统
	// 提示词，正文仍按需 plugin_read。它为空的含义是「这个智能体没有内置技能」，
	// 而不是「加载失败」——加载失败按没有处理，会话照常能聊。
	AgentSkills []SkillRef
	// Platform 是本轮已确认的平台部署事实（租户、IM、当前用户与空间），
	// 直接注入系统提示词。做成工具的话模型会凭先验抢答「我们用钉钉吧」，
	// 而这类事实必须恒真，不能放在概率路径上。
	Platform PlatformFacts
	// Plan 是本轮开始前已有的任务清单（断点续跑时由快照带回来）。
	// 顶层会话由内置实现注入 plan_write / plan_read 维护它；子 Agent 不建清单。
	Plan *Plan

	// Depth 是嵌套深度：0 表示用户发起的一轮，>0 表示由 agent_dispatch 派生的子 Agent。
	// 实现必须据此限制派发层级，否则子 Agent 可以无限递归。
	Depth int
	// RoundLimit 非零时覆盖实现自身的轮次上限，用于把父任务的剩余预算传给子 Agent。
	RoundLimit int
	// Deadline 非零时作为本次运行的截止时间（父任务预算继承）。
	Deadline time.Time
	// AllowDispatch 表示本轮允许把 agent_dispatch 暴露给模型。
	AllowDispatch bool
	// SubAgentPrompt 是子 Agent 专属的追加提示词（由派发方给出，同样只能追加）。
	SubAgentPrompt string

	// Resume 非空表示从断点继续：直接用快照里的消息序列，不再重建系统提示词与历史。
	Resume *ResumeState
	// Checkpoint 在每轮工具执行完成后被调用，用于把「走到哪了」落库。
	//
	// 只在轮次边界写：一轮里的模型增量没有可重放的价值，工具回执才是真的进展。
	// 实现可以忽略调用错误——写不进去不该打断任务本身。
	Checkpoint func(ResumeState) error
}

// ResumeState 是一次执行的断点快照，足够让实现「接着走」而不是重新规划。
//
// 里面是完整消息序列（含 assistant 的 tool_calls 与 tool 回执），所以续跑时
// 已经执行过的工具不会再跑一遍；计数器一起带着走，避免「续跑」等于把预算重新充满。
type ResumeState struct {
	Messages []llm.Message `json:"messages"`
	// Round 是已经完成的轮数（一次模型调用算一轮）
	Round int `json:"round"`
	// ToolCalls / TotalTokens 是已经消耗掉的预算
	ToolCalls   int `json:"tool_calls"`
	TotalTokens int `json:"total_tokens"`
	// Query 是本次执行的用户输入：续跑时工具裁剪与系统提示词仍要按它来
	Query string `json:"query"`
	// Pending 是已经发给模型、却还没拿到回执的工具调用。
	//
	// 一批调用是并行的，中断可能落在这批的中途：消息序列里 assistant 有 3 个
	// tool_call，tool 消息只有 1 条。直接拿去问模型，它会重新规划、把做过的
	// 副作用再做一遍；所以恢复时要先把这里补齐，再继续循环。
	Pending []PendingCall `json:"pending,omitempty"`
	// Plan 是中断时的任务清单：续跑要接着走已完成的步骤，而不是重新规划。
	Plan *Plan `json:"plan,omitempty"`
}

// PendingCall 是一条等待回执的工具调用。
type PendingCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// ActiveSkill 是会话选中的技能。
type ActiveSkill struct {
	Name string
	Path string
	Doc  string
}

// SkillRef 是智能体自带技能页的索引项：只有名字、说明和读取路径。
//
// 不预读正文——内置技能是「可能用到」的内容，全部塞进提示词会把上下文吃光；
// 这里给出路径，模型判断需要时再用 plugin_read 读全文。
type SkillRef struct {
	Name        string
	Description string
	// Path 相对空间 files/，可直接交给 plugin_read。
	Path string
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
	// Exec 是执行容器自述的运行环境；未上报时为零值，提示词里整段省略。
	Exec ExecFacts
}

// ExecFacts 是执行容器对自身运行环境的自述。
//
// 与 Tenant / IM 的区别：那两条来自部署配置，这一条来自执行体运行时上报。
// 分开建模是因为判断「模型能不能用某个命令」必须看真实镜像，写死一份说明
// 迟早会与镜像漂移，而漂移的表现是「模型反复执行不存在的命令」。
type ExecFacts struct {
	// Image 是镜像标识，仅用于排障对账
	Image string
	// OS 是发行版与架构，例如 debian 12 (linux/arm64)
	OS string
	// Tools 是镜像里确认可用的命令，未列出的按不可用处理
	Tools []ExecTool
	// Writable 是除空间目录外还可写的位置（如 deps 目录）
	Writable []string
	// Network 描述出网形态
	Network string
	// Sources 是安装源，键为生态，值为镜像 ID 或 URL
	Sources map[string]string
	// Persistent 表示容器重建后系统目录的改动是否保留
	Persistent bool
}

// ExecTool 是一个可用命令及其版本。
type ExecTool struct {
	Name    string
	Version string
}

// Empty 表示没有可说的内容，调用方据此整段省略而不是渲染空段落。
func (e ExecFacts) Empty() bool {
	return strings.TrimSpace(e.OS) == "" && len(e.Tools) == 0
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
	// Timeline 是这一轮产出内容的原始顺序：思考段、正文段与工具段交错排列。
	// Steps 只存工具步骤（带参数与回执），时间线里的工具段用 ToolID 指向它；
	// 会话层落库时把两者合成 message.parts，历史回放才能按真实顺序渲染。
	//
	// 可以不填：没有工具调用时位置是固定的（思考在前、正文在后），会话层不会落它。
	Timeline []Segment
	// NeedApproval 表示本次执行中至少有一次工具调用被策略要求审批
	NeedApproval bool
	Provider     string
	Model        string
	Usage        llm.Usage
	// StopReason 是循环的收尾原因：done（正常作答）/ budget（预算用尽，已强制收尾）
	// / stalled（判定再走一步也不会有新进展）。写入 run.budget 与日志，便于事后归因。
	StopReason string
	// Mode 是本次执行实际走到的档位（answer / exec / orchestrate），由实现按
	// 实际调用推断。只用于观测，不改变权限：降级靠提示词与工具面，不靠改名。
	Mode string
}
