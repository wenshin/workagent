package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/workagent/auto-agent/internal/mcp"
)

// mcpRuntime 是一个插件化的 MCP 连接：按 manifest 的白名单暴露工具。
//
// 与 §5.3 的全局 MCP Gateway 不冲突：那是平台统一管理的连接，
// 这里是「某个空间自己装的一个 MCP 服务」，鉴权与出网域名都来自插件能力清单。
type mcpRuntime struct {
	client *mcp.Client
	tools  map[string]mcp.ToolDef // 模型可见名 → 定义
	remote map[string]string      // 模型可见名 → 远端工具名
}

// newMCPRuntime 连接远端并同步工具清单。
//
// cachePath 是上次成功拉取的工具清单：远端抖动时沿用快照，至少让清单不消失
// （调用本身仍会失败，模型能看到工具暂时不可用，而不是整个插件凭空蒸发）。
func newMCPRuntime(ctx context.Context, meta Manifest, secrets Secrets, log *slog.Logger, cachePath string) (*mcpRuntime, error) {
	if meta.Transport == nil {
		return nil, fmt.Errorf("MCP 插件缺少 transport")
	}
	var authHeader, secret string
	if meta.Auth != nil {
		authHeader = meta.Auth.Name
		if meta.Auth.Secret != "" && secrets != nil {
			v, err := secrets.Get(ctx, "plugin", meta.Auth.Secret)
			if err != nil {
				log.Warn("MCP 插件引用的密钥不存在", "slug", meta.Slug, "secret", meta.Auth.Secret, "err", err)
			} else {
				secret = v
			}
		}
	}

	client := mcp.NewClient(meta.Slug, meta.Transport.URL, authHeader, secret)
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := client.Initialize(callCtx); err != nil {
		return nil, err
	}
	defs, err := client.ListTools(callCtx)
	if err != nil {
		if cached, ok := readMCPCache(cachePath); ok {
			log.Warn("MCP 工具清单拉取失败，沿用上次快照", "slug", meta.Slug, "err", err)
			return buildMCPRuntime(client, meta, cached)
		}
		return nil, err
	}
	writeMCPCache(cachePath, defs, log)
	return buildMCPRuntime(client, meta, defs)
}

func buildMCPRuntime(client *mcp.Client, meta Manifest, defs []mcp.ToolDef) (*mcpRuntime, error) {
	rt := &mcpRuntime{
		client: client,
		tools:  map[string]mcp.ToolDef{},
		remote: map[string]string{},
	}
	for _, def := range defs {
		// 白名单支持尾部 `*`：远端工具随时会加，逐个列全不现实；deny 仍然优先。
		if !meta.AllowsTool(def.Name) {
			continue
		}
		rt.tools[def.Name] = def
		rt.remote[def.Name] = def.Name
	}
	if len(rt.tools) == 0 {
		return nil, fmt.Errorf("MCP 服务没有暴露白名单内的工具（tools.allow 与实际清单不匹配）")
	}
	return rt, nil
}

func readMCPCache(cachePath string) ([]mcp.ToolDef, bool) {
	if cachePath == "" {
		return nil, false
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	var defs []mcp.ToolDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, false
	}
	return defs, len(defs) > 0
}

func writeMCPCache(cachePath string, defs []mcp.ToolDef, log *slog.Logger) {
	if cachePath == "" {
		return
	}
	raw, err := json.Marshal(defs)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		log.Warn("写入 MCP 工具快照失败", "err", err)
		return
	}
	if err := os.WriteFile(cachePath, raw, 0o600); err != nil {
		log.Warn("写入 MCP 工具快照失败", "err", err)
	}
}

func (r *mcpRuntime) names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *mcpRuntime) call(ctx context.Context, remote string, args json.RawMessage) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return r.client.CallTool(callCtx, remote, args)
}
