// Package tool 实现 Tool 三层模型与执行链（技术方案 §5.6.1、§5.7.7 ToolInvoker）。
//
// 三层：
//
//	Capability  能力接口，描述语义与 IO Schema（见 store.Capability）
//	Tool        能力的实现，内置 Go 函数或 MCP 远端工具
//	Binding     把工具约束到空间/用户，并携带约束（行数、脱敏、是否需审批）
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/spacelog"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// PluginTry 抽象「插件试跑」，避免 tool 包反向依赖 plugin 包。
type PluginTry interface {
	// DryRunJSON 返回可直接放进上下文的 JSON 回执；失败时也要尽量带回诊断。
	DryRunJSON(ctx context.Context, spaceID, slug, userID string, input json.RawMessage) (string, error)
}

// PluginRestarter 抽象「重启常驻插件进程」，同样避免 tool 包反向依赖 plugin 包。
//
// userID 只为审计：重启会打断正在进行的调用，谁按的必须留痕。
type PluginRestarter interface {
	Restart(ctx context.Context, userID, spaceID, slug string) error
}

// PluginCall 是一次插件调用的记账，供 plugin_logs 展示。
//
// 它记录的是「平台看到的调用结果」，不是插件 print 的内容：脚本进程的 stdout
// 是 RPC 帧，stderr 只在崩溃时被读一次，都不是可查询的日志。用户问「刚才那次
// 为什么失败」时，需要的正是这一条：什么时候调的、成没成、耗时多久、错在哪。
type PluginCall struct {
	At time.Time
	// Tool 是模型看到的工具名：脚本插件为 <slug>.<slug>，MCP 为 <slug>.<远端工具>。
	Tool string
	OK   bool
	// Duration 是端到端耗时，含进程冷启动与网络往返。
	Duration time.Duration
	// Err 是失败时的错误摘要（已截断），成功时为空。
	Err string
}

// PluginLogs 提供插件最近的调用记录，由 plugin.Manager 实现。
//
// 记录留在内存、按空间隔离：它回答的是「刚刚发生了什么」，重启即清空；
// 合规审计另有 tool_call / audit_log，不走这里。
type PluginLogs interface {
	// RecentCalls 返回某个插件在该空间最近的调用，按时间倒序，最多 limit 条。
	// slug 为空表示该空间全部插件。
	RecentCalls(ctx context.Context, spaceID, slug string, limit int) []PluginCall
}

// PluginRef 指向另一个空间里的一个插件，用于「子会话借用发起方空间的智能体」。
// SpaceID 是插件来源空间，Slug 是插件 slug；同名 slug 在多个空间可以并存，所以空间不能省。
type PluginRef struct {
	SpaceID string
	Slug    string
}

// PluginBorrower 是可选的跨空间借用能力，由插件管理器实现。
// 借用出来的工具执行时仍落在当前会话空间（文件读写不改空间），只有插件内容与配置来自来源空间。
type PluginBorrower interface {
	ToolsFor(ctx context.Context, spaceID string, refs []PluginRef) ([]Tool, error)
}

// Env 是工具执行时的上下文，决定能看到哪些数据。
type Env struct {
	UserID    string
	SpaceID   string
	SessionID string
	// RunID 用于把 tool_call 归到具体一次执行（§6 tool_call.run_id）
	RunID string
	// SpacePerm 是调用方在该空间的权限，空表示不是成员（策略据此拒绝）
	SpacePerm string
	// WikiSpaceID 是企业 wiki 的宿主空间（系统空间），wiki_* 工具只读写它的 files/wiki/。
	// 为空表示平台还没初始化系统空间：此时 wiki 工具明确报不可用，不退回当前空间，
	// 否则每个空间都会长出一份同名 wiki，知识反而分裂。
	WikiSpaceID string
	// AgentID 是当前会话绑定的智能体（插件 slug）。wiki 在宿主空间里再按它分区：
	// 每个智能体独占 files/wiki/<AgentID>/，工具参数里的 path 一律相对该目录。
	// 为空表示不知道写哪一份，wiki 工具明确报不可用，而不是落到某个公共目录。
	AgentID string

	Store   store.Store
	Storage storage.Storage
	Log     *slog.Logger

	// Plugins 是「当前空间 + 系统空间」合并后的插件目录，为 nil 时 plugin_search / plugin_read 不可用。
	Plugins PluginLoader

	// ExtraPluginRefs 是跨空间借用的插件（子会话继承发起方空间的智能体及其依赖）。
	// 只影响工具与技能的可视范围，不改文件读写空间：文件工具始终用 SpaceID。
	ExtraPluginRefs []PluginRef

	// Search 是搜索 provider 注册表（internal/search），为 nil 时 web_search 不可用。
	// 密钥与出网策略都在实现内部，不经由工具参数传递。
	Search *search.Registry

	// Shell 是命令执行沙箱（internal/sandbox），为 nil 时 shell_exec 不可用。
	// 隔离档位、超时与环境清理都由它的实现决定，工具只负责拼参数与格式化输出。
	Shell sandbox.Runner

	// PluginTry 是插件试跑能力（plugin.Manager.DryRun），为 nil 时 plugin_try 不可用。
	// 「对话开发插件」如果没有试跑，就只能发布上线后才发现配置错。
	PluginTry PluginTry

	// PluginRestart 是常驻进程重启能力（plugin.Manager.Restart），为 nil 时
	// plugin_restart 不可用。常驻进程跑的是启动那一刻的代码，改完草稿只有显式
	// 重启才会加载新版本。
	PluginRestart PluginRestarter

	// PluginLogs 是插件调用记录（plugin.Manager 实现），为 nil 时 plugin_logs 不可用。
	// 只读当前空间：借用插件也按执行落地的空间查，看得到「这次为什么没成」。
	PluginLogs PluginLogs

	// SpaceLog 是持久化的空间运行日志（files/.logs）：shell 与插件执行后各写一条，
	// 按天分文件、历史压缩。为 nil 时不写——测试与不关心留证的调用方不必接它。
	SpaceLog *spacelog.Logger

	// Secrets 是凭证存储，只用于查询「这个凭证配过没有」（名字 + 指纹）。
	// 值是平台内部资产：这里没有 Get 通道，工具也永远不该拿到明文。
	Secrets secret.Store
	// CanWriteSecrets 表示当前用户能否写平台凭证（系统管理员）。
	// 不能写时 secret_request 只给出「转给管理员」的指引，而不是弹一个填了也存不下的表单。
	CanWriteSecrets bool

	// Tasks 是任务派发能力（session 包实现），为 nil 时 task_* 工具不可用。
	// 派发要代建会话、校验空间成员，这些都不该由工具直接碰存储，所以走接口注入。
	Tasks TaskDispatcher

	// Approvals 是审批通道（session 包实现）：只有命中审批动作时才会被用到，
	// 为 nil 时按「拒绝」处理——宁可挡住，也不要在没有出口的情况下静默执行。
	Approvals Approver

	// NetGrant 表示这次调用已获准联网，由 Invoker 按审批结论置位。
	// 工具不自己判断「能不能联网」：判据在 ShellNetPlan，编排在 Invoker，
	// 工具内部等审批会被执行阶段的墙钟（TimeoutFor 或默认 30s）掐断。
	NetGrant bool

	// Replay 是续跑时的结果缓存：key 是 ArgsHash，值是被中断那一轮已经产生的回执。
	//
	// 命中就直接回放，不再执行——副作用不能重来。为空表示这是全新的一轮，
	// 或者上一轮没有值得回放的结果。
	Replay map[string]InvokeResult
}

// TaskDispatchInput 是一次派发的入参。
//
// 按「需求背景 / 已做的事 / 待确认清单 / 确认后用途」分段，而不是让模型把一切塞进
// 一段 brief：子会话的开场要拟人、要能一眼看出在问什么，流水账做不到这一点。
type TaskDispatchInput struct {
	Title string `json:"title"`
	// Background 是需求背景：为什么要这份东西、口径与范围是什么。
	Background string `json:"background"`
	// Done 是已经做到哪一步，可以贴代码或初步结论。
	Done string `json:"done"`
	// Checks 是需要对方逐条确认的点。
	Checks []string `json:"checks"`
	// After 是确认之后打算怎么用；空表示把结论带回发起方会话。
	After string `json:"after"`
	// Brief 是补充说明，保留给没有分段的旧调用。
	Brief string `json:"brief"`
	// Assignee 可以是用户 id、工号或姓名；解析规则由实现决定。
	Assignee string `json:"assignee"`
}

// TaskInfo 是任务的对外视图。
type TaskInfo struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Assignee      string     `json:"assignee"`
	AssigneeName  string     `json:"assignee_name,omitempty"`
	Status        string     `json:"status"`
	WaitingSince  *time.Time `json:"waiting_since,omitempty"`
	DispatchCount int        `json:"dispatch_count"`
	Result        string     `json:"result,omitempty"`
}

// TaskDispatcher 是「主 Agent 派发子任务」的能力边界。
type TaskDispatcher interface {
	Dispatch(ctx context.Context, userID, sessionID string, in TaskDispatchInput) (TaskInfo, error)
	Tasks(ctx context.Context, userID, sessionID string) ([]TaskInfo, error)
	CancelTask(ctx context.Context, userID, taskID string) error
	// RemindTask 催办已有任务：只往原会话补一条提醒，不新建会话、不改被派发人。
	// 模型必须先 task_list 拿到 task_id，否则会退化成「再派发一次」。
	RemindTask(ctx context.Context, userID, taskID string) (TaskInfo, error)
	// ReplyTask 是子会话侧的正式回执：由被派发人确认结论后触发。
	// 平台不再在每轮结束时自动回填，否则「我先确认一下」也会被当成结果。
	ReplyTask(ctx context.Context, userID, sessionID, summary string) (TaskInfo, error)
}

// Tool 是一个可被模型调用的工具。
type Tool struct {
	// Name 是模型可见名，只允许 [a-zA-Z0-9_-]，因此能力名中的点号会转下划线
	Name string
	// Capability 是能力接口名，如 space.list
	Capability  string
	Description string
	Parameters  json.RawMessage
	Risk        int
	// RiskFor 可选：按实参细化「展示用」风险等级（1~4，返回 0 表示判不出来）。
	//
	// 单一工具覆盖多种动作时需要它——shell_exec 从 `ls` 到 `rm -rf` 走的是同一份
	// 工具声明，只按声明展示就只能永远写「写入」。它只影响卡片徽标与历史回看，
	// 不参与审批判定，也不改变空间写权限的判据（那里仍按声明的 Risk）。
	RiskFor func(t Tool, args json.RawMessage) int
	// RiskLabelFor 可选：按实参给出比等级更具体的动作标签（「网络请求」「写文件」
	// 「脚本执行」…），返回空串表示判不出来。
	//
	// 等级只有四档，`curl`、`python x.py`、`echo a > b` 全落在「写入」上，用户
	// 看不出这一步到底动了什么。标签只补这层说明：徽标文案用它，颜色仍按等级，
	// 审批与权限判定完全不读它。
	RiskLabelFor func(t Tool, args json.RawMessage) string
	// SideEffects 声明影响范围：read-only（默认）/ write-workspace（仅当前空间工作区）。
	// 策略层据此区分「空间内写」与「外部写」。
	SideEffects string
	Runtime     string // builtin | mcp | plugin
	ServerID    string
	ServerName  string

	// NetworkWrite 表示这个工具会以非 GET/HEAD 的动词出网（插件清单声明）。
	// 这类调用默认要人工确认：远端可能已经接收并落地了改动。
	NetworkWrite bool
	// NetHosts / NetVerbs 是清单声明允许访问的域名与 HTTP 方法（已排序、去重）。
	//
	// 只给确认卡当文案：审批判定仍然只看 NetworkWrite，调用也不会被这两个字段
	// 再筛一次（白名单在出网代理里强制）。没有它们，卡片只能写「访问外部系统」，
	// 用户看不出要往哪个域名发什么请求——而这正是他决定点不点同意所需的全部信息。
	NetHosts []string
	NetVerbs []string
	// ShellApproval 是「命中就需要确认」的命令名单，只对 shell 类工具有意义。
	// 为空表示不是 shell 工具；内置 shell_exec 用 DefaultApprovalCommands。
	ShellApproval []string
	// NetPlan 是 shell 类工具的网络判据入口：把实参换算成这次的网络策略。
	// 为 nil 表示这个工具不涉及网络准入（MCP / 文件工具）。
	//
	// 放在 Tool 上是因为「要不要网」必须由工具解释参数（命令原文），
	// 而「谁批准、批准后怎么重跑」必须由 Invoker 编排（它持有审批通道与墙钟外的 ctx）。
	NetPlan func(args json.RawMessage) ShellNetPolicy
	// ApprovalLevel 是安装方给这个插件定的审批档位（declared / always / off）。
	// 空值与非法值都按 declared 处理，见 NormalizeApprovalLevel。
	ApprovalLevel string

	// MaxResultBytes 是回填给模型的单次结果上限（默认 4KB）。
	// 读类工具（file_read / plugin_read）可申请更高额度，避免长文档被无谓截断。
	MaxResultBytes int
	// NoReplay 表示「不参与续跑回放」：返回值取决于磁盘内容而不是参数的工具
	// 必须打开它。回放的 key 只有「工具 + 参数」，plugin_try 用同一组参数重跑
	// 会拿到上一次的回执——作者改完代码看到的却还是旧结果。
	NoReplay bool
	// TimeoutFor 可选：按实参给出这次调用的墙钟上限（返回 0 表示用 Invoker 默认）。
	//
	// 与 RiskFor 同理，判据留在工具自己的代码里：只有它知道这次命令是 `ls` 还是
	// `pip install`，两者的合理耗时差两个数量级。Invoker 只负责取用，
	// 再与全局上限取小——工具不能自己把平台的资源边界改宽。
	TimeoutFor func(t Tool, args json.RawMessage) time.Duration
	// Agent 是自带工具的归属智能体 slug（agent.tools 声明的那批）。空表示空间级
	// 插件；非空时只有绑定了该智能体的会话才看得到——按 Env.AgentID 精确匹配过滤。
	Agent string

	Invoke func(ctx context.Context, args json.RawMessage, env Env) (string, error)
}

// Definition 转成模型可识别的函数定义。
func (t Tool) Definition() llm.Tool {
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}
}

// EffectiveRisk 返回这一次调用用于展示的风险等级。
//
// RiskFor 判得出来就用它，判不出来（返回 0 或越界）退回工具声明的 Risk：
// 展示宁可报高——把写操作显示成只读，比把只读显示成写入糟糕得多。
func (t Tool) EffectiveRisk(args json.RawMessage) int {
	if t.RiskFor != nil {
		if r := t.RiskFor(t, args); r >= policy.RiskReadOnly && r <= policy.RiskSensitive {
			return r
		}
	}
	return t.Risk
}

// EffectiveRiskLabel 返回这一次调用的动作标签，判不出来时为空串。
//
// 空串交给前端退回等级文案（只读 / 查询 / 写入 / 高危）：没有更具体的说法时，
// 说「写入」比编一个「可能是脚本」要诚实。
func (t Tool) EffectiveRiskLabel(args json.RawMessage) string {
	if t.RiskLabelFor == nil {
		return ""
	}
	return strings.TrimSpace(t.RiskLabelFor(t, args))
}

// MCPTool 是 MCP Server 暴露的工具（由 mcp.Manager 提供）。
type MCPTool struct {
	ServerID    string
	ServerName  string
	Name        string
	Description string
	InputSchema json.RawMessage
}

// MCPProvider 抽象 MCP 工具来源，避免 tool 包依赖 mcp 的具体实现。
type MCPProvider interface {
	Tools(ctx context.Context) ([]MCPTool, error)
	Call(ctx context.Context, serverID, toolName string, args json.RawMessage) (string, error)
}

// PluginProvider 抽象插件工具来源（docs/插件体系设计.md），
// 避免 tool 包依赖 plugin 的具体实现。
type PluginProvider interface {
	// Tools 返回该空间已安装并启用的插件工具。
	Tools(ctx context.Context, spaceID string) ([]Tool, error)
}

// Registry 组装当前可用的工具集合。
type Registry struct {
	store   store.Store
	mcp     MCPProvider
	plugins PluginProvider
	limits  Limits
	log     *slog.Logger
}

// SetPluginProvider 注入插件来源。插件在装配层晚于 Registry 构造，
// 因此用 setter 而不是构造函数参数，避免把装配顺序变成隐式约束。
func (r *Registry) SetPluginProvider(p PluginProvider) { r.plugins = p }

// Limits 控制单次对话暴露给模型的工具规模（§5.7.2 Tool 结果与上下文保护）。
type Limits struct {
	// MaxTools 超过该数量后按相关性裁剪，避免 Schema 占满上下文
	MaxTools int
	// TopK 裁剪后保留的工具数
	TopK int
}

// NewRegistry 构造注册表。
func NewRegistry(st store.Store, mcp MCPProvider, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		store:  st,
		mcp:    mcp,
		limits: Limits{MaxTools: 50, TopK: 20},
		log:    log,
	}
}

// Load 返回本次对话可用的工具：内置工具 + MCP 工具，并合入绑定约束。
//
// query 为用户本轮输入，仅在工具数量超过上限时用于相关性裁剪。
func (r *Registry) Load(ctx context.Context, env Env, query string) ([]Tool, error) {
	tools := builtinTools()
	pol := r.builtinPolicy(ctx)
	if len(pol.disabled) > 0 {
		tools = withoutTools(tools, pol.disabled)
	}
	for i := range tools {
		// 档位对每个内置工具都生效（没配过的保持默认档），名单只有 shell 有
		if lv, ok := pol.levels[tools[i].Name]; ok {
			tools[i].ApprovalLevel = lv
		}
		if tools[i].Name != "shell_exec" {
			continue
		}
		if pol.hasShellCfg {
			// 安装级配置覆盖默认名单；空切片表示「一条都不确认」。
			tools[i].ShellApproval = pol.shellCmds
		}
	}
	if r.mcp != nil {
		remote, err := r.mcp.Tools(ctx)
		if err != nil {
			// MCP 不可用不应阻断对话，记录后降级为内置工具
			r.log.Warn("加载 MCP 工具失败", "err", err)
		}
		for _, item := range remote {
			tools = append(tools, mcpTool(item, r.mcp))
		}
	}
	if r.plugins != nil {
		extra, err := r.plugins.Tools(ctx, env.SpaceID)
		if err != nil {
			// 插件坏了不能拖垮对话：降级为「本次没有插件工具」，并在审计里留痕。
			r.log.Warn("加载插件工具失败", "space", env.SpaceID, "err", err)
		} else {
			tools = append(tools, extra...)
		}
		// 跨空间借用：子会话继承发起方空间的智能体，工具来自来源空间、执行落在本会话空间。
		// 借用失败同样只降级，不影响会话本身还能聊。
		if len(env.ExtraPluginRefs) > 0 {
			if b, ok := r.plugins.(PluginBorrower); ok {
				borrowed, err := b.ToolsFor(ctx, env.SpaceID, env.ExtraPluginRefs)
				if err != nil {
					r.log.Warn("加载借用插件工具失败", "space", env.SpaceID, "err", err)
				} else {
					tools = append(tools, borrowed...)
				}
			}
		}
	}

	// 智能体自带工具（agent.tools）只给绑定它的会话：按 Env.AgentID 精确过滤，
	// 而不是靠「装没装」判断——它们本来就没有安装记录。过滤放在去重之前，
	// 免得一个用不上的同名工具把空间级的那个挡掉。
	tools = withoutOtherAgents(tools, env.AgentID)

	// 同名工具只能留一个：模型看到两个同名函数会随机选，且策略判定会串味。
	// 内置优先（先出现），插件与 MCP 重名时让路并留日志。
	tools = dedupeByName(tools, r.log)

	if len(tools) > r.limits.MaxTools {
		tools = selectRelevant(tools, query, r.limits.TopK)
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// RuntimeBuiltin 标记平台内置工具：随平台发版、代码受控，不随空间变化。
const RuntimeBuiltin = "builtin"

// builtinToolSlugPrefix 是内置工具在系统空间里的插件 slug 前缀（`tool_file_write`）。
// slug 由 plugin.builtinToolSlug 生成、这里反解，两边必须一致。
const builtinToolSlugPrefix = "tool_"

// builtinOverrides 是内置件的全局策略：停用名单 + 各工具的审批档位 + shell 的命令名单。
type builtinOverrides struct {
	disabled map[string]bool
	// levels 是显式配置过的审批档位（工具名 → declared / always / off）。
	// 没出现过的工具按默认档（declared）走，所以这里只收非空值。
	levels map[string]string
	// shellCmds 与 hasShellCfg 描述命令名单：没显式配过时沿用平台默认。
	shellCmds   []string
	hasShellCfg bool
}

// builtinPolicy 读取内置件的全局策略。
//
// 内置工具的执行体是 Go 代码（见 builtin.go），安装记录只是元数据：不查这一下，
// 系统空间里那个停用开关、审批档位与命令名单就只是标签，模型照样按默认值跑。
// 这些只能在系统空间做，所以只看那一份全局安装；读取失败按「按默认值」处理，
// 不能让配置问题把对话打挂。
func (r *Registry) builtinPolicy(ctx context.Context) builtinOverrides {
	out := builtinOverrides{}
	sys, err := r.store.GetSpaceByType(ctx, systemSpaceType)
	if err != nil || sys.ID == "" {
		return out
	}
	installs, err := r.store.ListPluginInstalls(ctx, sys.ID)
	if err != nil {
		r.log.Warn("读取内置件启停状态失败", "err", err)
		return out
	}
	out.disabled = disabledToolNames(installs)
	out.levels = approvalLevels(installs)
	out.shellCmds, out.hasShellCfg = shellApprovalCommands(installs)
	return out
}

// approvalLevels 从安装记录里摘出显式配置过的档位（工具名 → 档位）。
//
// 档位对每个内置工具都生效，不只是 shell：装了 wiki_write 想让它每次确认，
// 或装了 file_write 觉得空间权限够了不想再弹，都走这一处。没配过的工具不出现
// 在结果里，由调用方按默认档处理。
func approvalLevels(installs []store.PluginInstall) map[string]string {
	levels := map[string]string{}
	for _, in := range installs {
		// 只有内置工具才有对应 slug（`tool_xxx`）；技能、智能体、源没有执行器
		name := strings.TrimPrefix(in.Slug, builtinToolSlugPrefix)
		if name == in.Slug {
			continue
		}
		var cfg map[string]string
		if len(in.Config) == 0 || json.Unmarshal(in.Config, &cfg) != nil {
			continue
		}
		if raw := strings.TrimSpace(cfg[ApprovalPolicyKey]); raw != "" {
			levels[name] = NormalizeApprovalLevel(raw)
		}
	}
	return levels
}

// shellApprovalCommands 从安装记录里摘出 shell 的命令名单。
//
// 返回的 bool 表示「配过」：没配过时调用方沿用平台默认名单，配成空串才是
// 「一条都不确认」——两者不能混为一谈。
func shellApprovalCommands(installs []store.PluginInstall) ([]string, bool) {
	for _, in := range installs {
		if in.Slug != builtinToolSlugPrefix+"shell_exec" {
			continue
		}
		var cfg map[string]string
		if len(in.Config) == 0 || json.Unmarshal(in.Config, &cfg) != nil {
			continue
		}
		if raw, ok := cfg["approval_commands"]; ok {
			return splitCommands(raw), true
		}
	}
	return nil, false
}

// splitCommands 按逗号、中文逗号、空白切分命令名单。
func splitCommands(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', '\n', '\r', '\t', ' ':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// disabledToolNames 把系统空间的安装记录换算成「被停用的内置工具名」。
// 只有 tool_* 的安装对应 Go 侧工具；其它 kind 的停用由各自的加载路径处理。
func disabledToolNames(installs []store.PluginInstall) map[string]bool {
	off := map[string]bool{}
	for _, in := range installs {
		if in.Status != store.PluginInstallDisabled {
			continue
		}
		if name := strings.TrimPrefix(in.Slug, builtinToolSlugPrefix); name != in.Slug {
			off[name] = true
		}
	}
	return off
}

// withoutTools 摘掉被停用的内置工具：停用要真的看不见，不能只是卡片上换个颜色。
func withoutTools(tools []Tool, off map[string]bool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if off[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// withoutOtherAgents 摘掉别的智能体自带的工具。
//
// 自带工具没有安装记录，唯一能判断「该不该给它看」的依据就是会话固化的
// agent_id；这里必须精确匹配，`extends` 的父子也不互相继承——否则一个智能体
// 的能力会沿继承链悄悄扩散。
func withoutOtherAgents(tools []Tool, agentID string) []Tool {
	hasScoped := false
	for _, t := range tools {
		if t.Agent != "" {
			hasScoped = true
			break
		}
	}
	if !hasScoped {
		return tools
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if t.Agent != "" && t.Agent != agentID {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Definitions 批量转换成模型工具定义。
func Definitions(tools []Tool) []llm.Tool {
	out := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Definition())
	}
	return out
}

// Find 按模型可见名查找工具。
func Find(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// dedupeByName 去掉同名工具，保留最先出现的一个（内置 → MCP → 插件）。
func dedupeByName(tools []Tool, log *slog.Logger) []Tool {
	seen := make(map[string]struct{}, len(tools))
	out := tools[:0]
	for _, t := range tools {
		key := strings.ToLower(t.Name)
		if _, ok := seen[key]; ok {
			if log != nil {
				log.Warn("工具名重复，已忽略后出现的一个", "tool", t.Name, "runtime", t.Runtime)
			}
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}

func mcpTool(item MCPTool, provider MCPProvider) Tool {
	// 模型函数名不允许点号与斜杠：统一成 server_tool
	name := sanitizeName(item.ServerName + "_" + item.Name)
	schema := item.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	serverID := item.ServerID
	toolName := item.Name

	return Tool{
		Name:        name,
		Capability:  item.Name,
		Description: fmt.Sprintf("[MCP:%s] %s", item.ServerName, item.Description),
		Parameters:  schema,
		// 远端工具按「只读有成本」处理：数据范围与鉴权在远端 server 上，
		// 注册/安装这个动作本身就是授权，策略层不再逐次核对绑定。
		//
		// 这条路径来自 mcp_server 注册表（旧注册方式），没有插件清单可声明
		// tools.writes，所以无法判断远端是否会改数据，按只读处理、不弹确认。
		// 需要审批的 MCP 请走 kind=mcp 插件并在清单里声明 tools.writes。
		Risk:       policy.RiskReadCost,
		Runtime:    "mcp",
		ServerID:   serverID,
		ServerName: item.ServerName,
		Invoke: func(ctx context.Context, args json.RawMessage, _ Env) (string, error) {
			return provider.Call(ctx, serverID, toolName, args)
		},
	}
}

func sanitizeName(name string) string {
	replacer := strings.NewReplacer(".", "_", "/", "_", ":", "_", " ", "_")
	out := replacer.Replace(name)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// selectRelevant 在工具过多时按关键词打分裁剪，保留最高分的 TopK。
//
// 复杂场景可替换为向量检索（§5.4 MCP 工具清单），当前先用确定性关键词匹配，
// 避免在工具选择这一高频路径上增加一次模型调用。
func selectRelevant(tools []Tool, query string, topK int) []Tool {
	if topK <= 0 || topK >= len(tools) {
		return tools
	}
	terms := tokenize(query)
	scored := make([]struct {
		tool  Tool
		score int
		index int
	}, 0, len(tools))

	for i, t := range tools {
		score := 0
		haystack := strings.ToLower(t.Name + " " + t.Capability + " " + t.Description)
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				score += 2
			}
		}
		// 内置只读工具常驻，避免裁剪掉基础能力
		if t.Runtime == "builtin" && t.Risk == policy.RiskReadOnly {
			score += 1
		}
		scored = append(scored, struct {
			tool  Tool
			score int
			index int
		}{t, score, i})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].index < scored[j].index
	})

	out := make([]Tool, 0, topK)
	for i := 0; i < topK && i < len(scored); i++ {
		out = append(out, scored[i].tool)
	}
	return out
}

func tokenize(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return r == ' ' || r == ',' || r == '，' || r == '。' || r == '?' || r == '？' || r == '/' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
