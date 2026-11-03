package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// builtinTools 返回平台内置能力实现。
//
// 全部为只读（L1）：MVP 不开放写操作，避免模型误改数据；
// 需要写能力的场景走绑定 + 审批，见 policy.RBACEngine。
func builtinTools() []Tool {
	return []Tool{
		timeNow(),
		resourceList(),
		sessionList(),
		fileList(),
	}
}

func timeNow() Tool {
	return Tool{
		Name:        "time_now",
		Capability:  "time.now",
		Description: "获取服务器当前的日期与时间（含时区与星期）。回答时间相关问题前先调用。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.RiskReadOnly,
		Runtime:     "builtin",
		Invoke: func(context.Context, json.RawMessage, Env) (string, error) {
			now := time.Now()
			return now.Format("2006-01-02 15:04:05 MST (Monday)"), nil
		},
	}
}

func resourceList() Tool {
	return Tool{
		Name:        "resource_list",
		Capability:  "resource.list",
		Description: "列出当前用户可访问的工作空间（名称、类型、权限、层级路径）。用户提到某个空间但不确定名称时使用。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.RiskReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, _ json.RawMessage, env Env) (string, error) {
			spaces, err := env.Store.ListSpaces(ctx, env.UserID)
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
			return marshalLimited(out, 8*1024), nil
		},
	}
}

func sessionList() Tool {
	return Tool{
		Name:        "session_list",
		Capability:  "session.list",
		Description: "列出当前空间下的会话（最近活跃优先）。用户询问历史对话或需要引用其他会话时使用。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {"type": "integer", "description": "最多返回条数，默认 10，最大 30"}
  },
  "additionalProperties": false
}`),
		Risk:    policy.RiskReadOnly,
		Runtime: "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
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

			sessions, err := env.Store.ListSessions(ctx, env.SpaceID)
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
			return marshalLimited(out, 8*1024), nil
		},
	}
}

func fileList() Tool {
	return Tool{
		Name:        "file_list",
		Capability:  "fs.list",
		Description: "列出当前空间目录下的文件与子目录（相对空间根目录）。需要了解空间里有哪些文件时使用。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间根目录的路径，空表示根目录"}
  },
  "additionalProperties": false
}`),
		Risk:    policy.RiskReadOnly,
		Runtime: "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Storage == nil {
				return "", fmt.Errorf("文件存储不可用")
			}
			var in struct {
				Path string `json:"path"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("参数解析失败：%w", err)
				}
			}

			rel := strings.Trim(strings.TrimSpace(in.Path), "/")
			if strings.Contains(rel, "..") {
				return "", fmt.Errorf("路径非法：不允许跳出空间目录")
			}

			// 空间根目录 + 相对路径，避免越权访问其他空间
			prefix := env.Storage.SpaceKey(env.SpaceID)
			if rel != "" {
				prefix = path.Join(prefix, rel)
			}

			entries, err := env.Storage.List(ctx, prefix)
			if err != nil {
				if err == storage.ErrNotFound {
					return "目录不存在。", nil
				}
				return "", err
			}
			if len(entries) == 0 {
				return "目录为空。", nil
			}

			out := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				name := strings.TrimPrefix(strings.TrimPrefix(e.Key, prefix), "/")
				if name == "" {
					name = path.Base(e.Key)
				}
				out = append(out, map[string]any{
					"name":     name,
					"dir":      e.IsDir,
					"size":     e.Size,
					"modified": e.ModTime.Format(time.RFC3339),
				})
			}
			return marshalLimited(out, 8*1024), nil
		},
	}
}

func marshalLimited(v any, limit int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	if len(b) <= limit {
		return string(b)
	}
	// 超限时截断，并明确告知已截断，避免模型以为拿到了全量
	return string(b[:limit]) + fmt.Sprintf("…（已截断，共 %d 字节）", len(b))
}

var _ = store.ErrNotFound
