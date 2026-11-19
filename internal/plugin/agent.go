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

// ResolveAgentPlugin 读取一个智能体插件的提示词正文。
//
// 只读这一个插件，不再沿 agent.extends 拼父链。理由：散文拼接会把「可以写文件」
// 和「不要写文件」放进同一层互相抵消，结果取决于措辞与顺序；而父智能体发版会在
// 使用方不知情时改变它的行为，排查时只能猜。要复用别人的工作方式就复制正文，
// 要复用能力就写 agent.requires——两者都是显式的。
//
// 正文可以由安装级配置覆盖（插件卡片上的「提示词覆盖」）：内置件只有系统空间那
// 一份安装记录，改它等于改所有空间；自建插件只影响当前空间的这份安装。
func (m *Manager) ResolveAgentPlugin(ctx context.Context, spaceID, rawSlug string) (agentspec.Resolved, error) {
	slug := strings.TrimSpace(rawSlug)
	if slug == "" {
		return agentspec.Resolved{}, store.ErrNotFound
	}
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return agentspec.Resolved{}, err
	}
	inst, ok := m.snapshot(spaceID)[slug]
	if !ok || inst.Manifest.Kind != KindAgent {
		return agentspec.Resolved{}, store.ErrNotFound
	}

	entry := AgentEntry
	if inst.Manifest.Agent != nil && inst.Manifest.Agent.Entry != "" {
		entry = inst.Manifest.Agent.Entry
	}
	text, err := ReadFile(ctx, m.o.Storage, inst.Dir, entry)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return agentspec.Resolved{}, err
	}
	prompt := strings.TrimSpace(text)

	// 安装级覆盖：留空表示用插件自带的 AGENT.md。
	// 与 shell 的命令名单相反——那里清空等于「都不确认」，这里清空必须等于「用默认」，
	// 否则把输入框清空保存会把智能体变成空人格。
	if override := strings.TrimSpace(inst.Config[ConfigAgentPrompt]); override != "" {
		prompt = override
	}
	if len(prompt) > agentspec.MaxPromptBytes {
		return agentspec.Resolved{}, fmt.Errorf("%w: 智能体提示词超过 %d 字节", agentspec.ErrInvalid, agentspec.MaxPromptBytes)
	}

	name := inst.Manifest.Name
	if name == "" {
		name = slug
	}
	// 平台内置件（origin=system）与会话的关系是「运营对象 vs 业务配置」：
	// 前者运行时解析正文，后者创建时固化。插件实例上看不到 origin，回查一次发布记录。
	builtin := false
	if p, err := m.o.Store.GetPluginBySlug(ctx, slug); err == nil {
		builtin = p.Origin == store.PluginOriginSystem
	}
	return agentspec.Resolved{
		ID:      slug,
		Name:    name,
		Version: inst.Version,
		Prompt:  prompt,
		Builtin: builtin,
	}, nil
}
