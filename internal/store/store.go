package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("not found")

// User 是平台用户，身份唯一键是 (Provider, ProviderUID)（§5.1.1）。
type User struct {
	ID          string
	Provider    string // dingtalk | wecom | feishu
	ProviderUID string
	Name        string
	Avatar      string
	Mobile      string
	Email       string
	JobNumber   string
	CorpName    string
	// ProfileWarning 是最近一次通讯录同步失败的用户可读原因，成功时为 NULL。
	ProfileWarning string
	Status         string
	CreatedAt      time.Time
}

// IdentityProfile 是身份接入层（internal/identity）产出的统一用户资料。
// 同一 (Provider, ProviderUID) 对应同一个 User；通讯录字段只在有值时覆盖。
type IdentityProfile struct {
	Provider    string
	ProviderUID string
	UnionID     string
	OpenID      string
	EmployeeID  string
	Name        string
	Avatar      string
	Mobile      string
	Email       string
	JobNumber   string
	Title       string
	CorpName    string
	// ContactError 是通讯录同步失败原因；为空表示本次同步成功（会清空历史记录）。
	ContactError string
	Raw          []byte // 通讯录原始返回，落 user_identity.raw
}

// Space 是空间（个人空间 / 共享空间 / 系统空间），parent_id 为空表示根空间。
type Space struct {
	ID               string
	OwnerID          string
	Name             string
	Type             string // personal | shared | system
	ParentID         string
	Path             string
	Depth            int
	IsSystemReserved bool
	Status           string
	Perm             string // 当前用户在该空间的权限
	CreatedAt        time.Time
}

// SpaceInput 是创建空间的入参，ID 与 Path 由业务层生成。
type SpaceInput struct {
	ID       string
	OwnerID  string
	Name     string
	Type     string
	ParentID string
	Path     string
	Depth    int
	// IsSystemReserved 为 true 表示系统内置空间：不允许重命名、删除。
	IsSystemReserved bool
}

// Session 是聊天会话，直接归属于某个空间。
type Session struct {
	ID      string
	SpaceID string
	// Dir 是会话所属目录（相对 space/files/ 的路径），为空表示直接挂在空间根
	Dir       string
	Title     string
	TitleAuto bool // true 表示标题可被首条消息改写
	// SkillPath 是会话绑定的技能（相对空间 files/ 的 SKILL.md 路径），空表示未绑定
	SkillPath string
	// AgentID 是会话绑定的智能体（插件 slug），空表示未绑定
	AgentID string
	// AgentSpaceID 是智能体所属空间；空表示与会话同空间。
	// 派发出来的子会话会指向发起方空间，按 slug 解析时不能只看会话自己的空间。
	AgentSpaceID string
	// AgentVersion 是创建时绑定的版本，用于发现配置漂移
	AgentVersion string
	// AgentPrompt 是创建时固化的追加提示词，管理员事后改配置不影响已开会话
	AgentPrompt   string
	CreatedBy     string
	LastMessageAt *time.Time
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Message 是会话中的一条消息。
type Message struct {
	ID        string
	SessionID string
	Role      string // user | assistant | system
	Content   string
	// Reasoning 是模型的思考过程，仅 assistant 消息可能有值
	Reasoning string
	// Parts 是结构化的执行轨迹（思考、工具调用），JSON 数组；为空表示纯文本消息
	Parts []byte
	// Stats 是这条助手消息的用量与耗时，仅 assistant 消息有值（历史消息为 0）
	Stats MessageStats
	// Streaming 表示这条消息还在生成中：首个增量到达时占位，结束时整段覆盖（§15.12.2）
	Streaming bool
	// Interrupted 表示这一轮被取消或中断，内容只覆盖到中断点
	Interrupted bool
	// Source 是消息来源：user 用户输入 / task_result 子任务回执 / external 外部回信
	Source    string
	CreatedAt time.Time
}

// TaskStatus 是派发任务的状态（§15.12.4）。没有「过期」：只能由用户取消。
type TaskStatus string

const (
	TaskPending         TaskStatus = "pending"
	TaskRunning         TaskStatus = "running"
	TaskWaitingExternal TaskStatus = "waiting_external"
	TaskSucceeded       TaskStatus = "succeeded"
	TaskFailed          TaskStatus = "failed"
	TaskCancelled       TaskStatus = "cancelled"
)

// Task 是一次派发：父会话把一件事交给子会话（归属被咨询人）去办。
type Task struct {
	ID              string
	SpaceID         string
	ParentSessionID string
	ChildSessionID  string
	Title           string
	Brief           string
	Assignee        string
	Status          TaskStatus
	WaitingSince    *time.Time
	DispatchCount   int
	Result          string
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Share 是一条用户授权的临时只读链接（§15.12.3）。库里只存 token 的 sha256。
type Share struct {
	ID        string
	SessionID string
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	ViewCount int
	CreatedBy string
	CreatedAt time.Time
}

// Expired 判断链接是否已不可用。
func (s Share) Expired(now time.Time) bool {
	return now.After(s.ExpiresAt) || s.RevokedAt != nil
}

// MessageStats 是产生一条助手消息的那一轮 run 的 token 消耗与总墙钟耗时（含工具轮次）。
// 只做展示与成本归因，不参与上下文预算计算。
type MessageStats struct {
	InTokens   int
	OutTokens  int
	DurationMS int
}

// Capability 是平台能力接口（技术方案 §5.6.1），只描述语义与 IO Schema。
type Capability struct {
	Name        string
	Description string
	InputSchema []byte
	RiskLevel   int // 1 只读无副作用 / 2 只读有成本 / 3 写 / 4 高敏感
	Idempotent  bool
	SideEffects string
}

// ToolRecord 是能力的实现（§5.6.1 第二层）。
type ToolRecord struct {
	Name        string
	Capability  string
	Runtime     string // builtin | mcp
	RiskLevel   int
	MCPServerID string
}

// ToolBinding 把工具约束在空间/用户上，并携带约束（§5.6.1 第三层）。
type ToolBinding struct {
	ID          string
	ToolName    string
	Runtime     string
	MCPServerID string
	SpaceID     string
	UserID      string
	Constraints []byte
	RiskLevel   int
}

// MCPServer 是注册的 MCP Server（§5.3.1）。
type MCPServer struct {
	ID         string
	Name       string
	Transport  string
	Endpoint   string
	AuthHeader string
	SecretRef  string
	Visibility string
	Health     string
	ToolsCache []byte
	LastSyncAt *time.Time
}

// AuditEntry 是一条审计记录（§8.2）。
type AuditEntry struct {
	Actor   string
	Action  string
	Object  string
	Result  string
	Detail  []byte
	TraceID string
}

// RunStatus 是一次执行的结局（§5.7.5）。
const (
	RunRunning         = "running"
	RunWaitingApproval = "waiting_approval"
	RunSucceeded       = "succeeded"
	RunFailed          = "failed"
	RunCancelled       = "cancelled"
)

// Run 是一次执行（§6 run），round 内所有模型与工具调用都归属它。
type Run struct {
	ID        string
	SessionID string
	Status    string
	Budget    []byte
	Usage     []byte
	Provider  string
	Model     string
	Error     string
	StartedAt time.Time
	EndedAt   *time.Time
}

// RunStep 是一条 checkpoint（§6 run_step / §5.7.5）。
type RunStep struct {
	ID        string
	RunID     string
	Seq       int
	Type      string
	Payload   []byte
	CreatedAt time.Time
}

// ToolCallRecord 是一次工具调用记录（§6 tool_call）。
type ToolCallRecord struct {
	SessionID string
	RunID     string
	ToolName  string
	Args      []byte
	Status    string
	LatencyMS int
	RiskLevel int
}

// AuthSession 是登录会话（注意：与聊天会话 session 表不是一回事）。
type AuthSession struct {
	TokenHash string
	UserID    string
	ExpiresAt time.Time
}

// Store 是持久化接口，业务层只依赖它。
type Store interface {
	Ping(ctx context.Context) error

	UpsertUserByIdentity(ctx context.Context, p IdentityProfile) (User, error)
	GetUser(ctx context.Context, id string) (User, error)
	// SearchUsers 按姓名/工号/邮箱模糊匹配，供派发任务时把人名解析成用户。
	SearchUsers(ctx context.Context, keyword string, limit int) ([]User, error)
	// FindUserByJobNumber 按工号查用户，用于按配置识别系统管理员。
	FindUserByJobNumber(ctx context.Context, jobNumber string) (User, error)

	CreateAuthSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error
	GetAuthSession(ctx context.Context, tokenHash string) (AuthSession, error)
	DeleteAuthSession(ctx context.Context, tokenHash string) error

	// EnsurePersonalSpace 保证用户拥有个人空间（含一个「新会话」），返回可见空间及是否本次创建。
	EnsurePersonalSpace(ctx context.Context, userID string) ([]Space, bool, error)
	ListSpaces(ctx context.Context, userID string) ([]Space, error)

	CreateSpace(ctx context.Context, in SpaceInput) (Space, error)
	GetSpace(ctx context.Context, id string) (Space, error)
	// GetSpaceByType 按类型取第一个未删除空间（系统空间全局唯一）。
	GetSpaceByType(ctx context.Context, spaceType string) (Space, error)
	// EnsureSpaceMember 幂等地把用户加入空间，已有记录不覆盖（权限升级走专门流程）。
	EnsureSpaceMember(ctx context.Context, spaceID, userID, perm string) error
	// SpacePerm 返回用户在某空间的权限，无权限返回空字符串。
	SpacePerm(ctx context.Context, spaceID, userID string) (string, error)
	RenameSpace(ctx context.Context, id, name string) error
	// DeleteSpaceTree 软删整棵子树，返回被删除的空间列表（业务层据此移入回收站）。
	DeleteSpaceTree(ctx context.Context, id string) ([]Space, error)

	// CreateSession 创建会话；agent 参数是创建时固化的智能体绑定（id / 版本 / 追加提示词）。
	// 标题按「自动」处理：用户发第一条消息时会被改写。智能体默认属于本空间。
	CreateSession(ctx context.Context, spaceID, userID, title, dir, agentID, agentVersion, agentPrompt string) (Session, error)
	// CreateSessionTitled 创建会话并指定标题是否自动改写。
	// 派发出来的任务会话传 false：标题是任务内容，不能被对方的第一句「好的」覆盖。
	// agentSpaceID 为空表示智能体与会话同空间；派发时传发起方空间。
	CreateSessionTitled(ctx context.Context, spaceID, userID, title, dir, agentSpaceID, agentID, agentVersion, agentPrompt string, titleAuto bool) (Session, error)
	ListSessions(ctx context.Context, spaceID string) ([]Session, error)
	// ListUserSessions 返回用户在可见空间下的全部会话（左侧树一次性取数）。
	ListUserSessions(ctx context.Context, userID string) ([]Session, error)
	GetSession(ctx context.Context, id string) (Session, error)
	// RenameSession 修改标题；auto 为 true 表示仍允许被首条消息覆盖。
	RenameSession(ctx context.Context, id, title string, auto bool) error
	// DeleteSession 软删会话，历史消息保留。
	DeleteSession(ctx context.Context, id string) error
	// TouchSession 更新最近活动时间。
	TouchSession(ctx context.Context, id string) error
	// ClearSessionsDir 把该目录（含子目录）下的会话移回空间根，用于删除目录。
	ClearSessionsDir(ctx context.Context, spaceID, dir string) error
	// MoveSessionsDir 目录改名后同步会话的 dir 前缀。
	MoveSessionsDir(ctx context.Context, spaceID, from, to string) error
	// UpdateSessionSkill 绑定/清除会话技能（相对空间 files/ 的 SKILL.md 路径，空串清除）。
	UpdateSessionSkill(ctx context.Context, id, path string) error
	// UpdateSessionAgent 重新绑定会话的智能体，按当前配置重新固化提示词。
	// agentSpaceID 为空表示智能体与会话同空间。
	UpdateSessionAgent(ctx context.Context, id, agentSpaceID, agentID, agentVersion, agentPrompt string) error

	CreateMessage(ctx context.Context, sessionID, role, content string) (Message, error)
	// CreateMessageFrom 写入一条带来源的消息（task_result / external 需要区分渲染与提示词包装）。
	CreateMessageFrom(ctx context.Context, sessionID, role, content, source string) (Message, error)
	// CreateStreamingMessage 先占位一条 streaming 的助手消息，首个增量到达时调用。
	CreateStreamingMessage(ctx context.Context, sessionID string) (Message, error)
	// FinishMessage 用最终正文覆盖占位消息；interrupted 标记这一轮是否被中断。
	FinishMessage(ctx context.Context, id, content, reasoning string, parts []byte, stats MessageStats, interrupted bool) (Message, error)
	// CreateAssistantMessage 写入带思考过程与工具轨迹的助手消息，并记录这一轮的用量与耗时。
	CreateAssistantMessage(ctx context.Context, sessionID, content, reasoning string, parts []byte, stats MessageStats) (Message, error)
	ListMessages(ctx context.Context, sessionID string) ([]Message, error)
	CountMessages(ctx context.Context, sessionID string) (int, error)

	// ---- Tool 三层模型（§5.6.1）----
	UpsertCapability(ctx context.Context, c Capability) error
	UpsertTool(ctx context.Context, t ToolRecord) error
	// ListToolBindings 返回空间维度与用户维度的有效绑定。
	ListToolBindings(ctx context.Context, spaceID, userID string) ([]ToolBinding, error)
	UpsertToolBinding(ctx context.Context, b ToolBinding) (ToolBinding, error)
	// DeletePluginToolRecords 卸载插件时清掉它注册的能力/工具/绑定（审计与历史不计）。
	DeletePluginToolRecords(ctx context.Context, slug string) error

	// ---- MCP（§5.3.1 / §5.4）----
	ListMCPServers(ctx context.Context) ([]MCPServer, error)
	UpsertMCPServer(ctx context.Context, s MCPServer) (MCPServer, error)
	UpdateMCPToolsCache(ctx context.Context, id string, cache []byte) error

	// ---- 审计与调用记录（§8.2 / §6）----
	AppendAuditLog(ctx context.Context, e AuditEntry) error
	RecordToolCall(ctx context.Context, r ToolCallRecord) error

	// ---- Run 与 checkpoint（§5.7.5 / §6）----
	// CreateRun 开启一次执行，初始状态为 running。
	CreateRun(ctx context.Context, r Run) (Run, error)
	// AppendRunStep 追加一条 checkpoint，seq 由调用方保证递增。
	AppendRunStep(ctx context.Context, s RunStep) (RunStep, error)
	// FinishRun 落最终状态、用量与错误信息。
	FinishRun(ctx context.Context, r Run) error
	// GetRun 读一次执行的状态：前端刷新后据此判断「还在不在跑」。
	GetRun(ctx context.Context, id string) (Run, error)
	// GetActiveRun 返回会话当前活跃的 run（running / waiting_approval），没有则 ok=false。
	//
	// staleAfter > 0 时顺带按心跳判活：心跳停了超过这个时长的 run 不再算「在跑」，
	// 否则进程崩溃后前端会一直以为任务还在，点停止也没有任何人来收尾。
	GetActiveRun(ctx context.Context, sessionID string, staleAfter time.Duration) (Run, bool, error)
	// ListRunSteps 按 seq 返回一次执行的全部 checkpoint，供恢复时重放。
	ListRunSteps(ctx context.Context, runID string) ([]RunStep, error)

	// TouchRun 刷新一次执行的心跳：跑它的进程还活着。
	//
	// 这是「在跑」的唯一凭据。用应用侧刷新而不是 started_at，是因为长任务本身就该跑很久，
	// 只有「心跳停了」才说明执行没了。
	TouchRun(ctx context.Context, runID string) error
	// SaveRunResume 覆盖一次执行的断点快照（每轮一次，只有最新值有意义）。
	SaveRunResume(ctx context.Context, runID string, payload []byte) error
	// GetRunResume 取一次执行的断点快照；没有则 ok=false。
	GetRunResume(ctx context.Context, runID string) ([]byte, bool, error)
	// DeleteRunResume 丢弃断点：用户主动停止的那一轮不该再被「继续」。
	DeleteRunResume(ctx context.Context, runID string) error
	// ResumableRun 返回会话里最近一次留着断点的 run；没有则 ok=false。
	ResumableRun(ctx context.Context, sessionID string) (Run, bool, error)
	// SweepStaleRuns 把心跳停了却还挂着 running 的执行置为 failed，返回收尾条数。
	//
	// 顺带把它们的流式占位消息标成中断——否则刷新后会一直显示「生成中」，
	// 而那条消息永远不会有人来写完。
	SweepStaleRuns(ctx context.Context, staleAfter time.Duration, reason string) (int, error)
	// AbandonRun 收尾一条「没人跑」的执行：置为 failed 并结算它的流式占位消息。
	//
	// 与 SweepStaleRuns 的区别只在触发时机与判据：清扫靠心跳超时批量做，
	// 这条给 active-run 用——库里还挂着 running、进程内却没有执行体时当场收尾，
	// 不必等 idle 超时（进程被 kill、迁移给历史行补默认心跳都会造出这种假活）。
	// staleAfter > 0 时按心跳再挡一道：心跳还在跳说明执行在别处活着，本进程无权替它收尾。
	// 断点（run_resume）不动：用户仍可以在会话里点「继续」。
	AbandonRun(ctx context.Context, runID, sessionID, reason string, staleAfter time.Duration) error

	// ---- 任务派发（§15.12.4）----
	CreateTask(ctx context.Context, t Task) (Task, error)
	GetTask(ctx context.Context, id string) (Task, error)
	// GetTaskByChild 按子会话反查任务：子会话跑完时要据此回填父会话。
	GetTaskByChild(ctx context.Context, childSessionID string) (Task, error)
	// ListTasks 返回父会话派发出的任务，按创建时间倒序。
	ListTasks(ctx context.Context, parentSessionID string) ([]Task, error)
	// UpdateTask 覆盖可变字段：状态、子会话、等待起点、催办次数、结果。
	UpdateTask(ctx context.Context, t Task) error

	// ---- 排障只读链接（§15.12.3）----
	CreateShare(ctx context.Context, s Share) (Share, error)
	// GetShareByHash 按 token 的 sha256 取链接，连带返回会话（公开访问只有这一条查询）。
	GetShareByHash(ctx context.Context, hash string) (Share, error)
	RevokeShare(ctx context.Context, id string) error
	ListShares(ctx context.Context, sessionID string) ([]Share, error)
	TouchShareView(ctx context.Context, id string) error

	// ---- 上下文与记忆（§5.7.2 / §5.7.4）----
	GetSessionSummary(ctx context.Context, sessionID string) (SessionSummary, bool, error)
	SaveSessionSummary(ctx context.Context, s SessionSummary) error
	SaveMessageEmbedding(ctx context.Context, r EmbeddingRecord) error
	ListMessageEmbeddings(ctx context.Context, sessionID string, limit int) ([]EmbeddingRecord, error)

	// ---- 插件体系（docs/插件体系设计.md）----
	// 内容以文件为准、状态以 DB 为准：草稿不落库，发布才写 plugin / plugin_version。
	UpsertPlugin(ctx context.Context, p Plugin) (Plugin, error)
	GetPluginBySlug(ctx context.Context, slug string) (Plugin, error)
	ListPlugins(ctx context.Context) ([]Plugin, error)
	AddPluginVersion(ctx context.Context, v PluginVersion, latest string) (PluginVersion, error)
	GetPluginVersion(ctx context.Context, pluginID, version string) (PluginVersion, error)
	ListPluginVersions(ctx context.Context, pluginID string) ([]PluginVersion, error)
	UpsertPluginInstall(ctx context.Context, in PluginInstall) (PluginInstall, error)
	ListPluginInstalls(ctx context.Context, spaceID string) ([]PluginInstall, error)
	// ListAllPluginInstalls 返回全部空间的安装记录（管理视角）：判断某个凭证
	// 「是否还有插件在用」必须跨空间，凭证本身是平台级、不分空间的。
	ListAllPluginInstalls(ctx context.Context) ([]PluginInstall, error)
	// GetPluginInstall 按「空间 + slug」取安装记录，用于配置读写。
	GetPluginInstall(ctx context.Context, spaceID, slug string) (PluginInstall, error)
	// UpdatePluginInstallConfig 覆盖安装级配置；只改配置，不动版本与状态。
	UpdatePluginInstallConfig(ctx context.Context, spaceID, pluginID string, cfg json.RawMessage) error
	DeletePluginInstall(ctx context.Context, spaceID, pluginID string) error
	// DeletePlugin 删掉插件及其版本与安装记录（外键级联），用于下线已合并的内置件。
	DeletePlugin(ctx context.Context, id string) error
	// ---- MCP 应用名归属：一个应用名一把签名密钥，明文只给创建者 ----
	// GetMCPApp 按应用名取归属；没有记录返回 ErrNotFound。
	GetMCPApp(ctx context.Context, app string) (MCPApp, error)
	// ClaimMCPApp 幂等抢占应用名：created=true 表示这次抢到的，
	// false 表示别人已经占着（返回已有归属，调用方据此提示找创建人）。
	ClaimMCPApp(ctx context.Context, app, userID, spaceID string) (MCPApp, bool, error)
	// ListMCPApps 返回全部归属记录，按应用名排序。
	// 只给管理员的应用密钥页用：它回答「谁创建的」，密钥本身在凭证存储里。
	ListMCPApps(ctx context.Context) ([]MCPApp, error)
	// ---- 插件候选：空间本地插件「已启用、未发布」的市场元数据 ----
	UpsertPluginCandidate(ctx context.Context, c PluginCandidate) (PluginCandidate, error)
	GetPluginCandidate(ctx context.Context, spaceID, slug string) (PluginCandidate, error)
	// ListPluginCandidates 返回全部候选（市场「待发布」分类）。
	ListPluginCandidates(ctx context.Context) ([]PluginCandidate, error)
	// ListPluginCandidatesBySpace 返回某空间的候选，用于本地插件启用状态。
	ListPluginCandidatesBySpace(ctx context.Context, spaceID string) ([]PluginCandidate, error)
	// MarkPluginCandidatePublished 记录候选已发布到的 plugin；不改本地启用状态。
	MarkPluginCandidatePublished(ctx context.Context, spaceID, slug, pluginID string) error
	DeletePluginCandidate(ctx context.Context, spaceID, slug string) error
	// ListAllSpaces 返回全部未删除空间，供启动时的存量迁移与默认安装集使用。
	ListAllSpaces(ctx context.Context) ([]Space, error)
	// CountSessionsUsingSkill 统计绑定了某个技能路径的会话，卸载前做引用检查。
	CountSessionsUsingSkill(ctx context.Context, spaceID, prefix string) (int, error)
	// MarkPluginBootstrapped 标记空间已完成默认安装集初始化；
	// 返回 false 表示之前已经标记过（卸载后不再追溯安装）。
	MarkPluginBootstrapped(ctx context.Context, spaceID string) (bool, error)
	// SavePluginScan 记录一次发布扫描结论。同一 plugin + version + rules_version
	// 覆盖写：规则没换时重扫只保留最新一次。
	SavePluginScan(ctx context.Context, s PluginScan) (PluginScan, error)
	// GetPluginScan 取某版本最近一次的扫描结论，按扫描时间倒序。
	GetPluginScan(ctx context.Context, pluginID, version string) (PluginScan, error)
}

// SessionSummary 是一段历史消息的滚动摘要。
type SessionSummary struct {
	SessionID     string
	UptoMessageID string
	Content       string
	MessageCount  int
	Model         string
	UpdatedAt     time.Time
}

// EmbeddingRecord 是消息向量与其原文，用于按需检索。
type EmbeddingRecord struct {
	MessageID string
	SessionID string
	Model     string
	Vector    []float32
	Role      string
	Content   string
}

// 插件状态。安装态与发布态分开：停用发布件不影响已安装的运行。
const (
	PluginStatusEnabled     = "enabled"
	PluginStatusDisabled    = "disabled"
	PluginStatusDeprecated  = "deprecated"
	PluginInstallEnabled    = "enabled"
	PluginInstallDisabled   = "disabled"
	PluginVisibilityPublic  = "public"
	PluginVisibilityPrivate = "private"

	// 插件来源：组织内自建 / 平台内置 / 外部。
	PluginOriginInternal = "internal"
	PluginOriginSystem   = "system"
	PluginOriginExternal = "external"

	// 本地插件候选状态：只表示本空间是否启用，发布态由 plugin_id 是否为空判断。
	PluginCandidateEnabled  = "enabled"
	PluginCandidateDisabled = "disabled"
)

// Plugin 是已发布插件的元数据（不含内容）。草稿不落库。
type Plugin struct {
	ID            string
	Slug          string
	Kind          string
	Name          string
	Description   string
	Visibility    string
	Status        string
	LatestVersion string
	OwnerSpaceID  string
	// Origin 区分组织内自建 / 平台内置 / 外部，市场分类依赖它。
	Origin     string
	Extensible bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PluginCandidate 是空间本地插件的「启用 + 待发布」记录。
//
// 内容仍以空间文件为准；这里只存市场展示与状态需要的元数据。发布后
// PluginID 指向全局 plugin，但本地插件仍可继续编辑，hash 变化即「有新版本」。
type PluginCandidate struct {
	ID            string
	SpaceID       string
	SpaceName     string
	Slug          string
	Kind          string
	Name          string
	Description   string
	AuthorID      string
	AuthorName    string
	ManifestHash  string
	Risk          int
	Capabilities  json.RawMessage
	Status        string
	PluginID      string
	PublishedName string
	EnabledAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PluginVersion 是不可变发布快照。Manifest 是发布时的清单原文，
// name / description 是快照，不是第二处可编辑的定义。
type PluginVersion struct {
	PluginID     string
	Version      string
	Manifest     []byte
	ArtifactPath string
	SHA256       string
	Capabilities []byte
	Risk         int
	Notes        string
	PublishedBy  string
	PublishedAt  time.Time
}

// PluginInstall 是空间级安装记录：安装即授权，卸载不删审计。
type PluginInstall struct {
	SpaceID          string
	PluginID         string
	Slug             string
	Version          string
	Status           string
	Risk             int
	InstalledBy      string
	InstalledAt      time.Time
	DismissedVersion string
	// Config 是空间级配置（连接地址、鉴权头、凭证名），空对象表示沿用清单。
	Config json.RawMessage
}

// MCPApp 是 MCP 应用名（清单里的 auth.app）的归属。
//
// 一个应用名只有一把签名密钥，密钥按应用名存在凭证存储里；归属记录回答的是
// 「这把密钥是谁生成的」——只有创建者能从会话界面取回明文，其他人只能看到
// 创建者姓名并线下要。应用名是全局的，不随空间复制。
type MCPApp struct {
	App          string
	OwnerUserID  string
	OwnerSpaceID string
	CreatedAt    time.Time
}

// PluginScan 是一次发布扫描的结论（docs/插件安全基线.md §1.5）。
//
// 它是元数据，不是发布件的一部分：artifact_sha256 与发布快照无关，
// 所以规则升级后可以重扫同一版本，结论会变、产物不会变。
type PluginScan struct {
	ID           string
	PluginID     string
	Version      string
	SHA256       string
	RulesVersion string
	Scanner      string
	// Status 取值见 securityscan：passed / blocked / unscanned / error。
	Status      string
	MaxSeverity string
	Deps        int
	Lockfile    string
	// Findings 是命中的原始 JSON 数组，读取方自己按 securityscan.Finding 解码。
	Findings      json.RawMessage
	BlockedReason string
	DepError      string
	ScannedBy     string
	ScannedAt     time.Time
}
