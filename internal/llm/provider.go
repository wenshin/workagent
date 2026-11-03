package llm

import (
	"context"
	"errors"
	"fmt"
)

// Capability 是模型能力标签，ModelRouter 据此选模型（技术方案 §5.7.3）。
type Capability string

const (
	CapTool      Capability = "tool"
	CapThinking  Capability = "thinking"
	CapVision    Capability = "vision"
	CapStructure Capability = "structure"
	CapEmbedding Capability = "embedding"
)

// ErrUnsupportedProvider 表示当前协议族不具备该能力（例如 Anthropic 无 Embedding 接口）。
var ErrUnsupportedProvider = errors.New("llm: capability not supported by provider")

// ProviderInfo 描述一个已实例化的模型端点。
type ProviderInfo struct {
	Name         string // 逻辑名：primary / fallback / embedding
	Kind         string // 协议族：openai | anthropic
	Model        string
	Capabilities []Capability
}

// Has 判断 Provider 是否具备某项能力。
func (i ProviderInfo) Has(c Capability) bool {
	for _, item := range i.Capabilities {
		if item == c {
			return true
		}
	}
	return false
}

// Provider 是 Provider 适配层（§5.7.1 L1）的统一接口。
//
// 上层 Harness / ModelRouter 只依赖本接口，不感知具体协议：
// openai 兼容协议覆盖 OpenAI / DeepSeek / 通义兼容模式 / vLLM / Ollama，
// anthropic 走原生 Messages 协议。
type Provider interface {
	Info() ProviderInfo
	// Chat 发起一次流式对话；onDelta 可为 nil（只要最终结果）。
	Chat(ctx context.Context, req ChatRequest, onDelta DeltaFunc) (Result, error)
	// Embed 计算文本向量；不支持时返回 ErrUnsupportedProvider。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ProviderError 携带 HTTP 状态与可重试语义，供 ModelRouter 判断是否降级。
type ProviderError struct {
	Provider string
	Status   int
	Message  string
	Err      error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("%s: %d %s", e.Provider, e.Status, e.Message)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// Retryable 表示是否可切备模型：网络错误、超时、429、5xx 可降级；4xx 与主动取消不降级。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	// 用户主动取消或整体超时不再重试
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var pe *ProviderError
	if errors.As(err, &pe) {
		switch {
		case pe.Status == 0: // 连接失败、读流中断
			return true
		case pe.Status == 429:
			return true
		case pe.Status >= 500:
			return true
		default:
			return false
		}
	}
	return false
}
