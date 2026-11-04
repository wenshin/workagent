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

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// Env 是工具执行时的上下文，决定能看到哪些数据。
type Env struct {
	UserID    string
	SpaceID   string
	SessionID string
	// RunID 用于把 tool_call 归到具体一次执行（§6 tool_call.run_id）
	RunID string
	// SpacePerm 是调用方在该空间的权限，空表示不是成员（策略据此拒绝）
	SpacePerm string

	Store   store.Store
	Storage storage.Storage
	Log     *slog.Logger

	// CreateSkill 由会话层注入「在当前空间创建技能目录」的实现（space.Service.CreateSkill），
	// 工具包不依赖 space 包，避免循环依赖；为 nil 时 skill_create 不可用。
	CreateSkill func(ctx context.Context, name, description string) (dir string, err error)

	// Sources 是「当前空间 + 系统空间」合并后的能力源视图，为 nil 时相关工具不可用。
	Sources SourceLoader

	// Search 是搜索 provider 注册表（internal/search），为 nil 时 web_search 不可用。
	// 密钥与出网策略都在实现内部，不经由工具参数传递。
	Search *search.Registry
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

// Registry 组装当前可用的工具集合。
type Registry struct {
	store  store.Store
	mcp    MCPProvider
	limits Limits
	log    *slog.Logger
}

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

	bindings, err := r.store.ListToolBindings(ctx, env.SpaceID, env.UserID)
	if err != nil {
		return nil, err
	}
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
			}
		}
	}

	if len(tools) > r.limits.MaxTools {
		tools = selectRelevant(tools, query, r.limits.TopK)
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
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
