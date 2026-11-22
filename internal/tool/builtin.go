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
	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/storage"
)

// builtinTools 返回平台内置能力实现。
//
// 除 fs.write 外全部只读（L1）。fs.write 的风险等级是 L3，但副作用范围被限制在
// 当前空间文件根下（SideEffectsWorkspace），策略层按空间权限放行，见 policy.RBACEngine。
func builtinTools() []Tool {
	return []Tool{
		spaceList(),
		sessionList(),
		taskCreate(),
		taskList(),
		taskRemind(),
		taskCancel(),
		taskReply(),
		fileList(),
		fileRead(),
		fileWrite(),
		wikiList(),
		wikiRead(),
		wikiWrite(),
		wikiSearch(),
		pluginSearch(),
		pluginRead(),
		webSearch(),
		shellExec(),
		pluginTry(),
		secretRequest(),
		mcpAppKey(),
	}
}

// Manifest 是内置工具的声明式定义，写入系统空间的 tools/ 目录，作为全局搜索源。
type Manifest struct {
	Name        string `json:"name"`
	Capability  string `json:"capability"`
	Description string `json:"description"`
	Runtime     string `json:"runtime"`
	Risk        int    `json:"risk_level"`
	// SideEffects 与 Tool.SideEffects 同义：内置工具的风险要能传到插件体系那边，
	// 否则市场里会把「写空间文件」的工具显示成只读。
	SideEffects string          `json:"side_effects"`
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
			SideEffects: t.SideEffects,
			Parameters:  t.Parameters,
		})
	}
	return out
}

// 系统空间的固定目录名；与 space 包保持一致，这里用字面量避免包间循环依赖。
const systemSpaceType = "system"

func spaceList() Tool {
	return Tool{
		Name:        "space_list",
		Capability:  "space.list",
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
			out := make([]map[string]any, 0, in.Limit)
			for _, sess := range sessions {
				// 会话只对创建者可见（§15.12.3）：这个工具不能成为看别人会话的口子
				if sess.CreatedBy != "" && sess.CreatedBy != env.UserID {
					continue
				}
				if len(out) >= in.Limit {
					break
				}
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

// pluginSearch 按关键词检索「当前空间 + 系统空间」的插件。
//
// 插件总量没有上限，因此不提供「列出全部」的工具：模型按关键词检索，
// 命中同名多来源时必须在回答前让用户确认（约束写在系统提示词里）。
func pluginSearch() Tool {
	return Tool{
		Name:       "plugin_search",
		Capability: "plugin.search",
		Description: "按关键词检索可用的插件：技能（skills）、工具（tools）、MCP 服务（mcps）与组件（components），返回最相关的若干条及读取路径。" +
			"插件总量没有上限，不要试图列举全部；需要某类能力但不确定叫什么时，用用户原话里的关键词检索。" +
			"不确定有什么可用能力时，可以省略 query 只给 kind，按名称列出该类插件再挑。" +
			"结果里 source=agent 的是当前智能体自带的内置技能，别的智能体看不到。" +
			"命中多个同名插件时先让用户选择，不要自行假设。",
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
			if env.Plugins == nil {
				return "", fmt.Errorf("插件目录不可用")
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
			kind := normalizePluginKind(in.Kind)
			if in.Kind != "" && kind == "" {
				return "", fmt.Errorf("kind 只能是 skills / tools / mcps / components")
			}
			// 不再常驻技能索引，所以允许「只给 kind」按名称拉取该类目录
			if query == "" && kind == "" {
				return "", fmt.Errorf("query 与 kind 至少提供一个：给关键词检索，或给 kind 列出该类插件")
			}
			if in.Limit <= 0 {
				in.Limit = 8
			}
			if in.Limit > 20 {
				in.Limit = 20
			}

			entries, err := env.Plugins.Plugins(ctx)
			if err != nil {
				return "", err
			}
			hits := make([]scoredPlugin, 0, len(entries))
			for _, e := range entries {
				if kind != "" && e.Kind != kind {
					continue
				}
				if query == "" {
					// 只给 kind：按名称列目录，score 取相同值让排序退化为 kind + name
					hits = append(hits, scoredPlugin{PluginEntry: e, Score: 1})
					continue
				}
				if score := scorePlugin(query, e); score > 0 {
					hits = append(hits, scoredPlugin{PluginEntry: e, Score: score})
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

			res := pluginSearchResult{Query: query, Matched: len(hits), Items: []scoredPlugin{}}
			if len(hits) > in.Limit {
				hits = hits[:in.Limit]
				res.Truncated = true
			}
			res.Items = append(res.Items, hits...)
			switch {
			case res.Matched == 0:
				res.Hint = "没有匹配的插件，可换关键词重试；当前空间的文件用 file_list 查看"
			case query == "":
				res.Hint = "未按关键词过滤，按名称列出；用 query 可以缩小范围"
			case looksAmbiguous(res.Items):
				res.Hint = "存在同名插件（来源不同），使用前请让用户确认用哪一个"
			}
			body, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return "", err
			}
			return clamp(string(body), 8*1024), nil
		},
	}
}

// pluginRead 读取插件文件：当前空间优先，找不到再到系统空间找。
//
// 空间里的技能草稿（plugins/<技能slug>/SKILL.md）与已安装件（published/skills/<slug>/<version>/SKILL.md）
// 用同一个工具读取，模型不需要关心插件在哪一侧。
func pluginRead() Tool {
	return Tool{
		Name:       "plugin_read",
		Capability: "plugin.read",
		Description: "读取插件文件正文（技能的 SKILL.md、工具定义或 MCP 快照 JSON）。" +
			"路径取自 plugin_search；当前空间存在同名文件时优先，否则读系统空间；长文档可用 offset / limit 分段读取。",
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
			if env.Plugins == nil {
				return "", fmt.Errorf("插件目录不可用")
			}
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			text, err := env.Plugins.ReadPlugin(ctx, in.Path)
			if err != nil {
				return "", err
			}
			return sliceBytes(text, in.Offset, in.Limit, 16*1024, 24*1024), nil
		},
	}
}

// scoredPlugin 是带匹配分数的检索结果；Score 只在内部排序用，不返回给模型。
type scoredPlugin struct {
	PluginEntry
	Score int `json:"-"`
}

type pluginSearchResult struct {
	Query     string         `json:"query"`
	Matched   int            `json:"matched"`
	Truncated bool           `json:"truncated,omitempty"`
	Items     []scoredPlugin `json:"items"`
	Hint      string         `json:"hint,omitempty"`
}

// normalizePluginKind 兼容单复数写法，返回索引里使用的复数形式。
func normalizePluginKind(kind string) string {
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

// scorePlugin 计算插件与关键词的相关度：名称命中权重最高，其次描述。
//
// 中文查询通常不带空格，因此除整词匹配外，再按相邻两字（bigram）命中计分。
func scorePlugin(query string, e PluginEntry) int {
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

// looksAmbiguous 判断结果里是否存在同名不同来源的插件。
func looksAmbiguous(items []scoredPlugin) bool {
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
			// wiki 是系统空间里的知识页区（按智能体分区），file_* 只认当前空间，
			// 放行会让两处各长一份
			if isWikiPath(rel) {
				return "", errWikiPath
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
				// 平台保留的隐藏项（.meta.json / .install.json / .history）是后端元数据，不暴露给模型；
				// 用户自己的隐藏文件（.env 等）正常列出，与普通文件一致。
				if spacefs.Reserved(name) {
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

// taskCreate 把一件事派发给指定的人：平台会为他单独建一个会话。
//
// 派发是「异步」的：调用返回后当前这一轮就正常结束，用户可以在本会话继续问别的事；
// 对方回复后，平台会把回执作为一条消息送回本会话并自动续上一轮。
func taskCreate() Tool {
	return Tool{
		Name:       "task_create",
		Capability: "task.create",
		Description: "把一件需要他人确认或处理的事派发出去（例如请数据负责人确认口径）。" +
			"任务落在对方的个人空间里，他不需要是当前空间的成员，没用过平台的人也会被自动开通个人空间。" +
			"派发后本轮会正常结束，用户仍可继续对话；对方确认结论后回执会自动回到本会话。" +
			"对方看不到本空间的文件，background / done 里需要引用的内容必须直接写出来。" +
			"分段填：background 说清需求与口径，done 写已经做到哪一步，checks 列出要他逐条确认的点。" +
			"assignee 传姓名、工号或用户 id，匹配到多人时工具会报错并要求用工号指定。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "一句话说明要对方做什么，会作为子会话标题"},
    "background": {"type": "string", "description": "需求背景：为什么要、口径与范围是什么；不要只重复 title"},
    "done": {"type": "string", "description": "已经完成的实现或初步结论，可贴 SQL/代码；对方看不到本空间文件，内容要自包含"},
    "checks": {"type": "array", "items": {"type": "string"}, "description": "需要对方逐条确认的点，1-5 条，每条一句话"},
    "after": {"type": "string", "description": "确认后打算怎么用（例如沉淀到知识库）；不写则默认把结论带回发起方会话"},
    "brief": {"type": "string", "description": "其它补充说明；新调用优先用 background / done / checks"},
    "assignee": {"type": "string", "description": "被派发人的姓名、工号或用户 id"}
  },
  "required": ["title", "assignee"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskWrite,
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Tasks == nil {
				return "", fmt.Errorf("任务派发不可用")
			}
			var in TaskDispatchInput
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			task, err := env.Tasks.Dispatch(ctx, env.UserID, env.SessionID, in)
			if err != nil {
				return "", err
			}
			return marshalLimited(map[string]any{
				"task_id": task.ID,
				"title":   task.Title,
				"status":  task.Status,
				"note":    "任务已派发，请告知用户：对方确认结论后结果会自动回到本会话。",
			}, 4*1024), nil
		},
	}
}

// taskList 查看本会话派发出的任务及其等待时长。
func taskList() Tool {
	return Tool{
		Name:        "task_list",
		Capability:  "task.list",
		Description: "列出本会话派发出去的任务与当前状态。用户问「那个任务怎么样了」时使用。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.RiskReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, _ json.RawMessage, env Env) (string, error) {
			if env.Tasks == nil {
				return "", fmt.Errorf("任务派发不可用")
			}
			tasks, err := env.Tasks.Tasks(ctx, env.UserID, env.SessionID)
			if err != nil {
				return "", err
			}
			if len(tasks) == 0 {
				return "本会话还没有派发过任务。", nil
			}
			return marshalLimited(tasks, 8*1024), nil
		},
	}
}

// taskCancel 取消一个尚未完成的任务。
func taskCancel() Tool {
	return Tool{
		Name:        "task_cancel",
		Capability:  "task.cancel",
		Description: "取消一个还没完成的任务。已经拿到结果或已被取消的任务不可再取消。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {"task_id": {"type": "string", "description": "task_list 返回的任务 id"}},
  "required": ["task_id"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskWrite,
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Tasks == nil {
				return "", fmt.Errorf("任务派发不可用")
			}
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			if err := env.Tasks.CancelTask(ctx, env.UserID, in.TaskID); err != nil {
				return "", err
			}
			return `{"status":"cancelled"}`, nil
		},
	}
}

// taskRemind 是「同一件事再问一次」的正确路径：催办原会话，不新建。
// 没有它，模型想追问时只能再调 task_create，对方空间里就会堆出一串相似会话。
func taskRemind() Tool {
	return Tool{
		Name:       "task_remind",
		Capability: "task.remind",
		Description: "同一个任务需要再问一次时用这个，不要重新派发：向原任务会话追加一条提醒，" +
			"不会新建会话、不改变被派发人。先用 task_list 找到那个任务的 id。" +
			"如果对方已经确认结论，回执会自动回来，不需要催。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {"task_id": {"type": "string", "description": "task_list 返回的任务 id"}},
  "required": ["task_id"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskWrite,
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Tasks == nil {
				return "", fmt.Errorf("任务派发不可用")
			}
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			task, err := env.Tasks.RemindTask(ctx, env.UserID, in.TaskID)
			if err != nil {
				return "", err
			}
			return marshalLimited(map[string]any{
				"task_id":        task.ID,
				"status":         task.Status,
				"dispatch_count": task.DispatchCount,
				"note":           "已催办，对方确认结论后回执会回到本会话。",
			}, 4*1024), nil
		},
	}
}

// taskReply 是子会话侧的正式回执：只有被派发人确认结论后才该调用。
//
// 平台不在每轮结束时自动回填，所以这是任务从 waiting_external 走到 succeeded 的
// 唯一出口；不调它，发起方会一直看到「等待中」。
func taskReply() Tool {
	return Tool{
		Name:       "task_reply",
		Capability: "task.reply",
		Description: "把确认过的结论回执给发起人，任务由此结束。" +
			"只有本会话是别人派发来的任务时才有用；必须在用户明确确认结论之后调用，" +
			"不要一有回答就调，也不要用它发送中间态的反问、草稿或未确认的内容。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "要回传的完整结论：结论、依据、还需要发起人确认什么，脱离上下文也能看懂"}
  },
  "required": ["summary"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskWrite,
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Tasks == nil {
				return "", fmt.Errorf("任务派发不可用")
			}
			var in struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			task, err := env.Tasks.ReplyTask(ctx, env.UserID, env.SessionID, in.Summary)
			if err != nil {
				return "", err
			}
			return marshalLimited(map[string]any{
				"status": task.Status,
				"note":   "已回执给发起人，本会话可以继续聊别的事。",
			}, 4*1024), nil
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
