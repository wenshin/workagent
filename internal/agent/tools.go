package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/workagent/auto-agent/internal/llm"
)

// Scope 是工具执行时的身份上下文，决定能看到哪些数据。
type Scope struct {
	UserID  string
	SpaceID string
}

// Tool 是一个可被模型调用的只读工具。
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Run         func(ctx context.Context, args json.RawMessage, scope Scope) (string, error)
}

// Definitions 把工具转换成模型可识别的函数定义。
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

// buildTools 返回首版内置的只读工具集。
//
// 只放只读能力：MVP 阶段不提供写操作，避免模型误删数据。
func (s *Service) buildTools() []Tool {
	return []Tool{
		{
			Name:        "get_current_time",
			Description: "获取服务器当前的日期与时间（含时区），需要回答时间相关问题时使用。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Run: func(ctx context.Context, _ json.RawMessage, _ Scope) (string, error) {
				now := time.Now()
				return now.Format("2006-01-02 15:04:05 MST (Monday)"), nil
			},
		},
		{
			Name:        "list_my_spaces",
			Description: "列出当前用户可访问的工作空间，含空间名称、类型与层级。用户提到某个空间但不确定名称时可用。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Run: func(ctx context.Context, _ json.RawMessage, scope Scope) (string, error) {
				spaces, err := s.store.ListSpaces(ctx, scope.UserID)
				if err != nil {
					return "", err
				}
				if len(spaces) == 0 {
					return "当前没有任何空间。", nil
				}
				out := make([]map[string]any, 0, len(spaces))
				for _, sp := range spaces {
					out = append(out, map[string]any{
						"name": sp.Name,
						"type": sp.Type,
						"perm": sp.Perm,
						"path": sp.Path,
					})
				}
				return toJSON(out), nil
			},
		},
		{
			Name:        "list_space_sessions",
			Description: "列出当前空间下的会话（最近活跃的在前）。用户询问历史对话、或需要引用别的会话时使用。",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {"type": "integer", "description": "最多返回条数，默认 10，最大 30"}
  },
  "additionalProperties": false
}`),
			Run: func(ctx context.Context, args json.RawMessage, scope Scope) (string, error) {
				var in struct {
					Limit int `json:"limit"`
				}
				if len(args) > 0 {
					if err := json.Unmarshal(args, &in); err != nil {
						return "", fmt.Errorf("参数解析失败：%w", err)
					}
				}
				if in.Limit <= 0 {
					in.Limit = 10
				}
				if in.Limit > 30 {
					in.Limit = 30
				}

				sessions, err := s.store.ListSessions(ctx, scope.SpaceID)
				if err != nil {
					return "", err
				}
				if len(sessions) > in.Limit {
					sessions = sessions[:in.Limit]
				}
				out := make([]map[string]any, 0, len(sessions))
				for _, sess := range sessions {
					item := map[string]any{"title": sess.Title}
					if sess.LastMessageAt != nil {
						item["last_message_at"] = sess.LastMessageAt.Format(time.RFC3339)
					}
					out = append(out, item)
				}
				return toJSON(out), nil
			},
		},
	}
}

func findTool(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
