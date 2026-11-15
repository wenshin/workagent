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
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// PluginTry 抽象「插件试跑」，避免 tool 包反向依赖 plugin 包。
type PluginTry interface {
	// DryRunJSON 返回可直接放进上下文的 JSON 回执；失败时也要尽量带回诊断。
	DryRunJSON(ctx context.Context, spaceID, slug, userID string, input json.RawMessage) (string, error)
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

	Store   store.Store
	Storage storage.Storage
	Log     *slog.Logger

	// Sources 是「当前空间 + 系统空间」合并后的能力源视图，为 nil 时相关工具不可用。
	Sources SourceLoader

	// Search 是搜索 provider 注册表（internal/search），为 nil 时 web_search 不可用。
	// 密钥与出网策略都在实现内部，不经由工具参数传递。
	Search *search.Registry

	// Shell 是命令执行沙箱（internal/sandbox），为 nil 时 shell_exec 不可用。
	// 隔离档位、超时与环境清理都由它的实现决定，工具只负责拼参数与格式化输出。
	Shell sandbox.Runner

	// Plugins 是插件试跑能力（plugin.Manager.DryRun），为 nil 时 plugin_try 不可用。
	// 「对话开发插件」如果没有试跑，就只能发布上线后才发现配置错。
	Plugins PluginTry

	// Secrets 是凭证存储，只用于查询「这个凭证配过没有」（名字 + 指纹）。
	// 值是平台内部资产：这里没有 Get 通道，工具也永远不该拿到明文。
	Secrets secret.Store
	// CanWriteSecrets 表示当前用户能否写平台凭证（系统管理员）。
	// 不能写时 secret_request 只给出「转给管理员」的指引，而不是弹一个填了也存不下的表单。
	CanWriteSecrets bool

	// Tasks 是任务派发能力（session 包实现），为 nil 时 task_* 工具不可用。
	// 派发要代建会话、校验空间成员，这些都不该由工具直接碰存储，所以走接口注入。
	Tasks TaskDispatcher
}

// TaskDispatchInput 是一次派发的入参。
type TaskDispatchInput struct {
	Title string `json:"title"`
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
}

// Tool 是一个可被模型调用的工具。
type Tool struct {
	// Name 是模型可见名，只允许 [a-zA-Z0-9_-]，因此能力名中的点号会转下划线
	Name string
	// Capability 是能力接口名，如 resource.list
	Capability  string
	Description string
	Parameters  json.RawMessage
	Risk        int
	// SideEffects 声明影响范围：read-only（默认）/ write-workspace（仅当前空间工作区）。
	// 策略层据此区分「空间内写」与「外部写」，后者必须绑定 + 审批。
	SideEffects string
	Runtime     string // builtin | mcp
	ServerID    string
	ServerName  string

	HasBinding  bool
	Constraints policy.Constraints
	// MaxResultBytes 是回填给模型的单次结果上限（默认 4KB）。
	// 读类工具（file_read / resource_read）可申请更高额度，避免长文档被无谓截断。
	MaxResultBytes int

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
	if off := r.disabledBuiltinTools(ctx); len(off) > 0 {
		tools = withoutTools(tools, off)
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
	}

	// 同名工具只能留一个：模型看到两个同名函数会随机选，且策略判定会串味。
	// 内置优先（先出现），插件与 MCP 重名时让路并留日志。
	tools = dedupeByName(tools, r.log)

	bindings, err := r.store.ListToolBindings(ctx, env.SpaceID, env.UserID)
	if err != nil {
		return nil, err
	}
	applyBindings(tools, bindings)

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

// disabledBuiltinTools 返回「被系统管理员停用」的内置工具名。
//
// 内置工具的执行体是 Go 代码（见 builtin.go），安装记录只是元数据：不查这一下，
// 系统空间里那个停用开关就只是标签，模型照样能调。停用只能在系统空间做，
// 所以只看那一份全局安装；读取失败按「没停用」处理，不能让开关把对话打挂。
func (r *Registry) disabledBuiltinTools(ctx context.Context) map[string]bool {
	sys, err := r.store.GetSpaceByType(ctx, systemSpaceType)
	if err != nil || sys.ID == "" {
		return nil
	}
	installs, err := r.store.ListPluginInstalls(ctx, sys.ID)
	if err != nil {
		r.log.Warn("读取内置件启停状态失败", "err", err)
		return nil
	}
	return disabledToolNames(installs)
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

// applyBindings 把管理员的 tool_binding 贴到工具上。
//
// 内置工具视为已绑定：tool_binding 的语义是「授权这个工具访问哪段数据」
// （数据库、MCP、自定义工具），而内置工具由平台发版、代码受控，也没有
// 按空间逐个配置的入口。按 L2 要求绑定等于把能力整体关掉——plugin_try
// 就因此被判成「需要管理员绑定」而拒绝。L3 仍要 allow_write、L4 仍要审批，
// 这里只放开「有没有绑定」这一项，不扩大写权限。
func applyBindings(tools []Tool, bindings []store.ToolBinding) {
	byName := make(map[string]store.ToolBinding, len(bindings))
	for _, b := range bindings {
		byName[strings.ToLower(b.ToolName)] = b
	}

	for i := range tools {
		key := strings.ToLower(tools[i].Name)
		if b, ok := byName[key]; ok {
			tools[i].HasBinding = true
			tools[i].Constraints = policy.ParseConstraints(b.Constraints)
			continue
		}
		// MCP 工具名带服务前缀，允许按「服务名/工具名」绑定
		if tools[i].ServerName != "" {
			if b, ok := byName[strings.ToLower(tools[i].ServerName+"/"+tools[i].Capability)]; ok {
				tools[i].HasBinding = true
				tools[i].Constraints = policy.ParseConstraints(b.Constraints)
				continue
			}
		}
		if tools[i].Runtime == RuntimeBuiltin {
			tools[i].HasBinding = true
		}
	}
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
		// 远端工具默认按「只读有成本」处理，需显式绑定后才可用
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
