package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/storage"
)

// 企业 wiki：跨空间共享的 markdown 知识区，固定落在系统空间的 files/wiki/ 下，
// 并在里面按智能体分区——每个智能体独占 files/wiki/<智能体 slug>/，工具参数里的
// path 一律相对这个目录。
//
// 为什么按智能体分区：不同智能体的口径、模板、结论文档是各自的事，混在一个池子里
// 会互相污染（同一个「分析口径」在两个智能体下可能完全不同）。分区的边界又与写者
// 天然对齐：写的一方就是会话绑定的那个智能体，所以不需要再引入一套页级 ACL。
//
// 为什么不让它跟着空间走：知识一旦按空间分家，同一个企业会出现 N 份互不相通的口径，
// 而这恰恰是 wiki 要解决的问题。所以宿主固定为系统空间，工具参数里没有 space 维度。
//
// 并发策略（见 wikiWrite）：不引入分布式锁，靠三件事兜住——
//   - 分片：日志类内容建议一天一页 / 一主题一页，写者不重叠；
//   - 版本校对：overwrite 可带 if_version，不匹配就报冲突让模型重读重写；
//   - 进程内串行：同一路径的读改写加互斥锁，挡住单实例内的并发。
//
// 单机部署下这套是安全的；多实例部署时 overwrite 仍可能丢更新，需要存储层支持
// 条件写（S3 的 If-Match）才能真正消除，这里先不做，避免给本地文件系统造一套假锁。
const (
	// wikiRoot 是固定前缀，不接受参数覆盖，否则工具就能写到系统空间别处去
	wikiRoot = "wiki"
	// wikiMaxDepth 限制页面层级：wiki 靠 wiki_list 导航，埋太深模型找不到
	wikiMaxDepth = 3
	// wikiMaxWriteBytes 是单页上限。wiki 是条目不是仓库，页面过大说明该拆分
	wikiMaxWriteBytes = 128 << 10
	// wikiScanFiles / wikiScanBytes 限制一次 list/search 的扫描量
	wikiScanFiles = 500
	wikiScanBytes = 256 << 10
)

// errWikiPath 当模型试图用 file_* 工具读写 wiki/ 时给出改道提示。
//
// file_* 的根是当前空间，直接放行会让用户在各自空间里长出一份同名 wiki，
// 与系统空间那份形成事实上的两个知识库，且两边都以为自己是权威。
var errWikiPath = errors.New("wiki/ 是智能体的知识页区，请改用 wiki_list / wiki_read / wiki_write / wiki_search")

func isWikiPath(rel string) bool {
	return rel == wikiRoot || strings.HasPrefix(rel, wikiRoot+"/")
}

// wikiLocks 按存储 key 串行化「读-改-写」，避免同进程内两个会话互相覆盖。
var wikiLocks sync.Map

func lockWiki(key string) func() {
	v, _ := wikiLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// wikiReady 检查 wiki 是否可用，并返回当前智能体的分区目录名。
//
// 缺宿主空间或缺智能体都明确报错而不是静默降级：前者说明平台还没初始化，
// 后者说明这一轮不知道该读写哪一份知识，随便挑一个目录只会让口径串味。
func wikiReady(env Env) (string, error) {
	if env.Storage == nil {
		return "", fmt.Errorf("文件存储不可用")
	}
	if env.WikiSpaceID == "" {
		return "", fmt.Errorf("企业 wiki 不可用：平台还没初始化系统空间（需要系统管理员登录一次）")
	}
	return wikiAgentDir(env.AgentID)
}

// wikiAgentDir 校验并返回智能体分区目录名（就是智能体插件 slug）。
//
// 它会被拼进存储 key，所以按路径段校验：不允许空、不允许带分隔符、不允许隐藏段，
// 否则一个手滑的 agent_id 就能把知识写到别的分区、甚至跳出 wiki/ 目录。
func wikiAgentDir(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("企业 wiki 不可用：当前会话没有绑定智能体")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("企业 wiki 不可用：会话绑定的智能体标识非法（%q）", raw)
	}
	return name, nil
}

// wikiRel 校验 wiki 内的相对路径：复用工作区的越界与隐藏文件规则，再加深度与后缀。
func wikiRel(raw string, dir bool) (string, error) {
	rel, err := cleanRelPath(raw)
	if err != nil {
		return "", err
	}
	if rel == "" {
		if dir {
			return "", nil
		}
		return "", fmt.Errorf("path 不能为空")
	}
	if depthOfRel(rel) > wikiMaxDepth {
		return "", fmt.Errorf("wiki 路径最多 %d 层，如 topics/报销制度.md", wikiMaxDepth)
	}
	if !dir && strings.ToLower(path.Ext(rel)) != ".md" {
		return "", fmt.Errorf("wiki 只存 markdown 页面（.md），收到 %q", rel)
	}
	return rel, nil
}

func depthOfRel(rel string) int {
	if rel == "" {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

// wikiKey 拼出存储 key：spaces/{系统空间}/files/wiki/{智能体}/{rel}。
// agentDir 必须来自 wikiReady，不能直接取 env.AgentID（未经校验的标识会跳出分区）。
func wikiKey(env Env, agentDir, rel string) string {
	return path.Join(env.Storage.SpaceKey(env.WikiSpaceID), workspaceFilesDir, wikiRoot, agentDir, rel)
}

// contentVersion 是页面版本指纹：取内容 sha256 前 8 字节。
//
// 用内容哈希而不是单调版本号，因为存储层没有版本列；好处是不需要额外元数据，
// 坏处是「改回原样」会被视为同一版本（对冲突检测无害）。
func contentVersion(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

type wikiEntry struct {
	Path string
	Size int64
	Mod  time.Time
}

// readWikiFile 读整页内容；exists=false 表示文件不存在（不是错误）。
func readWikiFile(ctx context.Context, env Env, key string) (string, bool, error) {
	rc, err := env.Storage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	defer func() { _ = rc.Close() }()

	b, err := io.ReadAll(io.LimitReader(rc, wikiMaxWriteBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > wikiMaxWriteBytes {
		return "", false, fmt.Errorf("页面超过 %d 字节，请拆成多个页面", wikiMaxWriteBytes)
	}
	return string(b), true, nil
}

// walkWiki 递归收集某个智能体分区下的 markdown 页面，返回相对该分区的路径。
func walkWiki(ctx context.Context, env Env, agentDir, dirRel string) ([]wikiEntry, error) {
	out := make([]wikiEntry, 0, 16)
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := env.Storage.List(ctx, wikiKey(env, agentDir, rel))
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
		for _, e := range entries {
			name := path.Base(e.Key)
			// 隐藏文件是平台元数据，wiki 里也不暴露
			if strings.HasPrefix(name, ".") {
				continue
			}
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			if e.IsDir {
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			if strings.ToLower(path.Ext(name)) != ".md" {
				continue
			}
			if len(out) >= wikiScanFiles {
				return nil
			}
			out = append(out, wikiEntry{Path: child, Size: e.Size, Mod: e.ModTime})
		}
		return nil
	}
	if err := walk(dirRel); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- 工具

func wikiList() Tool {
	return Tool{
		Name:       "wiki_list",
		Capability: "wiki.list",
		Description: "列出当前智能体知识目录（wiki/<智能体>/）里的页面，路径相对该目录；别的智能体的页面看不到。" +
			"写之前先 list 一遍，避免给同一个主题新建重复页面。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "prefix": {"type": "string", "description": "只列某个子目录，如 topics；省略表示全部"}
  },
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			agentDir, err := wikiReady(env)
			if err != nil {
				return "", err
			}
			var in struct {
				Prefix string `json:"prefix"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("参数解析失败：%w", err)
				}
			}
			rel, err := wikiRel(in.Prefix, true)
			if err != nil {
				return "", err
			}

			pages, err := walkWiki(ctx, env, agentDir, rel)
			if err != nil {
				return "", err
			}
			if len(pages) == 0 {
				return fmt.Sprintf("智能体 %s 的知识目录（wiki/%s/）里还没有页面。", agentDir, agentDir), nil
			}

			rows := make([]map[string]any, 0, len(pages))
			for _, p := range pages {
				rows = append(rows, map[string]any{
					"path":     p.Path,
					"size":     p.Size,
					"modified": p.Mod.Format(time.RFC3339),
				})
			}
			return marshalLimited(rows, 8*1024), nil
		},
	}
}

func wikiRead() Tool {
	return Tool{
		Name:       "wiki_read",
		Capability: "wiki.read",
		Description: "读取当前智能体知识目录下的单个页面（path 相对 wiki/<智能体>/），返回正文与 version。" +
			"overwrite 前必须先用本工具拿到 version，否则并发修改会被覆盖。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对当前智能体知识目录的路径，如 topics/报销制度.md"},
    "offset": {"type": "integer", "description": "起始字节偏移，默认 0；长页面分段读"},
    "limit": {"type": "integer", "description": "本次最多读取的字节数，默认 16384，最大 24576"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:           policy.RiskReadOnly,
		SideEffects:    policy.SideEffectsReadOnly,
		Runtime:        "builtin",
		MaxResultBytes: maxReadBytes,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			agentDir, err := wikiReady(env)
			if err != nil {
				return "", err
			}
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			rel, err := wikiRel(in.Path, false)
			if err != nil {
				return "", err
			}

			body, exists, err := readWikiFile(ctx, env, wikiKey(env, agentDir, rel))
			if err != nil {
				return "", err
			}
			if !exists {
				return "", fmt.Errorf("页面不存在：wiki/%s/%s；可用 wiki_list 查看当前智能体有哪些页面", agentDir, rel)
			}

			version := contentVersion(body)
			offset := in.Offset
			if offset < 0 {
				offset = 0
			}
			if offset > len(body) {
				offset = len(body)
			}
			limit := in.Limit
			if limit <= 0 {
				limit = defaultReadBytes
			}
			if limit > maxReadBytes {
				limit = maxReadBytes
			}
			end := offset + limit
			truncated := end < len(body)
			if !truncated {
				end = len(body)
			}

			var b strings.Builder
			fmt.Fprintf(&b, "path: %s\nagent: %s\nversion: %s\nsize: %d bytes, %d lines\n",
				rel, agentDir, version, len(body), strings.Count(body, "\n")+1)
			if offset > 0 || truncated {
				fmt.Fprintf(&b, "range: %d-%d\n", offset, end)
			}
			b.WriteString("---\n")
			b.WriteString(strings.ToValidUTF8(body[offset:end], ""))
			if truncated {
				fmt.Fprintf(&b, "\n…（已截断，可用 offset=%d 继续读取）", end)
			}
			return b.String(), nil
		},
	}
}

func wikiWrite() Tool {
	return Tool{
		Name:       "wiki_write",
		Capability: "wiki.write",
		Description: "写入当前智能体的知识页（跨空间共享、按智能体隔离：path 相对 wiki/<智能体>/，别的智能体的页面写不进去）。" +
			"团队知识——分析口径、业务流程、结论纪要、可复用 SQL—— 用它沉淀，" +
			"不要在当前空间用 file_write 自建 knowledge/ 之类的知识目录。" +
			"mode：create 仅新建 / overwrite 覆盖（默认）/ append 追加。" +
			"覆盖已有页面时务必带上 wiki_read 返回的 if_version，被他人改过会报冲突；" +
			"日志类内容用 append 追加到按日期命名的页面，天生不冲突。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对当前智能体知识目录的路径，如 topics/报销制度.md"},
    "content": {"type": "string", "description": "要写入的完整 markdown 内容"},
    "mode": {"type": "string", "enum": ["create", "overwrite", "append"], "description": "写入方式，默认 overwrite"},
    "if_version": {"type": "string", "description": "可选，覆盖前校验版本；不匹配则拒绝写入"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 写的是系统空间固定前缀，跨空间共享但范围锁死，按空间写权限放行
		SideEffects: policy.SideEffectsShared,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			agentDir, err := wikiReady(env)
			if err != nil {
				return "", err
			}
			var in struct {
				Path      string `json:"path"`
				Content   string `json:"content"`
				Mode      string `json:"mode"`
				IfVersion string `json:"if_version"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			rel, err := wikiRel(in.Path, false)
			if err != nil {
				return "", err
			}

			key := wikiKey(env, agentDir, rel)
			unlock := lockWiki(key)
			defer unlock()

			existing, exists, err := readWikiFile(ctx, env, key)
			if err != nil {
				return "", err
			}

			mode := strings.ToLower(strings.TrimSpace(in.Mode))
			if mode == "" {
				mode = "overwrite"
			}

			body := in.Content
			switch mode {
			case "create":
				if exists {
					return "", fmt.Errorf("页面已存在：%s；如需修改请先 wiki_read 再用 overwrite 带 if_version", rel)
				}
			case "overwrite":
				if want := strings.TrimSpace(in.IfVersion); want != "" {
					if !exists {
						return "", fmt.Errorf("写入冲突：%s 已被删除（你读到的是 %s）；请重新 wiki_list 确认后再写", rel, want)
					}
					if cur := contentVersion(existing); cur != want {
						return "", fmt.Errorf("写入冲突：%s 已被他人修改（当前 version=%s，你读到的是 %s）。"+
							"请重新 wiki_read 拿到最新内容后重写；若只是追加信息，改用 mode=append", rel, cur, want)
					}
				}
			case "append":
				if exists {
					if !strings.HasSuffix(existing, "\n") {
						existing += "\n"
					}
					body = existing + body
				}
			default:
				return "", fmt.Errorf("mode 只能是 create / overwrite / append")
			}

			if len(body) > wikiMaxWriteBytes {
				return "", fmt.Errorf("页面超过 %d 字节，请拆成多个页面", wikiMaxWriteBytes)
			}
			if _, err := env.Storage.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
				return "", err
			}

			return fmt.Sprintf("已写入 wiki/%s/%s（%d 字节，mode=%s，version=%s）",
				agentDir, rel, len(body), mode, contentVersion(body)), nil
		},
	}
}

func wikiSearch() Tool {
	return Tool{
		Name:       "wiki_search",
		Capability: "wiki.search",
		Description: "在当前智能体的知识目录里按关键词找页面和所在行，适合回答前定位「这件事记在哪个页面」。" +
			"命中结果给出 path，再用 wiki_read 读全文；别的智能体的知识不在检索范围内。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "关键词，大小写不敏感，按行匹配"},
    "limit": {"type": "integer", "description": "最多返回多少条命中，默认 8，最大 20"}
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			agentDir, err := wikiReady(env)
			if err != nil {
				return "", err
			}
			var in struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			query := strings.TrimSpace(in.Query)
			if query == "" {
				return "", fmt.Errorf("query 不能为空")
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 8
			}
			if limit > 20 {
				limit = 20
			}

			pages, err := walkWiki(ctx, env, agentDir, "")
			if err != nil {
				return "", err
			}
			if len(pages) == 0 {
				return fmt.Sprintf("智能体 %s 的知识目录（wiki/%s/）里还没有页面。", agentDir, agentDir), nil
			}

			needle := strings.ToLower(query)
			hits := make([]map[string]any, 0, limit)
			for _, page := range pages {
				if len(hits) >= limit {
					break
				}
				body, exists, err := readWikiFile(ctx, env, wikiKey(env, agentDir, page.Path))
				if err != nil || !exists {
					continue
				}
				for i, line := range strings.Split(body, "\n") {
					if !strings.Contains(strings.ToLower(line), needle) {
						continue
					}
					hits = append(hits, map[string]any{
						"path": page.Path,
						"line": i + 1,
						"text": clipRunes(strings.TrimSpace(line), 160),
					})
					if len(hits) >= limit {
						break
					}
				}
			}
			if len(hits) == 0 {
				return fmt.Sprintf("没有页面命中 %q；可用 wiki_list 浏览智能体 %s 的全部页面。", query, agentDir), nil
			}
			return marshalLimited(hits, 8*1024), nil
		},
	}
}

// clipRunes 按字符截断，避免把中文切成半个字。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
