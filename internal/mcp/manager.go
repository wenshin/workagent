package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// Manager 管理已注册的 MCP Server：拉取工具清单、缓存、代理调用（§5.3.1）。
type Manager struct {
	store store.Store
	log   *slog.Logger

	mu      sync.RWMutex
	clients map[string]*Client // serverID → 已握手的连接
}

// NewManager 构造管理器。
func NewManager(st store.Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: st, log: log, clients: make(map[string]*Client)}
}

// Sync 遍历全部 MCP Server，握手并拉取工具清单后落缓存。
//
// 单个 Server 失败不影响其他 Server，也不阻断启动。
func (m *Manager) Sync(ctx context.Context) error {
	servers, err := m.store.ListMCPServers(ctx)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return nil
	}

	failed := 0
	for _, server := range servers {
		defs, err := m.fetchTools(ctx, server)
		if err != nil {
			failed++
			m.log.Warn("MCP Server 工具同步失败", "server", server.Name, "err", err)
			m.dropClient(server.ID)
			continue
		}

		cache, _ := json.Marshal(defs)
		if err := m.store.UpdateMCPToolsCache(ctx, server.ID, cache); err != nil {
			m.log.Warn("写入 MCP 工具缓存失败", "server", server.Name, "err", err)
			continue
		}
		m.log.Info("MCP Server 已同步", "server", server.Name, "tools", len(defs))
	}

	if failed > 0 {
		m.log.Warn("部分 MCP Server 同步失败", "failed", failed, "total", len(servers))
	}
	return nil
}

// Tools 返回所有远端工具，数据源为本地缓存，调用链上不再访问远端。
func (m *Manager) Tools(ctx context.Context) ([]tool.MCPTool, error) {
	servers, err := m.store.ListMCPServers(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]tool.MCPTool, 0, 16)
	for _, server := range servers {
		if len(server.ToolsCache) == 0 {
			continue
		}
		var defs []ToolDef
		if err := json.Unmarshal(server.ToolsCache, &defs); err != nil {
			m.log.Warn("MCP 工具缓存解析失败", "server", server.Name, "err", err)
			continue
		}
		for _, def := range defs {
			out = append(out, tool.MCPTool{
				ServerID:    server.ID,
				ServerName:  server.Name,
				Name:        def.Name,
				Description: def.Description,
				InputSchema: def.InputSchema,
			})
		}
	}
	return out, nil
}

// Call 代理一次远端工具调用。
func (m *Manager) Call(ctx context.Context, serverID, toolName string, args json.RawMessage) (string, error) {
	servers, err := m.store.ListMCPServers(ctx)
	if err != nil {
		return "", err
	}

	var target *store.MCPServer
	for i := range servers {
		if servers[i].ID == serverID {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return "", errServerNotFound
	}

	client, err := m.client(ctx, *target)
	if err != nil {
		return "", err
	}
	return client.CallTool(ctx, toolName, args)
}

func (m *Manager) client(ctx context.Context, server store.MCPServer) (*Client, error) {
	m.mu.RLock()
	existing, ok := m.clients[server.ID]
	m.mu.RUnlock()
	if ok {
		return existing, nil
	}

	if server.Transport != "" && server.Transport != "streamable_http" {
		return nil, errUnsupportedTransport
	}

	client := NewClient(server.Name, server.Endpoint, server.AuthHeader, server.SecretRef)
	if err := client.Initialize(ctx); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.clients[server.ID] = client
	m.mu.Unlock()
	return client, nil
}

func (m *Manager) fetchTools(ctx context.Context, server store.MCPServer) ([]ToolDef, error) {
	client, err := m.client(ctx, server)
	if err != nil {
		return nil, err
	}
	return client.ListTools(ctx)
}

func (m *Manager) dropClient(serverID string) {
	m.mu.Lock()
	delete(m.clients, serverID)
	m.mu.Unlock()
}
