package agentspec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
)

// readLimit 是单个配置文件的大小上限，比 MaxPromptBytes 留出 JSON 结构余量。
const readLimit = MaxPromptBytes + 8<<10

// Loader 从系统空间的 agents/ 目录读取配置。
//
// 只读系统空间：调用方所在的普通空间无法用同名文件覆盖平台配置
// （ReadSourceFile 会回落到系统空间，所以必须显式传系统空间 ID）。
type Loader struct {
	spaces *space.Service
}

// NewLoader 构造加载器。
func NewLoader(spaces *space.Service) *Loader {
	return &Loader{spaces: spaces}
}

// List 返回可选的智能体清单（已过滤停用项），按 id 排序。
//
// index.json 缺失时返回空列表而不是错误：系统尚未种子化时不应阻塞会话功能。
func (l *Loader) List(ctx context.Context) ([]Summary, error) {
	raw, err := l.read(ctx, path.Join(Dir, IndexFile))
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var idx Index
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		return nil, fmt.Errorf("%w: %s 不是合法 JSON: %v", ErrInvalid, IndexFile, err)
	}

	out := make([]Summary, 0, len(idx.Agents))
	seen := make(map[string]bool, len(idx.Agents))
	for _, s := range idx.Agents {
		if err := validateID(s.ID); err != nil {
			continue
		}
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if s.Name == "" {
			s.Name = s.ID
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get 加载并合并指定智能体。
func (l *Loader) Get(ctx context.Context, id string) (Resolved, error) {
	if err := validateID(id); err != nil {
		return Resolved{}, err
	}

	spec, err := l.loadSpec(ctx, id)
	if err != nil {
		return Resolved{}, err
	}

	ownName := spec.Prompt
	if strings.TrimSpace(ownName) == "" {
		ownName = PromptFile
	}
	ownPrompt, err := l.read(ctx, path.Join(Dir, id, ownName))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Resolved{}, err
	}

	var basePrompt string
	if spec.Extends == BaseID {
		base, err := l.loadSpec(ctx, BaseID)
		if err != nil {
			return Resolved{}, fmt.Errorf("智能体 %s 继承 %s 失败: %w", id, BaseID, err)
		}
		baseName := base.Prompt
		if strings.TrimSpace(baseName) == "" {
			baseName = PromptFile
		}
		basePrompt, err = l.read(ctx, path.Join(Dir, BaseID, baseName))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Resolved{}, err
		}
	}

	return resolve(spec, basePrompt, ownPrompt)
}

func (l *Loader) loadSpec(ctx context.Context, id string) (Spec, error) {
	raw, err := l.read(ctx, path.Join(Dir, id, SpecFile))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Spec{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Spec{}, err
	}

	var spec Spec
	dec := json.NewDecoder(strings.NewReader(raw))
	// 拒绝未知字段：配置文件里写错字段名必须显式失败，而不是被静默忽略
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("%w: %s/%s/%s: %v", ErrInvalid, Dir, id, SpecFile, err)
	}
	if spec.ID == "" {
		spec.ID = id
	}
	if spec.ID != id {
		return Spec{}, fmt.Errorf("%w: 目录名 %q 与配置里的 id %q 不一致", ErrInvalid, id, spec.ID)
	}
	return spec, nil
}

// read 读系统空间里的相对路径，统一把「文件不存在」映射成 ErrNotFound。
func (l *Loader) read(ctx context.Context, rel string) (string, error) {
	if l == nil || l.spaces == nil {
		return "", fmt.Errorf("%w: 系统空间未初始化", ErrNotFound)
	}
	sysID, err := l.spaces.SystemSpaceID(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	text, err := l.spaces.ReadSourceFile(ctx, sysID, rel, readLimit)
	if err != nil {
		if errors.Is(err, space.ErrFileNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return text, nil
}
