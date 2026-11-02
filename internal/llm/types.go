package llm

import "encoding/json"

// 角色常量，与 OpenAI Chat Completions 协议一致。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message 是一条对话消息；工具调用与工具结果复用同一结构。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 是模型请求的一次工具调用。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具名与参数（JSON 字符串）。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 是暴露给模型的函数定义。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述函数的名称、用途与参数 Schema。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Delta 是流式返回的一小段增量。
type Delta struct {
	// Reasoning 是模型的思考过程（DeepSeek-R1 / Qwen 思考模式）
	Reasoning string
	// Content 是最终回答的正文本
	Content string
}

// Result 是一次完整模型调用的产出。
type Result struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
}
