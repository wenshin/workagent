package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/storage"
)

// builtinTools 返回平台内置能力实现。
//
// 除 fs.write 外全部只读（L1）。fs.write 的风险等级是 L3，但副作用范围被限制在
// 当前空间文件根下（SideEffectsWorkspace），策略层按空间权限放行，见 policy.RBACEngine。
func builtinTools() []Tool {
	return []Tool{
		timeNow(),
		resourceList(),
		sessionList(),
		fileList(),
		fileRead(),
		fileWrite(),
		skillCreate(),
		globalSourceList(),
		globalSourceRead(),
	}
}

// Manifest 是内置工具的声明式定义，写入系统空间的 tools/ 目录，作为全局搜索源。
type Manifest struct {
	Name        string          `json:"name"`
	Capability  string          `json:"capability"`
	Description string          `json:"description"`
	Runtime     string          `json:"runtime"`
	Risk        int             `json:"risk_level"`
	Parameters  json.RawMessage `json:"parameters"`
}

// BuiltinManifests 返回内置工具清单：系统空间 tools/ 的内容与运行时保持一致，避免两处漂移。
func BuiltinManifests() []Manifest {
	tools := builtinTools()
	out := make([]Manifest, 0, len(tools))
	for _, t := range tools {
		out = append(out, Manifest{
			Name:        t.Name,
			Capability:  t.Capability,
			Description: t.Description,
			Runtime:     t.Runtime,
			Risk:        t.Risk,
			Parameters:  t.Parameters,
		})
	}
	return out
}

// 系统空间的固定目录名；与 space 包保持一致，这里用字面量避免包间循环依赖。
const systemSpaceType = "system"

var globalSourceDirs = []string{"skills", "tools", "mcps"}

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

// globalSourceList 列出「当前空间 + 系统空间」合并后的 skills / tools / mcps。
//
// 同名资源以当前空间为准：用户在自己的空间里定制了同名技能或工具时，
// 平台默认版本不会覆盖它，Agent 只会看到一份。
func globalSourceList() Tool {
	return Tool{
		Name:       "global_source_list",
		Capability: "system.source.list",
		Description: "列出当前空间与系统空间可用的 skills / tools / mcps（名称、描述、路径、来源）。" +
			"需要找可复用技能、工具定义或 MCP 服务时先调用；同名资源以当前空间为准。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {"type": "string", "enum": ["skills", "tools", "mcps"], "description": "只看某一类，省略则全部"}
  },
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Sources == nil {
				return "", fmt.Errorf("能力源不可用")
			}
			var in struct {
				Kind string `json:"kind"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("参数解析失败：%w", err)
				}
			}
			if in.Kind != "" && in.Kind != "skills" && in.Kind != "tools" && in.Kind != "mcps" {
				return "", fmt.Errorf("kind 只能是 skills / tools / mcps")
			}

			entries, err := env.Sources.Sources(ctx)
			if err != nil {
				return "", err
			}
			out := make([]SourceEntry, 0, len(entries))
			for _, e := range entries {
				if in.Kind != "" && e.Kind != in.Kind {
					continue
				}
				out = append(out, e)
			}
			if len(out) == 0 {
				return "当前空间与系统空间暂未提供相关资源。", nil
			}
			body, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return "", err
			}
			return clamp(string(body), 8*1024), nil
		},
	}
}

// globalSourceRead 读取能力源文件：当前空间优先，找不到再到系统空间找。
//
// 因此普通空间根下的技能（<技能名>/SKILL.md）与系统空间的 skills/<技能名>/SKILL.md
// 用同一个工具读取，模型不需要关心资源在哪一侧。
func globalSourceRead() Tool {
	return Tool{
		Name:       "global_source_read",
		Capability: "system.source.read",
		Description: "读取能力源文件正文（SKILL.md、工具定义或 MCP 快照 JSON）。" +
			"路径取自 global_source_list；当前空间存在同名文件时优先，否则读系统空间。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间 files 的路径，如 周报生成/SKILL.md 或 skills/空间管理/SKILL.md"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Sources == nil {
				return "", fmt.Errorf("能力源不可用")
			}
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			text, err := env.Sources.ReadSource(ctx, in.Path)
			if err != nil {
				return "", err
			}
			return text, nil
		},
	}
}

func fileList() Tool {
	return Tool{
		Name:        "file_list",
		Capability:  "fs.list",
		Description: "列出当前空间文件目录下的文件与子目录（路径相对空间文件根目录）。需要了解空间里有哪些文件时使用。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间文件根目录的路径，空表示根目录"}
  },
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Storage == nil || env.SpaceID == "" {
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

			rel, err := workspaceDirPath(in.Path)
			if err != nil {
				return "", err
			}

			// 只列当前空间文件根下的内容，既避免越权访问其他空间，也不暴露平台元数据
			prefix := path.Join(env.Storage.SpaceKey(env.SpaceID), workspaceFilesDir)
			if rel != "" {
				prefix = path.Join(prefix, rel)
			}

			entries, err := env.Storage.List(ctx, prefix)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					return "目录不存在。", nil
				}
				return "", err
			}

			out := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				name := path.Base(e.Key)
				// 隐藏文件（.meta.json 等）是平台元数据，不暴露给模型
				if strings.HasPrefix(name, ".") {
					continue
				}
				entryPath := name
				if rel != "" {
					entryPath = rel + "/" + name
				}
				out = append(out, map[string]any{
					"name":     name,
					"path":     entryPath,
					"dir":      e.IsDir,
					"size":     e.Size,
					"modified": e.ModTime.Format(time.RFC3339),
				})
			}
			if len(out) == 0 {
				return "目录为空。", nil
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
