// Package agentspec 定义「智能体」在会话层的类型与常量。
//
// 智能体是 `kind=agent` 插件（见 internal/plugin）：正文是 `AGENT.md`，
// 父链由 `agent.extends` 表达，依赖由 `agent.requires` 表达，发布、安装、审计
// 与技能/工具/MCP/组件共用一条生命周期。这个包只留下会话与接口层需要的形状，
// 不再负责读文件——旧的系统空间 agents/ 目录已迁移成插件并删除。
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
	Extends     string `json:"extends,omitempty"`
	// IconURL 是头像的访问地址；清单没声明头像或文件不存在时为空，
	// 前端据此回落到「智能体首字徽标」而不是普通会话图标。
	IconURL string `json:"icon_url,omitempty"`
}

// Resolved 是合并完成、可直接注入提示词的结果。
type Resolved struct {
	ID      string
	Name    string
	Version string
	// Prompt 是追加正文（父链在前、自身在后），不含平台基础规则
	Prompt string
}
