package session

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/resource"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

const (
	// sourceReadLimit 是 resource_read 单次读取能力源文件的上限，与工具侧 24KB 上限一致
	sourceReadLimit = 24 << 10
	// maxSkillDocBytes 是读取技能正文的上限；真正的注入预算按 token 控制（见 agent.systemMessage），
	// 这里只防止把超大文件整体读进内存
	maxSkillDocBytes = 64 << 10
	// publishedPathPrefix 是已发布资源在 resource_read 路径里的前缀
	publishedPathPrefix = "published/"
	// sourcePublished 标记条目来自「资源管理」中的发布件
	sourcePublished = "published"
)

// sourceLoader 把「当前空间 + 系统空间 + 已发布资源」适配成工具层接口。
//
// 空间与用户在构造时固定，工具侧不接受 space_id 参数，模型无法指定其他空间。
type sourceLoader struct {
	spaces    *space.Service
	published *resource.Service
	spaceID   string
	userID    string
}

func (l sourceLoader) Sources(ctx context.Context) ([]tool.SourceEntry, error) {
	idx := l.spaces.MergedSources(ctx, l.spaceID)
	out := make([]tool.SourceEntry, 0, len(idx.Skills)+len(idx.Tools)+len(idx.MCPS))
	out = appendSource(out, "skills", idx.Skills)
	out = appendSource(out, "tools", idx.Tools)
	out = appendSource(out, "mcps", idx.MCPS)

	// 已发布资源单独成层：不与草稿做同名覆盖，命中多条同名时由 resource_search
	// 返回 hint，让模型先向用户确认用哪一份。
	if l.published != nil {
		for _, e := range l.published.Sources(ctx, l.userID) {
			out = append(out, tool.SourceEntry{
				Kind:        pluralKind(e.Kind),
				Name:        e.Name,
				Description: e.Description,
				Path:        e.Path,
				Source:      sourcePublished,
			})
		}
	}
	return out, nil
}

func (l sourceLoader) ReadSource(ctx context.Context, rel string) (string, error) {
	if strings.HasPrefix(rel, publishedPathPrefix) {
		if l.published == nil {
			return "", fmt.Errorf("已发布资源不可用")
		}
		return l.published.ReadPublished(ctx, l.userID, rel, sourceReadLimit)
	}
	return l.spaces.ReadSourceFile(ctx, l.spaceID, rel, sourceReadLimit)
}

func (l sourceLoader) RefreshIndex(ctx context.Context) error {
	return l.spaces.RebuildIndex(ctx, l.spaceID)
}

func appendSource(out []tool.SourceEntry, kind string, entries []space.IndexEntry) []tool.SourceEntry {
	for _, e := range entries {
		out = append(out, tool.SourceEntry{
			Kind:        kind,
			Name:        e.Name,
			Description: e.Description,
			Path:        e.Path,
			Source:      e.Source,
		})
	}
	return out
}

// pluralKind 把资源类型转换成索引里使用的复数形式。
func pluralKind(kind string) string {
	switch kind {
	case "skill":
		return "skills"
	case "tool":
		return "tools"
	case "mcp":
		return "mcps"
	case "component":
		return "components"
	case "db":
		return "dbs"
	default:
		return kind + "s"
	}
}

// skillIndex 返回注入系统提示词的技能索引（当前空间 + 系统空间，已去重）。
//
// 发布件不进常驻索引：它们是「按需检索」的资源，需要时由模型用 resource_search 找到。
func (s *Service) skillIndex(ctx context.Context, spaceID string) []tool.SourceEntry {
	idx := s.spaces.MergedSources(ctx, spaceID)
	out := make([]tool.SourceEntry, 0, len(idx.Skills))
	for _, e := range idx.Skills {
		out = append(out, tool.SourceEntry{
			Kind:        "skills",
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
