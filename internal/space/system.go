package space

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// EnsureSystemSpace 幂等地保证「系统空间」存在，并把当前管理员加入成员。
//
// 系统空间全局唯一，只有系统管理员（配置 ADMIN_JOB_NUMBERS 中的工号）可见可编辑；
// 其根下的 skills / tools / mcps 是 Agent 默认全局可见的能力搜索源，不可改名删除。
func (s *Service) EnsureSystemSpace(ctx context.Context, user store.User) error {
	if !s.IsAdmin(user) {
		return nil
	}

	sp, err := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	switch {
	case errors.Is(err, store.ErrNotFound):
		id := platform.NewUUID()
		sp, err = s.store.CreateSpace(ctx, store.SpaceInput{
			ID:               id,
			OwnerID:          user.ID,
			Name:             SystemSpaceName,
			Type:             SpaceTypeSystem,
			Path:             "/system/" + id,
			Depth:            0,
			IsSystemReserved: true,
		})
		if err != nil {
			return fmt.Errorf("创建系统空间失败: %w", err)
		}
		s.log.Info("系统空间已创建", "space_id", sp.ID, "owner", user.ID)
	case err != nil:
		return err
	}

	// 每个管理员都是 manage，但系统空间不提供共享入口
	if err := s.store.EnsureSpaceMember(ctx, sp.ID, user.ID, "manage"); err != nil {
		return err
	}
	return s.seedSystemSpace(ctx, sp.ID)
}

// BootstrapSystemSpace 在服务启动时按配置工号尝试初始化系统空间。
//
// 管理员可能还没有登录过（app_user 无记录），此时跳过，等其首次登录时再创建。
func (s *Service) BootstrapSystemSpace(ctx context.Context) {
	if len(s.admins) == 0 {
		s.log.Info("未配置系统管理员（ADMIN_JOB_NUMBERS），跳过系统空间初始化")
		return
	}

	ready := 0
	for job := range s.admins {
		user, err := s.store.FindUserByJobNumber(ctx, job)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Warn("按工号查询管理员失败", "job_number", job, "err", err)
			}
			continue
		}
		if err := s.EnsureSystemSpace(ctx, user); err != nil {
			s.log.Warn("系统空间初始化失败", "job_number", job, "err", err)
			continue
		}
		ready++
	}
	if ready == 0 {
		s.log.Warn("系统管理员尚未登录过，系统空间将在首次登录时创建", "admins", len(s.admins))
	}
}

// seedSystemSpace 幂等写入内置目录与内容。
//
// skills / mcps 的内容允许管理员维护，已存在时不覆盖；tools/ 是平台托管清单，
// 每次按运行时覆盖（见 seedTools）。
func (s *Service) seedSystemSpace(ctx context.Context, spaceID string) error {
	if err := s.storage.EnsureDir(ctx, s.filesKey(spaceID)); err != nil {
		return err
	}
	for _, dir := range systemBuiltinDirs {
		if err := s.storage.EnsureDir(ctx, path.Join(s.filesKey(spaceID), dir)); err != nil {
			return err
		}
	}

	if err := s.seedSkills(ctx, spaceID); err != nil {
		return err
	}
	if err := s.seedTools(ctx, spaceID); err != nil {
		return err
	}
	if err := s.seedMCPs(ctx, spaceID); err != nil {
		return err
	}
	// 内置内容变了，索引要跟着重建（files/.meta.json）
	s.refreshIndex(ctx, spaceID)
	return nil
}

func (s *Service) seedSkills(ctx context.Context, spaceID string) error {
	for _, skill := range builtinSkills {
		dirKey := path.Join(s.filesKey(spaceID), systemSkillsDir, skill.DirName)
		if err := s.storage.EnsureDir(ctx, dirKey); err != nil {
			return err
		}

		meta := SkillMeta{
			Name:        skill.Name,
			Description: skill.Description,
			Type:        skillType,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		body, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return err
		}
		// 内置技能是平台托管内容（与 tools/ 一致）：随代码版本覆盖，
		// 否则升级后文档与运行时工具对不上。想定制请复制成自定义技能。
		if _, err := s.storage.Put(ctx, path.Join(dirKey, skillMetaFile),
			strings.NewReader(string(body)), int64(len(body))); err != nil {
			return err
		}
		if _, err := s.storage.Put(ctx, path.Join(dirKey, "SKILL.md"),
			strings.NewReader(skill.Doc), int64(len(skill.Doc))); err != nil {
			return err
		}
	}
	return nil
}

// seedTools 覆盖写入内置工具清单。
//
// 与 skills 不同，tools/ 是平台托管内容（系统空间不可编辑），必须跟运行时一致，
// 否则新增内置工具后系统空间里查不到，全局搜索源就失真了。
func (s *Service) seedTools(ctx context.Context, spaceID string) error {
	root := path.Join(s.filesKey(spaceID), systemToolsDir)
	keep := make(map[string]bool, len(tool.BuiltinManifests()))
	for _, m := range tool.BuiltinManifests() {
		body, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		name := m.Name + ".json"
		keep[name] = true
		if _, err := s.storage.Put(ctx, path.Join(root, name), strings.NewReader(string(body)), int64(len(body))); err != nil {
			return err
		}
	}

	// 清理已下线的工具清单，否则检索会返回运行时根本不存在的工具
	entries, err := s.storage.List(ctx, root)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := path.Base(e.Key)
		if e.IsDir || keep[name] || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := s.storage.Delete(ctx, e.Key); err != nil {
			s.log.Warn("清理过期工具清单失败", "space_id", spaceID, "file", name, "err", err)
		}
	}
	return nil
}

func (s *Service) seedMCPs(ctx context.Context, spaceID string) error {
	root := path.Join(s.filesKey(spaceID), systemMCPsDir)
	if err := s.putIfAbsent(ctx, path.Join(root, "README.md"), mcpReadme); err != nil {
		return err
	}

	servers, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return err
	}
	for _, server := range servers {
		doc := map[string]any{
			"name":       server.Name,
			"transport":  server.Transport,
			"endpoint":   server.Endpoint,
			"visibility": server.Visibility,
			"health":     server.Health,
			"tool_count": countMCPTools(server.ToolsCache),
		}
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := s.putIfAbsent(ctx, path.Join(root, safeFileName(server.Name)+".json"), string(body)); err != nil {
			return err
		}
	}
	return nil
}

func countMCPTools(cache []byte) int {
	if len(cache) == 0 {
		return 0
	}
	var defs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(cache, &defs); err != nil {
		return 0
	}
	return len(defs)
}

// putIfAbsent 仅在文件不存在时写入，避免覆盖管理员编辑过的内置内容。
func (s *Service) putIfAbsent(ctx context.Context, key, content string) error {
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	_, err := s.storage.Put(ctx, key, strings.NewReader(content), int64(len(content)))
	return err
}

func safeFileName(name string) string {
	clean := strings.TrimSpace(name)
	clean = strings.NewReplacer("/", "_", "\\", "_", " ", "_", "\t", "_", "\r", "_", "\n", "_").Replace(clean)
	if clean == "" || clean == "." || clean == ".." {
		return "unnamed"
	}
	return clean
}

type builtinSkill struct {
	DirName     string
	Name        string
	Description string
	Doc         string
}

// builtinSkills 是内置到系统空间 skills/ 的全局技能说明。
//
// 内容与平台当前能力保持一致：Agent 目前只读，写操作需用户在界面上完成。
// builtinSkills 是内置到系统空间 skills/ 的全局技能。
//
// 内容按「Agent 操作手册」写：讲清能用哪些工具、参数怎么校验、遇到限制怎么办，
// 而不是界面点击路径——这些文档会被注入或按需读取，直接指导模型的下一步动作。
var builtinSkills = []builtinSkill{
	{
		DirName:     "空间管理",
		Name:        "空间管理",
		Description: "空间与成员权限的基本规则，以及 Agent 能查询哪些空间信息",
		Doc: joinDoc(
			"# 空间管理",
			"",
			"## 用途",
			"空间是权限边界，只有一级；空间内的分类用目录表达。Agent 只能看到自己是成员的空间。",
			"",
			"## 规则",
			"- 空间类型：`personal`（个人空间，登录时自动创建）、`shared`、`system`（平台内置，仅管理员可见）。",
			"- 成员权限：`view` < `read` < `write` < `manage`；目录不单独授权，随所属空间。",
			"- 只有 `write` / `manage` 才能写文件或建技能；权限不足时如实告知，不要换路径重试。",
			"- 系统空间是平台默认能力源，任何用户都可以用 `resource_search` 检索、`resource_read` 只读访问。",
			"",
			"## 工具",
			"- `resource_list`：列出当前用户可见的空间（名称、类型、权限、路径）。",
			"",
			"## 回答要点",
			"- 用户问「我有哪些空间」时调用 `resource_list`，不要依赖历史记忆。",
			"- 创建、改名、删除空间尚未开放给 Agent，需要用户在界面操作，直接说明即可。",
		),
	},
	{
		DirName:     "文件与目录操作",
		Name:        "文件与目录操作",
		Description: "在当前空间读写文件、建目录与创建技能的操作手册",
		Doc: joinDoc(
			"# 文件与目录操作",
			"",
			"## 用途",
			"空间内的文件区（`files/`）是 Agent 唯一可读写的位置，所有路径都相对它。",
			"",
			"## 规则",
			"- 目录最多 5 层，名称不能含 `/`、`\\` 与 `..`；隐藏文件（`.meta.json`）不暴露、不可读。",
			"- 可读写类型：md / json / txt / yaml；读上限 24KB，写上限 256KB。",
			"- 修改已有文件前先 `file_read`，避免覆盖用户内容。",
			"- 技能是带 `.meta.json`（`type=skill`）的目录，只写 SKILL.md 不会被识别成技能。",
			"",
			"## 工具",
			"- `file_list(path)`：列目录与文件，返回 name / path / dir / size。",
			"- `file_read(path, offset, limit)`：读正文，长文件分段读。",
			"- `file_write(path, content, mode)`：`overwrite` / `append` / `create`，父目录不存在时自动创建。",
			"- `skill_create(name, description, content)`：建技能目录、写 `.meta.json` 与 SKILL.md 骨架。",
			"",
			"## 回答要点",
			"- 写入后告诉用户具体路径与字节数。",
			"- 不要尝试访问空间之外的路径（`../`），会被直接拒绝。",
		),
	},
	{
		DirName:     "会话管理",
		Name:        "会话管理",
		Description: "会话与目录的关系、会话级技能，以及 Agent 能查询的会话信息",
		Doc: joinDoc(
			"# 会话管理",
			"",
			"## 用途",
			"每个会话是一段与 Agent 的对话，归属某个空间，可挂在空间内的某个目录下。",
			"",
			"## 规则",
			"- 会话可挂在空间根或任意目录；目录改名时会话路径同步迁移，目录删除时会话移回空间根。",
			"- 每个空间至少保留一个会话。",
			"- 会话可以绑定一个技能（会话级技能），绑定后每轮对话都会按该技能的步骤执行。",
			"",
			"## 工具",
			"- `session_list`：列出当前空间的会话，最近活跃优先。",
			"",
			"## 回答要点",
			"- 用户问「最近的会话」时调用 `session_list`。",
			"- 新建、删除会话需要用户在界面操作，直接说明即可。",
		),
	},
}

func joinDoc(lines ...string) string {
	return strings.Join(lines, "\n")
}

var mcpReadme = joinDoc(
	"# MCP 服务（mcps）",
	"",
	"本目录是平台 MCP 服务的全局搜索源，Agent 与用户默认可见（通过系统空间读取）。",
	"",
	"- 每个 `<server-name>.json` 描述一个已注册的 MCP Server（name / transport / endpoint / visibility / health / tool_count）。",
	"- MCP Server 的权威数据在数据库 `mcp_server` 表，服务启动时同步工具清单；本目录是只读快照。",
	"- 新增或修改 MCP 服务请走后端注册接口，不要直接改本目录文件。",
)
