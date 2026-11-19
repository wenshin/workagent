// Package agentspec 定义「智能体」在会话层的类型与常量。
//
// 智能体是 `kind=agent` 插件（见 internal/plugin）：正文是 `AGENT.md`，
// 依赖由 `agent.requires` 表达，发布、安装、审计与技能/工具/MCP/组件共用一条
// 生命周期。这个包只留下会话与接口层需要的形状，不再负责读文件——旧的系统空间
// agents/ 目录已迁移成插件并删除。
//
// 智能体之间**没有运行时继承**：正文只读自己那一份。父链拼接会把相反的要求
// （「可以写文件」与「不要写文件」）放进同一层互相抵消，而且父智能体发版会在
// 使用方不知情时改变行为。要复用正文就复制，要复用能力写 `agent.requires`。
//
// 平台基础规则写死在 internal/agent/prompt.go，智能体只能在基础规则之后追加，
// 不能覆盖、不能删除（方案 A）；确需改动基础规则时走平台发版。
package agentspec

import "errors"

const (
	// BaseID 是平台默认智能体（内置插件 slug），也是会话不指定智能体时的落点。
	BaseID = "base"
)

// MaxPromptBytes 是单个智能体提示词的字节上限，防止配置变成提示词炸弹。
const MaxPromptBytes = 64 << 10

var (
	// ErrNotFound 智能体不存在
	ErrNotFound = errors.New("智能体不存在")
	// ErrInvalid 智能体配置不合法
	ErrInvalid = errors.New("智能体配置不合法")
)

// Summary 是选择器需要的元信息，不含提示词正文。
type Summary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	// Extends 已废弃，恒为空：保留字段只为兼容已有客户端的 JSON 形状。
	Extends string `json:"extends,omitempty"`
	// IconURL 是头像的访问地址；清单没声明头像或文件不存在时为空，
	// 前端据此回落到「智能体首字徽标」而不是普通会话图标。
	IconURL string `json:"icon_url,omitempty"`
}

// Resolved 是合并完成、可直接注入提示词的结果。
type Resolved struct {
	ID      string
	Name    string
	Version string
	// Prompt 是追加正文，不含平台基础规则
	Prompt string
	// Builtin 表示这是平台内置件（origin=system）。
	//
	// 内置件是运营对象：正文在运行时解析，插件卡片上改了立刻生效。会话不固化它的
	// 正文与版本，否则「运营改了规则，只有新会话生效」这种情况没人解释得通。
	// 自建/安装的智能体属于业务配置，仍按创建时固化，保证结果可追溯。
	Builtin bool
}
