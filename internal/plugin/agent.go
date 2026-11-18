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
			// 头像地址带文件修改时间：草稿改完立刻换地址，列表不必等版本号变
			item.IconURL = m.agentIconURL(ctx, spaceID, inst.Slug, inst.Dir, inst.Manifest.Agent.Icon)
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AgentDisplayNames 返回空间内所有智能体插件的展示名（含已停用与草稿）。
//
// 只用于「会话列表里这条会话是谁在答」：停用的智能体不能再新建会话，但历史会话
// 还在，图标要显示名字而不是退化成英文 slug。**选择器的白名单仍是 ListAgentPlugins**
// （只含已启用），两者不能混用，否则停用就只是标签。
func (m *Manager) AgentDisplayNames(ctx context.Context, spaceID string) (map[string]string, error) {
	items, err := m.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		if it.Kind != string(KindAgent) {
			continue
		}
		name := it.Name
		if name == "" {
			name = it.Slug
		}
		out[it.Slug] = name
	}
	return out, nil
}

// AgentDependencies 返回智能体自身 + agent.requires 的递归闭包（只含 slug，来源空间已知）。
//
// 子会话跨空间继承智能体时，只把这几个插件借过来，不把发起方空间的整份目录摊给
// 被派发人：requires 之外的文件、技能、MCP 都不该出现在子会话里。
func (m *Manager) AgentDependencies(ctx context.Context, spaceID, rawSlug string) ([]string, error) {
	slug := strings.TrimSpace(rawSlug)
	if slug == "" {
		return nil, store.ErrNotFound
	}
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, err
	}
	snap := m.snapshot(spaceID)

	var out []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(cur string) {
		if cur == "" || seen[cur] {
			return
		}
		inst, ok := snap[cur]
		if !ok {
			return
		}
		seen[cur] = true
		out = append(out, cur)
		if inst.Manifest.Agent == nil {
			return
		}
		for _, dep := range inst.Manifest.Agent.Requires {
			walk(dep)
		}
	}
	walk(slug)
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
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
