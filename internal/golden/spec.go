// Package golden 是 Agent Harness 的黄金用例机制。
//
// 解决的问题：harness 的行为（何时收尾、何时拦截重复调用、派发到第几层停）
// 靠人读代码是保证不了的，靠线上观察又太晚。黄金用例把「期望的循环行为」
// 固化成可重放的数据：同一份脚本化模型输入，必须得到同样的停止原因、
// 同样的工具序列、同样的结论。
//
// 两种用例共用一份文件（插件目录下的 evals.json）：
//
//	kind=tool   确定性用例：给定输入跑一次 dry-run，断言输出。不需要模型，
//	            因此在插件发布门禁里可以强制跑。
//	kind=agent  行为用例：脚本化模型驱动 harness，断言停止原因与调用序列。
//	            需要模型时不参与发布门禁（发布不应依赖外部服务可用），
//	            由 CI / 本地 `go test` 跑。
package golden

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
)

// FileName 是黄金用例在插件目录里的固定文件名。
// 放在插件包里而不是平台仓库：「这个技能按说明书能不能被用对」只有作者最清楚。
const FileName = "evals.json"

// 用例类型。
const (
	// KindTool 是确定性用例，发布门禁可强制执行
	KindTool = "tool"
	// KindAgent 是行为用例，需要模型，发布门禁默认跳过
	KindAgent = "agent"
)

// Suite 是一套黄金用例。
type Suite struct {
	Version int `json:"version"`
	// Required 表示这套用例缺失时发布应当失败；默认只告警
	Required bool   `json:"required,omitempty"`
	Cases    []Case `json:"cases"`
}

// Case 是一条黄金用例。
type Case struct {
	Name string `json:"name"`
	Kind string `json:"kind"`

	// ---- kind=tool ----
	// Input 是传给 dry-run 的输入（与 plugin_try 的 input 同格式）
	Input json.RawMessage `json:"input,omitempty"`

	// ---- kind=agent ----
	// Query 是本轮用户输入
	Query string `json:"query,omitempty"`
	// Script 是脚本化的模型回合，按顺序消费；用完即判失败
	Script []Turn `json:"script,omitempty"`
	// Tools 是用例提供的工具桩
	Tools []ToolSpec `json:"tools,omitempty"`
	// Limits 覆盖本条用例的 harness 预算，用于验证预算边界本身
	Limits *Limits `json:"limits,omitempty"`
	Expect Expect  `json:"expect"`

	// 以下字段只在发布门禁里被填充（不来自 JSON）：
	// 插件正文随用例一起进系统提示词，否则「这个技能能不能被用对」根本测不到。
	InjectSkill  *agent.ActiveSkill `json:"-"`
	InjectPrompt string             `json:"-"`
}

// Limits 是单条用例的预算覆盖。
type Limits struct {
	MaxRounds    int   `json:"max_rounds,omitempty"`
	MaxToolCalls int   `json:"max_tool_calls,omitempty"`
	StallRounds  int   `json:"stall_rounds,omitempty"`
	MaxDepth     int   `json:"max_depth,omitempty"`
	Dispatch     *bool `json:"dispatch,omitempty"`
}

// Turn 是一次模型调用的脚本化结果。
type Turn struct {
	Content   string `json:"content,omitempty"`
	ToolCalls []Call `json:"tool_calls,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	// Error 非空表示这次模型调用失败（用于验证降级与报错路径）
	Error string `json:"error,omitempty"`
}

// Call 是一次脚本化的工具调用。
type Call struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ToolSpec 是黄金用例里的工具桩。
type ToolSpec struct {
	Name string `json:"name"`
	// Result 是成功返回的内容
	Result string `json:"result,omitempty"`
	// Error 非空表示工具执行失败
	Error string `json:"error,omitempty"`
	// Risk 默认 1（只读）；用例声明的工具视为已绑定
	Risk int `json:"risk,omitempty"`
}

// Usage 是脚本化回合的 token 计量。
type Usage struct {
	InputTokens  int `json:"in,omitempty"`
	OutputTokens int `json:"out,omitempty"`
}

// Expect 是断言集合。两类用例共用结构，按 Kind 取用相应字段。
type Expect struct {
	// ---- kind=tool ----
	Contains      []string `json:"contains,omitempty"`
	NotContains   []string `json:"not_contains,omitempty"`
	ErrorExpected bool     `json:"error_expected,omitempty"`

	// ---- kind=agent ----
	// StopReason 取值：done / budget / stalled
	StopReason         string   `json:"stop_reason,omitempty"`
	ContentContains    []string `json:"content_contains,omitempty"`
	ContentNotContains []string `json:"content_not_contains,omitempty"`
	// ToolSequence 是实际执行的工具名顺序（被拦截的重复调用不计入）
	ToolSequence []string `json:"tool_sequence,omitempty"`
	// MaxRounds 是允许的最大模型调用次数
	MaxRounds int `json:"max_rounds,omitempty"`
	// MaxToolCalls 是允许的最大工具执行次数
	MaxToolCalls int `json:"max_tool_calls,omitempty"`
	// BlockedAtLeast 要求至少拦截多少次重复调用
	BlockedAtLeast int `json:"blocked_at_least,omitempty"`
}

// Load 从 reader 解析用例集并做静态校验。
func Load(r io.Reader) (Suite, error) {
	var s Suite
	dec := json.NewDecoder(r)
	// 未知字段直接报错：写错的断言字段会被静默忽略，用例就变成了「假绿」
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("黄金用例解析失败：%w", err)
	}
	if err := s.Validate(); err != nil {
		return Suite{}, err
	}
	return s, nil
}

// LoadFile 从文件加载用例集。
func LoadFile(path string) (Suite, error) {
	f, err := os.Open(path)
	if err != nil {
		return Suite{}, err
	}
	defer func() { _ = f.Close() }()
	return Load(f)
}

// Validate 做静态校验：名字、类型、脚本非空、断言不空。
// 一条「没有任何断言」的用例比没有用例更危险：它会稳定通过。
func (s Suite) Validate() error {
	if len(s.Cases) == 0 {
		return fmt.Errorf("黄金用例集为空：至少需要一条用例")
	}
	seen := make(map[string]struct{}, len(s.Cases))
	for i, c := range s.Cases {
		where := fmt.Sprintf("第 %d 条用例", i+1)
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("%s：name 不能为空", where)
		}
		where = fmt.Sprintf("用例 %q", c.Name)
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("%s：name 重复", where)
		}
		seen[c.Name] = struct{}{}
		switch c.Kind {
		case KindTool:
			if len(c.Expect.Contains) == 0 && len(c.Expect.NotContains) == 0 && !c.Expect.ErrorExpected {
				return fmt.Errorf("%s：tool 用例至少要有一个断言（contains / not_contains / error_expected）", where)
			}
		case KindAgent:
			if strings.TrimSpace(c.Query) == "" {
				return fmt.Errorf("%s：agent 用例缺少 query", where)
			}
			if len(c.Script) == 0 {
				return fmt.Errorf("%s：agent 用例缺少 script", where)
			}
			if !hasAgentAssertion(c.Expect) {
				return fmt.Errorf("%s：agent 用例至少要有一个断言（stop_reason / content_contains / tool_sequence / max_rounds / max_tool_calls / blocked_at_least）", where)
			}
		default:
			return fmt.Errorf("%s：kind 只能是 %s 或 %s", where, KindTool, KindAgent)
		}
	}
	return nil
}

func hasAgentAssertion(e Expect) bool {
	return e.StopReason != "" || len(e.ContentContains) > 0 || len(e.ContentNotContains) > 0 ||
		len(e.ToolSequence) > 0 || e.MaxRounds > 0 || e.MaxToolCalls > 0 || e.BlockedAtLeast > 0
}

// CasesByKind 返回指定类型的用例，顺序保持与文件一致。
func (s Suite) CasesByKind(kind string) []Case {
	out := make([]Case, 0, len(s.Cases))
	for _, c := range s.Cases {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// Kinds 返回用例集里出现的类型（去重、字典序），用于报告。
func (s Suite) Kinds() []string {
	set := map[string]struct{}{}
	for _, c := range s.Cases {
		set[c.Kind] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
