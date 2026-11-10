package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// maxAgentDepth 限制智能体继承层数：父链太长会让行为不可预测。
const maxAgentDepth = 4

// ListAgentPlugins 返回当前空间可选的智能体插件。
//
// 系统空间的安装是全局安装，因此「系统空间装了一个智能体」= 所有空间的会话都能选到它。
func (m *Manager) ListAgentPlugins(ctx context.Context, spaceID string) ([]agentspec.Summary, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, err
	}
	out := make([]agentspec.Summary, 0, 4)
	for _, inst := range m.snapshot(spaceID) {
		if inst.Manifest.Kind != KindAgent {
			continue
		}
		item := agentspec.Summary{
			ID:          inst.Slug,
			Name:        inst.Manifest.Name,
			Version:     inst.Version,
			Description: inst.Manifest.Description,
		}
		if item.Name == "" {
			item.Name = inst.Slug
		}
		if inst.Manifest.Agent != nil {
			item.Extends = inst.Manifest.Agent.Extends
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ResolveAgentPlugin 把智能体插件的父链拼成一段追加提示词。
//
// 顺序是「祖先在前、自己在后」：父智能体定风格，子智能体补细节。
// base 也是插件：`extends: "base"` 就是继承平台默认智能体，不再是特殊分支。
func (m *Manager) ResolveAgentPlugin(ctx context.Context, spaceID, rawSlug string) (agentspec.Resolved, error) {
	slug := strings.TrimSpace(rawSlug)
	if slug == "" {
		return agentspec.Resolved{}, store.ErrNotFound
	}
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return agentspec.Resolved{}, err
	}
	snap := m.snapshot(spaceID)

	var chain []*instance
	visited := map[string]bool{}
	cur := slug
	for {
		if visited[cur] {
			return agentspec.Resolved{}, fmt.Errorf("%w: 智能体 %s 的继承关系成环", agentspec.ErrInvalid, slug)
		}
		visited[cur] = true

		inst, ok := snap[cur]
		if !ok {
			if len(chain) == 0 {
				return agentspec.Resolved{}, store.ErrNotFound
			}
			// 父链落在 base 而 base 插件尚未就绪（系统空间还没初始化）：
			// 当作链尾处理，只注入已有部分，不让会话创建失败。
			if cur == agentspec.BaseID {
				break
			}
			return agentspec.Resolved{}, fmt.Errorf("%w: 父智能体 %s 未安装", agentspec.ErrInvalid, cur)
		}
		if inst.Manifest.Kind != KindAgent {
			if len(chain) == 0 {
				return agentspec.Resolved{}, store.ErrNotFound
			}
			return agentspec.Resolved{}, fmt.Errorf("%w: %s 不是智能体插件", agentspec.ErrInvalid, cur)
		}
		chain = append(chain, inst)
		if len(chain) > maxAgentDepth {
			return agentspec.Resolved{}, fmt.Errorf("%w: 智能体继承不能超过 %d 层", agentspec.ErrInvalid, maxAgentDepth)
		}

		parent := ""
		if inst.Manifest.Agent != nil {
			parent = strings.TrimSpace(inst.Manifest.Agent.Extends)
		}
		if parent == "" {
			break
		}
		cur = parent
	}

	var b strings.Builder
	for i := len(chain) - 1; i >= 0; i-- {
		entry := AgentEntry
		if chain[i].Manifest.Agent != nil && chain[i].Manifest.Agent.Entry != "" {
			entry = chain[i].Manifest.Agent.Entry
		}
		text, err := ReadFile(ctx, m.o.Storage, chain[i].Dir, entry)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return agentspec.Resolved{}, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	prompt := strings.TrimSpace(b.String())
	if len(prompt) > agentspec.MaxPromptBytes {
		return agentspec.Resolved{}, fmt.Errorf("%w: 智能体提示词超过 %d 字节", agentspec.ErrInvalid, agentspec.MaxPromptBytes)
	}

	head := chain[0].Manifest
	name := head.Name
	if name == "" {
		name = slug
	}
	return agentspec.Resolved{
		ID:      slug,
		Name:    name,
		Version: chain[0].Version,
		Prompt:  prompt,
	}, nil
}
