package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/storage"
)

// Agent 的空间文件边界：只能读写 spaces/{space_id}/files/ 下的文本文件。
//
// 三层约束：
//   - 路径相对空间文件根，禁止绝对路径、`..` 与隐藏文件（.meta.json 由平台维护）
//   - 只放行 md / json / txt / yaml 与插件脚本（ts / js），二进制不进模型上下文
//   - 单次读写有字节上限，避免超长内容撑爆上下文
//
// 空间 ID 来自会话所属空间（tool.Env.SpaceID），工具自身不接受 space_id 参数，
// 因此不存在跨空间读写的能力面。
const (
	// workspaceFilesDir 与 space 包的 dirsRoot 一致；用字面量避免包间循环依赖
	workspaceFilesDir = "files"

	defaultReadBytes = 16 * 1024
	// 留出余量，避免正文加上元信息后触发 Invoker 的 32KB 输出截断
	maxReadBytes  = 24 * 1024
	maxWriteBytes = 256 * 1024
)

var workspaceExts = map[string]bool{
	".md":   true,
	".json": true,
	".txt":  true,
	".yaml": true,
	".yml":  true,
	// 插件脚本：工具插件入口是 index.ts、脚本型搜索源是 main.ts，
	// 白名单里没有它们，模型在会话里根本写不出一个能跑的工具插件。
	".ts": true,
	".js": true,
}

// cleanRelPath 归一化并校验相对路径，拒绝越界与隐藏段。
func cleanRelPath(raw string) (string, error) {
	rel := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("路径非法：请使用相对空间文件根目录的路径")
	}
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return "", nil
	}
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "..":
			return "", fmt.Errorf("路径非法：不允许跳出空间文件目录")
		case seg == "" || seg == ".":
			return "", fmt.Errorf("路径非法")
		case strings.HasPrefix(seg, "."):
			return "", fmt.Errorf("路径非法：不允许访问隐藏文件（%s）", seg)
		}
	}
	return rel, nil
}

// workspaceDirPath 校验目录参数：允许空（表示文件根）。
func workspaceDirPath(raw string) (string, error) {
	return cleanRelPath(raw)
}

// workspaceFilePath 校验文件参数：必须带受支持的后缀。
func workspaceFilePath(raw string) (string, error) {
	rel, err := cleanRelPath(raw)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	if !workspaceExts[strings.ToLower(path.Ext(rel))] {
		return "", fmt.Errorf("仅支持 md / json / txt / yaml / ts / js 文件")
	}
	return rel, nil
}

// workspaceKey 拼出存储 key：spaces/{space_id}/files/{rel}。
func workspaceKey(env Env, rel string) string {
	return path.Join(env.Storage.SpaceKey(env.SpaceID), workspaceFilesDir, rel)
}

func fileRead() Tool {
	return Tool{
		Name:       "file_read",
		Capability: "fs.read",
		Description: "读取当前空间内的文本文件（md / json / txt / yaml / ts / js）。path 相对空间文件根目录，" +
			"如 plugins/weekly_report/SKILL.md。长文件可用 offset / limit 分段读取。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间文件根目录的路径，如 tools/xxx.json"},
    "offset": {"type": "integer", "description": "起始字节偏移，默认 0"},
    "limit": {"type": "integer", "description": "本次最多读取的字节数，默认 16384，最大 24576"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		// 读类工具允许更大的回填额度，避免长文档被 4KB 默认值截断
		MaxResultBytes: maxReadBytes,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Storage == nil || env.SpaceID == "" {
				return "", fmt.Errorf("文件存储不可用")
			}
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("参数解析失败：%w", err)
				}
			}

			rel, err := workspaceFilePath(in.Path)
			if err != nil {
				return "", err
			}
			if isWikiPath(rel) {
				return "", errWikiPath
			}
			if err := ensurePluginMutable(ctx, env, rel); err != nil {
				return "", err
			}
			key := workspaceKey(env, rel)

			entry, err := env.Storage.Stat(ctx, key)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					return "文件不存在：" + rel + "（可用 file_list 查看目录内容）", nil
				}
				return "", err
			}
			if entry.IsDir {
				return rel + " 是目录，不是文件；请用 file_list 查看其内容。", nil
			}

			limit := in.Limit
			if limit <= 0 {
				limit = defaultReadBytes
			}
			if limit > maxReadBytes {
				limit = maxReadBytes
			}
			offset := in.Offset
			if offset < 0 {
				offset = 0
			}

			f, err := env.Storage.Get(ctx, key)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					return "文件不存在：" + rel, nil
				}
				return "", err
			}
			defer func() { _ = f.Close() }()

			if offset > 0 {
				if _, err := io.CopyN(io.Discard, f, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
					return "", err
				}
			}
			data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
			if err != nil {
				return "", err
			}

			var b strings.Builder
			fmt.Fprintf(&b, "[文件] %s（共 %d 字节，本次读取 %d 字节，起始 %d）\n", rel, entry.Size, len(data), offset)
			b.WriteString(string(data))
			if rest := entry.Size - int64(offset) - int64(len(data)); rest > 0 {
				fmt.Fprintf(&b, "\n…（还有 %d 字节未读，可用 offset=%d 继续）", rest, offset+len(data))
			}
			return b.String(), nil
		},
	}
}

// ensurePluginMutable 阻止写入已安装插件目录：安装副本只读，只能启停/卸载。
func ensurePluginMutable(ctx context.Context, env Env, rel string) error {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] != "plugins" {
		return nil
	}
	if env.Store == nil || env.SpaceID == "" {
		return fmt.Errorf("文件存储不可用")
	}
	installs, err := env.Store.ListPluginInstalls(ctx, env.SpaceID)
	if err != nil {
		return err
	}
	slug := parts[1]
	for _, in := range installs {
		if in.Slug == slug {
			return fmt.Errorf("插件 %s 已安装，目录为只读；请先卸载或改为本地开发插件", slug)
		}
	}
	return nil
}

func fileWrite() Tool {
	return Tool{
		Name:       "file_write",
		Capability: "fs.write",
		Description: "写入当前空间内的文本文件（md / json / txt / yaml / ts / js），path 相对空间文件根目录，" +
			"父目录不存在时自动创建。mode：overwrite 覆盖（默认）/ append 追加 / create 仅新建（已存在则失败）。" +
			"修改已有文件前建议先用 file_read 读取现有内容。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "相对空间文件根目录的路径，如 plugins/weekly_report/SKILL.md"},
    "content": {"type": "string", "description": "要写入的完整文本内容"},
    "mode": {"type": "string", "enum": ["overwrite", "append", "create"], "description": "写入方式，默认 overwrite"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 仅作用于当前空间工作区，策略据此放行（仍需 write / manage 空间权限）
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Storage == nil || env.SpaceID == "" {
				return "", fmt.Errorf("文件存储不可用")
			}
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
				Mode    string `json:"mode"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}

			rel, err := workspaceFilePath(in.Path)
			if err != nil {
				return "", err
			}
			if isWikiPath(rel) {
				return "", errWikiPath
			}
			if err := ensurePluginMutable(ctx, env, rel); err != nil {
				return "", err
			}
			key := workspaceKey(env, rel)

			existing, statErr := env.Storage.Stat(ctx, key)
			exists := statErr == nil
			if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
				return "", statErr
			}
			if exists && existing.IsDir {
				return "", fmt.Errorf("%s 是目录，不能当文件写入", rel)
			}

			mode := strings.ToLower(strings.TrimSpace(in.Mode))
			if mode == "" {
				mode = "overwrite"
			}

			body := in.Content
			switch mode {
			case "create":
				if exists {
					return "", fmt.Errorf("文件已存在：%s；如需覆盖请用 mode=overwrite", rel)
				}
			case "overwrite":
				// 直接整体覆盖
			case "append":
				if exists {
					rc, err := env.Storage.Get(ctx, key)
					if err != nil {
						return "", err
					}
					prev, readErr := io.ReadAll(io.LimitReader(rc, maxWriteBytes))
					_ = rc.Close()
					if readErr != nil {
						return "", readErr
					}
					if len(prev) >= maxWriteBytes {
						return "", fmt.Errorf("文件已达 %d 字节上限，无法继续追加；可用 mode=overwrite 重写", maxWriteBytes)
					}
					body = string(prev) + body
				}
			default:
				return "", fmt.Errorf("mode 只能是 overwrite / append / create")
			}

			if len(body) > maxWriteBytes {
				return "", fmt.Errorf("内容过大（%d 字节），单文件上限 %d 字节", len(body), maxWriteBytes)
			}
			if _, err := env.Storage.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
				return "", err
			}
			// 可能新增了技能目录或工具清单，刷新索引让 Agent 与本轮后续调用立刻可见
			if env.Sources != nil {
				if err := env.Sources.RefreshIndex(ctx); err != nil && env.Log != nil {
					env.Log.Warn("刷新空间索引失败", "space_id", env.SpaceID, "path", rel, "err", err)
				}
			}
			return fmt.Sprintf("已写入 %s（%d 字节，mode=%s）", rel, len(body), mode), nil
		},
	}
}
