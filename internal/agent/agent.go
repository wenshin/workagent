// Package agent 定义 Agent 运行时的可替换契约（技术方案 §5.7.2 主循环、§5.7.7 框架选型）。
//
// 分层约束（§5.7.7）：Run 状态机、预算、审批挂起、checkpoint、上下文压缩与 ToolInvoker
// 全部由平台自研（internal/session、internal/tool、internal/ctxmgr、internal/policy）；
// 本包只抽出「编排与循环」这一层的契约，实现放在子包，切换实现只改 AGENT_IMPL：
//
//	internal/agent/builtin  内置 ReAct 循环（默认）
//	internal/agent/direct   无工具单轮直答（轻量路径）
//	internal/agent/eino     预留：Eino 编排适配，仅该子包可 import Eino
//
// 框架类型不得出现在本包的接口、事件与输入输出结构里（§5.7.7「框架类型不出模块边界」）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/tool"
)

// Capabilities 声明一个实现具备的能力，供上层按能力降级并向客户端说明。
//
// 这里只声明「编排层」能力；审批挂起、checkpoint、上下文压缩、预算控制对每个实现一致，
// 由平台提供，不在此声明（§5.7.7 能力边界表）。
type Capabilities struct {
	// Tools 是否加载工具并执行「模型 → 工具 → 模型」循环
	Tools bool `json:"tools"`
	// ParallelTools 是否并行执行同一轮的多个工具调用
	ParallelTools bool `json:"parallel_tools"`
	// Streaming 是否流式输出增量
	Streaming bool `json:"streaming"`
	// Reasoning 是否透传模型思考过程（message.delta 的 reasoning 字段）
	Reasoning bool `json:"reasoning"`
	// Approval 是否会产生审批请求（approval.req）
	Approval bool `json:"approval"`
	// Resume 是否支持从 checkpoint 续跑（§5.7.5，尚未实现）
	Resume bool `json:"resume"`
}

// Agent 是一次运行的编排实现。会话层只依赖本接口。
type Agent interface {
	// Name 返回实现名，写入 run.budget 与启动日志，便于按实现排查问题
	Name() string
	// Capabilities 返回实现具备的能力
	Capabilities() Capabilities
	// Ready 报告实现是否可用（通常取决于模型路由）
	Ready() bool
	// Model 返回当前实际使用的模型名
	Model() string
	// Budget 返回本次执行的预算上限，写入 run.budget（§5.7.2 Token 预算）
	Budget() map[string]any
	// Run 执行一次对话，过程中通过 emit 推送增量事件；emit 返回错误时实现应尽快退出
	Run(ctx context.Context, in RunInput, emit func(Event) error) (Result, error)
}

// Deps 是构造实现时注入的平台依赖。
//
// 实现只能通过这些依赖访问模型、工具与配置，不得直接读环境变量或建连。
type Deps struct {
	Config   *config.Config
	LLM      *llm.Router
	Registry *tool.Registry
	Invoker  *tool.Invoker
	Log      *slog.Logger
}

// Factory 构造一个实现。
type Factory func(Deps) (Agent, error)

// ErrBudgetExceeded 表示实现声明并自行执行的运行预算（墙钟、工具次数、轮次）已用尽。
// 循环外层据此把这次 run 判定为 cancelled（保留已生成内容并说明原因），而不是 failed——
// 「重试就好」的提示对预算用尽是误导。
var ErrBudgetExceeded = errors.New("运行预算已用尽")

var (
	registryMu sync.RWMutex
	factories  = map[string]Factory{}
)

// Register 注册一个实现，由实现包的 init 调用；同名覆盖。
func Register(name string, f Factory) {
	if name == "" || f == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	factories[name] = f
}

// Available 返回已注册的实现名（字典序），用于配置校验与错误提示。
func Available() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New 按名字构造实现；名字为空或未注册时返回错误。
func New(name string, deps Deps) (Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("未指定 Agent 实现（AGENT_IMPL），可用：%v", Available())
	}
	registryMu.RLock()
	factory, ok := factories[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("未知的 Agent 实现 %q，可用：%v", name, Available())
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	impl, err := factory(deps)
	if err != nil {
		return nil, fmt.Errorf("构造 Agent 实现 %q 失败：%w", name, err)
	}
	return impl, nil
}
