package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
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
		resourceSearch(),
		resourceRead(),
		webSearch(),
		shellExec(),
		pluginTry(),
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

// resourceSearch 按关键词检索「当前空间 + 系统空间」的资源。
//
// 资源总量没有上限，因此不提供「列出全部」的工具：模型按关键词检索，
// 命中同名多来源时必须在回答前让用户确认（约束写在系统提示词里）。
func resourceSearch() Tool {
	return Tool{
		Name:       "resource_search",
		Capability: "system.source.search",
		Description: "按关键词检索可用的技能（skills）、工具（tools）、MCP 服务（mcps）与组件（components），返回最相关的若干条及读取路径。" +
			"资源总量没有上限，不要试图列举全部；需要某类能力但不确定叫什么时，用用户原话里的关键词检索。" +
			"不确定有什么可用能力时，可以省略 query 只给 kind，按名称列出该类资源再挑。" +
			"命中多个同名资源时先让用户选择，不要自行假设。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "检索关键词，用能力名或任务关键词，如 周报、pdf、数据库；省略时需同时给 kind"},
    "kind": {"type": "string", "enum": ["skills", "tools", "mcps", "components"], "description": "只检索某一类，省略则全部"},
    "limit": {"type": "integer", "description": "返回条数，默认 8，最大 20"}
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
				Query string `json:"query"`
				Kind  string `json:"kind"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			query := strings.TrimSpace(in.Query)
			kind := normalizeSourceKind(in.Kind)
			if in.Kind != "" && kind == "" {
				return "", fmt.Errorf("kind 只能是 skills / tools / mcps / components")
			}
			// 不再常驻技能索引，所以允许「只给 kind」按名称拉取该类目录
			if query == "" && kind == "" {
				return "", fmt.Errorf("query 与 kind 至少提供一个：给关键词检索，或给 kind 列出该类资源")
			}
			if in.Limit <= 0 {
				in.Limit = 8
			}
			if in.Limit > 20 {
				in.Limit = 20
			}

			entries, err := env.Sources.Sources(ctx)
			if err != nil {
				return "", err
			}
			hits := make([]scoredSource, 0, len(entries))
			for _, e := range entries {
				if kind != "" && e.Kind != kind {
					continue
				}
				if query == "" {
					// 只给 kind：按名称列目录，score 取相同值让排序退化为 kind + name
					hits = append(hits, scoredSource{SourceEntry: e, Score: 1})
					continue
				}
				if score := scoreSource(query, e); score > 0 {
					hits = append(hits, scoredSource{SourceEntry: e, Score: score})
				}
			}
			sort.SliceStable(hits, func(i, j int) bool {
				if hits[i].Score != hits[j].Score {
					return hits[i].Score > hits[j].Score
				}
				if hits[i].Kind != hits[j].Kind {
					return hits[i].Kind < hits[j].Kind
				}
				return hits[i].Name < hits[j].Name
			})

			res := sourceSearchResult{Query: query, Matched: len(hits), Items: []scoredSource{}}
			if len(hits) > in.Limit {
				hits = hits[:in.Limit]
				res.Truncated = true
			}
			res.Items = append(res.Items, hits...)
			switch {
			case res.Matched == 0:
				res.Hint = "没有匹配的资源，可换关键词重试；当前空间的文件用 file_list 查看"
			case query == "":
				res.Hint = "未按关键词过滤，按名称列出；用 query 可以缩小范围"
			case looksAmbiguous(res.Items):
				res.Hint = "存在同名资源（来源不同），使用前请让用户确认用哪一个"
			}
			body, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return "", err
			}
			return clamp(string(body), 8*1024), nil
		},
	}
}

// resourceRead 读取资源文件：当前空间优先，找不到再到系统空间找。
//
// 空间里的技能草稿（plugins/<技能slug>/SKILL.md）与内置件（published/skills/<slug>/<version>/SKILL.md）
// 用同一个工具读取，模型不需要关心资源在哪一侧。
func resourceRead() Tool {
	return Tool{
		Name:       "resource_read",
		Capability: "system.source.read",
		Description: "读取资源文件正文（技能的 SKILL.md、工具定义或 MCP 快照 JSON）。" +
			"路径取自 resource_search；当前空间存在同名文件时优先，否则读系统空间；长文档可用 offset / limit 分段读取。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间 files 的路径，如 plugins/weekly_report/SKILL.md 或 published/skills/builtin_space/1.0.1/SKILL.md"},
    "offset": {"type": "integer", "description": "起始字节偏移，默认 0"},
    "limit": {"type": "integer", "description": "本次最多读取的字节数，默认 16384，最大 24576"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		// 读正文是明确的信息获取动作，允许比默认额度更大，减少分段次数
		MaxResultBytes: 24 * 1024,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Sources == nil {
				return "", fmt.Errorf("能力源不可用")
			}
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			text, err := env.Sources.ReadSource(ctx, in.Path)
			if err != nil {
				return "", err
			}
			return sliceBytes(text, in.Offset, in.Limit, 16*1024, 24*1024), nil
		},
	}
}

// scoredSource 是带匹配分数的检索结果；Score 只在内部排序用，不返回给模型。
type scoredSource struct {
	SourceEntry
	Score int `json:"-"`
}

type sourceSearchResult struct {
	Query     string         `json:"query"`
	Matched   int            `json:"matched"`
	Truncated bool           `json:"truncated,omitempty"`
	Items     []scoredSource `json:"items"`
	Hint      string         `json:"hint,omitempty"`
}

// normalizeSourceKind 兼容单复数写法，返回索引里使用的复数形式。
func normalizeSourceKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "":
		return ""
	case "skill", "skills":
		return "skills"
	case "tool", "tools":
		return "tools"
	case "mcp", "mcps":
		return "mcps"
	case "component", "components":
		return "components"
	default:
		return ""
	}
}

// scoreSource 计算资源与关键词的相关度：名称命中权重最高，其次描述。
//
// 中文查询通常不带空格，因此除整词匹配外，再按相邻两字（bigram）命中计分。
func scoreSource(query string, e SourceEntry) int {
	name := strings.ToLower(e.Name)
	desc := strings.ToLower(e.Description)
	score := 0
	for _, term := range strings.Fields(strings.ToLower(query)) {
		switch {
		case strings.Contains(name, term):
			score += 40
		case strings.Contains(desc, term):
			score += 15
		default:
			score += bigramScore(term, name)*2 + bigramScore(term, desc)
		}
	}
	return score
}

func bigramScore(term, haystack string) int {
	runes := []rune(term)
	if len(runes) < 2 {
		return 0
	}
	score := 0
	for i := 0; i+1 < len(runes); i++ {
		if strings.Contains(haystack, string(runes[i:i+2])) {
			score += 6
		}
	}
	return score
}

// looksAmbiguous 判断结果里是否存在同名不同来源的资源。
func looksAmbiguous(items []scoredSource) bool {
	seen := make(map[string]string, len(items))
	for _, it := range items {
		key := it.Kind + "/" + it.Name
		if prev, ok := seen[key]; ok && prev != it.Source {
			return true
		}
		seen[key] = it.Source
	}
	return false
}

// sliceBytes 按字节区间截取文本，供分页读取使用。
func sliceBytes(s string, offset, limit, defLimit, maxLimit int) string {
	if limit <= 0 {
		limit = defLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(s) {
		return ""
	}
	end := offset + limit
	truncated := end < len(s)
	if !truncated {
		end = len(s)
	}
	out := strings.ToValidUTF8(s[offset:end], "")
	if truncated {
		out += "…（已截断，可用 offset 继续读取）"
	}
	return out
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
