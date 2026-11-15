package session

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

const (
	// pluginReadLimit 是 plugin_read 单次读取插件文件的上限，与工具侧 24KB 上限一致
	pluginReadLimit = 24 << 10
	// maxSkillDocBytes 是读取技能正文的上限；真正的注入预算按 token 控制（见 agent.systemMessage），
	// 这里只防止把超大文件整体读进内存
	maxSkillDocBytes = 64 << 10
)

// pluginLoader 把「当前空间 + 系统空间 + 已安装插件」适配成工具层接口。
//
// 空间在构造时固定，工具侧不接受 space_id 参数，模型无法指定其他空间。
type pluginLoader struct {
	spaces  *space.Service
	spaceID string
}

func (l pluginLoader) Plugins(ctx context.Context) ([]tool.PluginEntry, error) {
	idx := l.spaces.MergedSources(ctx, l.spaceID)
	out := make([]tool.PluginEntry, 0, len(idx.Skills)+len(idx.Tools)+len(idx.MCPS)+len(idx.Components))
	out = appendPlugin(out, "skills", idx.Skills)
	out = appendPlugin(out, "tools", idx.Tools)
	out = appendPlugin(out, "mcps", idx.MCPS)
	out = appendPlugin(out, "components", idx.Components)

	// 发布件不默认进检索池：只有安装到本空间的插件才可用。市场浏览走插件接口；
	// 未安装的发布件不能被 plugin_search 当成可用能力。
	return out, nil
}

// ReadPlugin 读插件文件。
//
// published/<kind>/<slug>/<version>/<file> 也交给空间服务转发：路径里的 slug 与版本
// 只有插件层能映射到真实发布目录（artifact 以 plugin id 落盘），会话层不重复实现。
func (l pluginLoader) ReadPlugin(ctx context.Context, rel string) (string, error) {
	return l.spaces.ReadSourceFile(ctx, l.spaceID, rel, pluginReadLimit)
}

func (l pluginLoader) RefreshIndex(ctx context.Context) error {
	return l.spaces.RebuildIndex(ctx, l.spaceID)
}

func appendPlugin(out []tool.PluginEntry, kind string, entries []space.IndexEntry) []tool.PluginEntry {
	for _, e := range entries {
		out = append(out, tool.PluginEntry{
			Kind:        kind,
			Name:        e.Name,
			Description: e.Description,
			Path:        e.Path,
			Source:      e.Source,
		})
	}
	return out
}

// loadSkill 读取会话绑定技能的正文与展示名。
func (s *Service) loadSkill(ctx context.Context, spaceID, rel string) (*agent.ActiveSkill, error) {
	doc, err := s.spaces.SkillDoc(ctx, spaceID, rel, maxSkillDocBytes)
	if err != nil {
		return nil, err
	}
	name := path.Base(path.Dir(rel))
	for _, e := range s.spaces.MergedSources(ctx, spaceID).Skills {
		if e.Path == rel {
			name = e.Name
			break
		}
	}
	return &agent.ActiveSkill{Name: name, Path: rel, Doc: doc}, nil
}

// SetSkill 绑定或清除会话技能。path 必须是当前空间或系统空间已有的 SKILL.md 路径。
func (s *Service) SetSkill(ctx context.Context, userID, sessionID, skillPath string) (store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	target := strings.TrimSpace(skillPath)
	if target != "" {
		found := false
		for _, e := range s.spaces.MergedSources(ctx, sess.SpaceID).Skills {
			if e.Path == target {
				found = true
				break
			}
		}
		if !found {
			return store.Session{}, fmt.Errorf("技能不存在：%s", target)
		}
	}

	if err := s.store.UpdateSessionSkill(ctx, sessionID, target); err != nil {
		return store.Session{}, err
	}
	sess.SkillPath = target
	return sess, nil
}
