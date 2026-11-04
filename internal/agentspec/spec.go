// Package agentspec 负责「智能体设定」的加载与合并。
//
// 一个智能体 = 系统空间 agents/<id>/ 下的 agent.json + prompt.md。它只是行为设定，
// 不是权限：可见工具仍由 tool_binding 与策略决定，这里只决定往提示词里追加哪段正文。
//
// 平台基础规则写死在 internal/agent/prompt.go，智能体只能在基础规则之后追加，
// 不能覆盖、不能删除（方案 A）；确需改动基础规则时走平台发版。
package agentspec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// Dir 是智能体配置在空间 files/ 下的根目录
	Dir = "agents"
	// IndexFile 是选择器用的清单（不进模型上下文）
	IndexFile = "index.json"
	// SpecFile 是单个智能体的配置
	SpecFile = "agent.json"
	// PromptFile 是单个智能体的追加提示词
	PromptFile = "prompt.md"
	// BaseID 是默认智能体，也是唯一允许被 extends 的对象（禁止链式继承）
	BaseID = "base"
)

// MaxPromptBytes 是单个智能体提示词的字节上限，防止配置变成提示词炸弹。
const MaxPromptBytes = 64 << 10

var (
	// ErrNotFound 智能体不存在
	ErrNotFound = errors.New("智能体不存在")
	// ErrDisabled 智能体已停用
	ErrDisabled = errors.New("智能体已停用")
	// ErrInvalid 配置不合法
	ErrInvalid = errors.New("智能体配置不合法")
)

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// Spec 是 agent.json 的结构。
//
// 没有「覆盖基础规则」的字段，也没有任何 endpoint / 密钥 / 命令字段：
// 智能体只描述行为，不承载连接信息与权限。
type Spec struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	// Extends 只能是空或 "base"；链式继承会让行为不可预测，直接拒绝
	Extends string `json:"extends"`
	// Prompt 是追加提示词的文件名，默认 prompt.md
	Prompt string `json:"prompt"`
	// Enabled 省略时为 true
	Enabled *bool `json:"enabled,omitempty"`
}

// Summary 是选择器需要的元信息，不含提示词正文。
type Summary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Extends     string `json:"extends,omitempty"`
}

// Index 是 agents/index.json 的结构。
type Index struct {
	Agents []Summary `json:"agents"`
}

// Resolved 是合并完成、可直接注入提示词的结果。
type Resolved struct {
	ID      string
	Name    string
	Version string
	// Prompt 是追加正文（base 在前、自身在后），不含平台基础规则
	Prompt string
}

// IsEnabled 返回是否启用，缺省视为启用。
func (s Spec) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Validate 校验单个配置的静态约束。
func (s Spec) Validate() error {
	if !idRe.MatchString(s.ID) {
		return fmt.Errorf("%w: id 只能是小写字母开头的 1-32 位字母数字下划线短横线，收到 %q", ErrInvalid, s.ID)
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: name 不能为空（%s）", ErrInvalid, s.ID)
	}
	if !versionRe.MatchString(s.Version) {
		return fmt.Errorf("%w: version 必须是 x.y.z（%s 收到 %q）", ErrInvalid, s.ID, s.Version)
	}
	switch s.Extends {
	case "", BaseID:
	default:
		// 只允许直接继承 base，禁止 base -> a -> b 的链式继承
		return fmt.Errorf("%w: extends 只允许空或 %q（%s 收到 %q）", ErrInvalid, BaseID, s.ID, s.Extends)
	}
	if s.Extends == BaseID && s.ID == BaseID {
		return fmt.Errorf("%w: %s 不能继承自身", ErrInvalid, BaseID)
	}
	return nil
}

// resolve 把「基础规则之外的部分」拼成最终追加正文。
//
// 顺序固定为 base 在前、自身在后：追加内容无法删除前面的内容，
// 也无法改变平台基础规则的优先级。
func resolve(spec Spec, basePrompt, ownPrompt string) (Resolved, error) {
	if err := spec.Validate(); err != nil {
		return Resolved{}, err
	}
	if !spec.IsEnabled() {
		return Resolved{}, fmt.Errorf("%w: %s", ErrDisabled, spec.ID)
	}

	prompt := strings.TrimSpace(ownPrompt)
	if spec.Extends == BaseID {
		prompt = joinPrompts(strings.TrimSpace(basePrompt), prompt)
	}
	if len(prompt) > MaxPromptBytes {
		return Resolved{}, fmt.Errorf("%w: %s 的提示词超过 %d 字节", ErrInvalid, spec.ID, MaxPromptBytes)
	}

	return Resolved{
		ID:      spec.ID,
		Name:    spec.Name,
		Version: spec.Version,
		Prompt:  prompt,
	}, nil
}

func joinPrompts(base, own string) string {
	switch {
	case base == "":
		return own
	case own == "":
		return base
	default:
		return base + "\n\n" + own
	}
}

func validateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}
