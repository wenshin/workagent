package store

import (
	"context"
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
	SkillPath     string
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
	Parts     []byte
	CreatedAt time.Time
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

	CreateSession(ctx context.Context, spaceID, userID, title, dir string) (Session, error)
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

	CreateMessage(ctx context.Context, sessionID, role, content string) (Message, error)
	// CreateAssistantMessage 写入带思考过程与工具轨迹的助手消息。
	CreateAssistantMessage(ctx context.Context, sessionID, content, reasoning string, parts []byte) (Message, error)
	ListMessages(ctx context.Context, sessionID string) ([]Message, error)
	CountMessages(ctx context.Context, sessionID string) (int, error)

	// ---- Tool 三层模型（§5.6.1）----
	UpsertCapability(ctx context.Context, c Capability) error
	UpsertTool(ctx context.Context, t ToolRecord) error
	// ListToolBindings 返回空间维度与用户维度的有效绑定。
	ListToolBindings(ctx context.Context, spaceID, userID string) ([]ToolBinding, error)
	UpsertToolBinding(ctx context.Context, b ToolBinding) (ToolBinding, error)

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
	// ListRunSteps 按 seq 返回一次执行的全部 checkpoint，供恢复时重放。
	ListRunSteps(ctx context.Context, runID string) ([]RunStep, error)

	// ---- 上下文与记忆（§5.7.2 / §5.7.4）----
	GetSessionSummary(ctx context.Context, sessionID string) (SessionSummary, bool, error)
	SaveSessionSummary(ctx context.Context, s SessionSummary) error
	SaveMessageEmbedding(ctx context.Context, r EmbeddingRecord) error
	ListMessageEmbeddings(ctx context.Context, sessionID string, limit int) ([]EmbeddingRecord, error)
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
