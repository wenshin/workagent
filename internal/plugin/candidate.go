package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/workagent/auto-agent/internal/store"
)

// EnableLocal 启用空间本地插件：进入本空间运行池，并作为「待发布」候选出现。
//
// 内容仍以空间文件为准；候选只记录展示与状态。启用前检查同 slug 的安装记录，
// 避免已安装副本与本地目录同时生效。
func (m *Manager) EnableLocal(ctx context.Context, spaceID, slug, userID, authorName string) (store.PluginCandidate, error) {
	if _, err := m.installOf(ctx, spaceID, slug); err == nil {
		return store.PluginCandidate{}, fmt.Errorf("%w：同名插件已安装到本空间，不能再启用本地插件", ErrConflict)
	}
	b, err := LoadBundle(ctx, m.o.Storage, DraftKey(m.o.Storage, spaceID, slug), false)
	if err != nil {
		return store.PluginCandidate{}, err
	}
	risk, _ := Derive(b.Meta)
	caps, _ := json.Marshal(b.Meta.Capabilities)
	c, err := m.o.Store.UpsertPluginCandidate(ctx, store.PluginCandidate{
		SpaceID: spaceID, Slug: slug, Kind: string(b.Meta.Kind),
		Name: b.Meta.Name, Description: b.Meta.Description,
		AuthorID: userID, AuthorName: authorName,
		ManifestHash: b.Hash, Risk: risk, Capabilities: caps,
		Status: store.PluginCandidateEnabled,
	})
	if err != nil {
		return store.PluginCandidate{}, err
	}
	m.invalidate(spaceID, slug)
	// 本地插件也可能是搜索源：启用即生效，重载本空间完成注册
	if err := m.reloadSpace(ctx, spaceID); err != nil {
		m.o.Logger.Warn("重载空间失败", "space", spaceID, "err", err)
	}
	m.notify(ctx, spaceID)
	m.recordAudit(ctx, userID, "plugin.local.enable", slug, "enabled", map[string]any{"space": spaceID})
	return c, nil
}

// DisableLocal 停用空间本地插件：立即从检索与工具列表移除，内容保留。
func (m *Manager) DisableLocal(ctx context.Context, spaceID, slug, userID string) error {
	c, err := m.o.Store.GetPluginCandidate(ctx, spaceID, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	c.Status = store.PluginCandidateDisabled
	if _, err := m.o.Store.UpsertPluginCandidate(ctx, c); err != nil {
		return err
	}
	m.invalidate(spaceID, slug)
	global := false
	if sysID, err := m.systemSpaceID(ctx); err == nil && sysID != "" && sysID == spaceID {
		global = true
	}
	m.unregisterSource(spaceID, slug, global)
	m.notify(ctx, spaceID)
	m.recordAudit(ctx, userID, "plugin.local.disable", slug, "disabled", map[string]any{"space": spaceID})
	return nil
}

// LocalEnabledMap 返回空间内已启用的本地插件 slug，供索引扫描过滤。
func (m *Manager) LocalEnabledMap(ctx context.Context, spaceID string) (map[string]bool, error) {
	items, err := m.o.Store.ListPluginCandidatesBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(items))
	for _, c := range items {
		if c.Status == store.PluginCandidateEnabled {
			out[c.Slug] = true
		}
	}
	return out, nil
}

// LocalCandidates 返回空间内全部候选，便于面板展示「已发布 / 有新版本」。
func (m *Manager) LocalCandidates(ctx context.Context, spaceID string) ([]store.PluginCandidate, error) {
	return m.o.Store.ListPluginCandidatesBySpace(ctx, spaceID)
}

// ListCandidates 返回全平台候选，市场「待发布」分类使用。
func (m *Manager) ListCandidates(ctx context.Context) ([]store.PluginCandidate, error) {
	return m.o.Store.ListPluginCandidates(ctx)
}

func (m *Manager) invalidate(spaceID, slug string) {
	m.closeSlugProcs(spaceID, slug)
	m.mu.Lock()
	delete(m.inst[spaceID], slug)
	delete(m.mcpCs, spaceID+"/"+slug)
	m.mu.Unlock()
}

func (m *Manager) notify(ctx context.Context, spaceID string) {
	if m.o.OnChange != nil {
		m.o.OnChange(ctx, spaceID)
	}
}
