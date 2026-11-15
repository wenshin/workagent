package space

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

var (
	ErrNameInvalid     = errors.New("名称需为 1-64 个字符，且不能包含 / 或 \\")
	ErrForbidden       = errors.New("没有该空间的权限")
	ErrReserved        = errors.New("系统预置空间不支持修改或删除")
	ErrConfirmMismatch = errors.New("输入的名称与当前对象不一致")
	ErrDirExists       = errors.New("同名目录已存在")
	ErrDirNotFound     = errors.New("目录不存在")
	ErrDirTooDeep      = errors.New("目录最多支持 5 层")
	ErrFileNotFound    = errors.New("文件不存在")
	ErrFileUnsupported = errors.New("该文件类型不支持预览")
	ErrFileTooLarge    = errors.New("文件超过单次上传大小上限")
	// ErrPluginReadOnly：已安装插件的副本整棵只读，删除要走卸载
	ErrPluginReadOnly = errors.New("已安装插件只能通过卸载删除，不能直接删目录或文件")
	// ErrPluginRoot：整个插件区不能一把删掉，否则留下悬空的安装记录
	ErrPluginRoot = errors.New("插件目录不能整体删除，请逐个卸载插件")
)

const (
	// dirsRoot 是空间内用户文件区的目录名（§4.3 存储布局）
	dirsRoot = "files"
	// maxDirDepth 目录最多 5 层：空间下所有内容都是目录，但目录不允许无限嵌套
	maxDirDepth = 5
	// skillMetaFile 是 skill 目录下的元数据文件，隐藏、不参与树展示
	skillMetaFile = ".meta.json"
	// skillType 是 .meta.json 中 type 的取值
	skillType = "skill"

	// SpaceTypeSystem 是系统空间类型：全局唯一，只有系统管理员可见可编辑。
	SpaceTypeSystem = "system"
	// SystemSpaceName 是系统空间的固定名称。
	SystemSpaceName = "系统空间"
)

// systemBuiltinDirs 是系统空间根下受保护的内置目录。
//
// `plugins/` 是**全局安装目录**（内置技能、工具、智能体都装在这里），
// 被删掉或改名会让所有空间瞬间失去基础能力，所以在系统空间里禁止改名/删除。
var systemBuiltinDirs = []string{"plugins"}

// SkillMeta 是 skill 目录下 .meta.json 的内容。
type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// Service 承载空间业务规则：命名、权限、存储目录。
type Service struct {
	store   store.Store
	storage storage.Storage
	log     *slog.Logger
	// admins 是系统管理员工号集合：系统空间只对这些用户可见。
	admins map[string]bool
	// onCreated 在空间创建成功后触发（插件默认安装集）。
	// 用回调而不是直接依赖 plugin 包，避免 space ⇄ plugin 循环引用。
	onCreated func(ctx context.Context, spaceID string)
	// installed 返回已安装插件的能力条目（未落副本的内置件也必须在检索池里）。
	installed func(ctx context.Context, spaceID string) ([]IndexEntry, error)
	// disabledPlugins 返回对该空间已停用的插件 slug。索引是「文件现状」，
	// 停用后 plugins/<slug>/ 副本还在树里，合并时要按它摘掉。
	disabledPlugins func(ctx context.Context, spaceID string) (map[string]bool, error)
	// published 读取已发布件里的文件，供 plugin_read 读 published/... 路径。
	published func(ctx context.Context, spaceID, rel string) (string, error)
	// localPlugins 返回空间内已启用的本地插件 slug；未启用的草稿不进检索池。
	localPlugins func(ctx context.Context, spaceID string) (map[string]bool, error)
	// pluginChanged 在 plugins/ 目录被直接改动（删除草稿）后触发插件层重载，
	// 否则已加载的实例会继续带着已经不存在的插件跑。
	pluginChanged func(ctx context.Context, spaceID string) error
	// uploadMaxBytes 是单文件上传大小上限（字节）。装配时由配置注入，
	// 未注入按 DefaultUploadMaxBytes 处理。
	uploadMaxBytes int64
}

// SetUploadMaxBytes 设置单文件上传大小上限；非正值表示沿用默认值。
func (s *Service) SetUploadMaxBytes(n int64) {
	if n > 0 {
		s.uploadMaxBytes = n
	}
}

// UploadMaxBytes 返回单文件上传大小上限（字节）。
func (s *Service) UploadMaxBytes() int64 {
	if s.uploadMaxBytes > 0 {
		return s.uploadMaxBytes
	}
	return DefaultUploadMaxBytes
}

// SetSpaceCreated 注册空间创建后的回调。
func (s *Service) SetSpaceCreated(fn func(ctx context.Context, spaceID string)) {
	s.onCreated = fn
}

// SetInstalledSource 注入「已安装插件」能力源（plugin.Manager 实现）。
// 与文件索引合并后返回，Agent 才能检索到未落副本的预置技能正文。
func (s *Service) SetInstalledSource(fn func(ctx context.Context, spaceID string) ([]IndexEntry, error)) {
	s.installed = fn
}

// SetDisabledPluginSource 注入「已停用插件」查询（plugin.Manager 实现）。
// 停用是运行期状态，不体现为文件变化：索引必须读它，否则停用只改了标签。
func (s *Service) SetDisabledPluginSource(fn func(ctx context.Context, spaceID string) (map[string]bool, error)) {
	s.disabledPlugins = fn
}

// SetPublishedReader 注入已发布件读取器（plugin.Manager 实现）。
func (s *Service) SetPublishedReader(fn func(ctx context.Context, spaceID, rel string) (string, error)) {
	s.published = fn
}

// SetLocalPluginSource 注入本地插件启用状态（plugin.Manager 实现）。
func (s *Service) SetLocalPluginSource(fn func(ctx context.Context, spaceID string) (map[string]bool, error)) {
	s.localPlugins = fn
}

// SetPluginChanged 注入插件层重载（plugin.Manager.Reload）。
// 插件目录是文件事实，直接删目录绕过了插件接口，必须让运行时缓存失效。
func (s *Service) SetPluginChanged(fn func(ctx context.Context, spaceID string) error) {
	s.pluginChanged = fn
}

// NewService 构造空间服务；adminJobs 来自配置 ADMIN_JOB_NUMBERS。
func NewService(st store.Store, fs storage.Storage, log *slog.Logger, adminJobs []string) *Service {
	admins := make(map[string]bool, len(adminJobs))
	for _, job := range adminJobs {
		if job = strings.TrimSpace(job); job != "" {
			admins[job] = true
		}
	}
	return &Service{store: st, storage: fs, log: log, admins: admins}
}

// IsAdmin 判断用户是否为系统管理员（按配置工号）。
func (s *Service) IsAdmin(user store.User) bool {
	job := strings.TrimSpace(user.JobNumber)
	return job != "" && s.admins[job]
}

// SpacePerm 返回用户在该空间的权限，无权限返回空字符串。
// 插件接口用它与 session 保持同一套判定，不各自查表。
func (s *Service) SpacePerm(ctx context.Context, spaceID, userID string) (string, error) {
	return s.store.SpacePerm(ctx, spaceID, userID)
}

// CreateInput 是创建空间入参。空间只有一级，不再接受父空间。
type CreateInput struct {
	Name string
}

// List 返回用户可见空间（扁平，调用方用 BuildTree 排序）。
func (s *Service) List(ctx context.Context, userID string) ([]store.Space, error) {
	return s.store.ListSpaces(ctx, userID)
}

// Create 创建一级空间：id 先分配，path 由 id 推导，随后初始化存储目录。
func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (store.Space, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return store.Space{}, err
	}

	id := platform.NewUUID()
	sp, err := s.store.CreateSpace(ctx, store.SpaceInput{
		ID:      id,
		OwnerID: userID,
		Name:    name,
		Type:    "shared",
		Path:    "/shared/" + id,
		Depth:   0,
	})
	if err != nil {
		return store.Space{}, err
	}

	// 空间物理目录用 ID；用户目录建在 files/ 下（§4.3）
	if err := s.storage.EnsureDir(ctx, s.filesKey(sp.ID)); err != nil {
		// 目录可懒创建，不阻塞建空间
		s.log.Warn("初始化空间目录失败", "space_id", sp.ID, "err", err)
	}
	if s.onCreated != nil {
		// 默认安装集失败不能阻断建空间：空间可用性优先，失败会在日志与插件列表里暴露。
		s.onCreated(ctx, sp.ID)
	}
	return sp, nil
}

// Get 查询单个空间并校验可见性。
func (s *Service) Get(ctx context.Context, userID, spaceID string) (store.Space, error) {
	sp, err := s.store.GetSpace(ctx, spaceID)
	if err != nil {
		return store.Space{}, err
	}
	perm, err := s.store.SpacePerm(ctx, spaceID, userID)
	if err != nil {
		return store.Space{}, err
	}
	if perm == "" {
		return store.Space{}, ErrForbidden
	}
	sp.Perm = perm
	return sp, nil
}

// Rename 重命名空间（仅 manage，系统预置除外）。
func (s *Service) Rename(ctx context.Context, userID, spaceID, rawName string) (store.Space, error) {
	name, err := normalizeName(rawName)
	if err != nil {
		return store.Space{}, err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Space{}, err
	}
	if isLocked(sp) {
		return store.Space{}, ErrReserved
	}
	if sp.Perm != "manage" {
		return store.Space{}, ErrForbidden
	}

	if err := s.store.RenameSpace(ctx, spaceID, name); err != nil {
		return store.Space{}, err
	}
	sp.Name = name
	return sp, nil
}

// Delete 软删空间及其内容，并把存储目录移入回收站。
func (s *Service) Delete(ctx context.Context, userID, spaceID, confirmName string) error {
	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return err
	}
	if isLocked(sp) {
		return ErrReserved
	}
	if sp.Perm != "manage" {
		return ErrForbidden
	}
	if strings.TrimSpace(confirmName) != sp.Name {
		return ErrConfirmMismatch
	}

	removed, err := s.store.DeleteSpaceTree(ctx, spaceID)
	if err != nil {
		return err
	}

	for _, r := range removed {
		if err := s.storage.Rename(ctx, s.storage.SpaceKey(r.ID), storage.TrashKey(r.ID)); err != nil &&
			!errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("空间目录移入回收站失败", "space_id", r.ID, "err", err)
		}
	}
	s.log.Info("空间已删除", "space_id", spaceID, "count", len(removed), "by", userID)
	return nil
}

// Dir 是空间内的一个真实文件目录（§4.2）：只存在于文件系统，不落 space 表，
// 没有默认会话、不能单独分享，权限完全沿用所属空间。
//
// Path 是相对 space/files/ 的多级路径（如 "设计/原型"），Name 是展示名：
// skill 目录会读取 .meta.json 里的 name，其余等于路径最后一段。
type Dir struct {
	SpaceID string
	Path    string
	Name    string
	IsSkill bool
	// PluginKind 标记该目录是插件草稿（plugins/<slug>/）以及类型：
	// skill / tool / mcp / component；空串表示普通目录。
	// 树上一个目录就是一个插件，图标按类型区分，不靠 .meta.json 猜。
	PluginKind string
	// Builtin 标记系统空间的内置目录（skills / tools / mcps）：不可重命名、不可删除。
	Builtin bool
	// Installed 表示该目录是空间安装副本：只读，只能启停/卸载。
	Installed bool
	// Local 表示空间本地开发插件；Mutable 表示内容是否可编辑。
	Local   bool
	Mutable bool
	// Enabled 表示已安装或本地插件当前是否启用。
	Enabled     bool
	Description string
}

// filesKey 返回空间内用户文件区的 key，目录都建在它下面。
func (s *Service) filesKey(spaceID string) string {
	return path.Join(s.storage.SpaceKey(spaceID), dirsRoot)
}

// ListDirs 递归返回用户可见空间下的全部目录，一次取全供左侧树使用。
func (s *Service) ListDirs(ctx context.Context, userID string) ([]Dir, error) {
	spaces, err := s.store.ListSpaces(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]Dir, 0, 8)
	for _, sp := range spaces {
		installed, enabledLocal := s.pluginStates(ctx, sp.ID)
		out = append(out, s.walkDirs(ctx, sp.ID, sp.Type, "", 1, installed, enabledLocal)...)
	}
	return out, nil
}

// pluginStates 取空间的插件安装/启用状态，一次查库供整棵目录树复用。
func (s *Service) pluginStates(ctx context.Context, spaceID string) (installed, enabledLocal map[string]bool) {
	installed = map[string]bool{}
	if installs, err := s.store.ListPluginInstalls(ctx, spaceID); err == nil {
		for _, in := range installs {
			installed[in.Slug] = in.Status == store.PluginInstallEnabled
		}
	}
	enabledLocal = map[string]bool{}
	if s.localPlugins != nil {
		if m, err := s.localPlugins(ctx, spaceID); err == nil {
			enabledLocal = m
		}
	}
	return installed, enabledLocal
}

// draftSlugOf 拆出 plugins/<slug>/... 里的 slug；不在插件区时返回 false。
func draftSlugOf(rel string) (string, bool) {
	rest, ok := strings.CutPrefix(rel, DraftDirName+"/")
	if !ok {
		return "", false
	}
	slug, _, _ := strings.Cut(rest, "/")
	if slug == "" {
		return "", false
	}
	return slug, true
}

// isInstalledSubtree 判断路径是否落在已安装插件目录里（含子目录）。
//
// 安装副本是只读的：只能启停/卸载，直接增删改里面的文件会让
// 「已安装版本」与磁盘内容脱节，且发布/升级时会被覆盖。
func (s *Service) isInstalledSubtree(ctx context.Context, spaceID, rel string) bool {
	slug, ok := draftSlugOf(rel)
	if !ok {
		return false
	}
	installs, err := s.store.ListPluginInstalls(ctx, spaceID)
	if err != nil {
		// 查不到就按可写处理会让只读保护时有时无，宁可拒绝
		return true
	}
	for _, in := range installs {
		if in.Slug == slug {
			return true
		}
	}
	return false
}

// walkDirs 深度优先列目录：父目录在前，子目录紧随其后。
func (s *Service) walkDirs(ctx context.Context, spaceID, spaceType, parent string, depth int, installed, enabledLocal map[string]bool) []Dir {
	if depth > maxDirDepth {
		return nil
	}

	key := s.filesKey(spaceID)
	if parent != "" {
		key = path.Join(key, parent)
	}
	entries, err := s.storage.List(ctx, key)
	if err != nil {
		// 空间还没建过目录时 files/ 不存在，属于正常空态
		if !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("读取空间目录失败", "space_id", spaceID, "dir", parent, "err", err)
		}
		return nil
	}

	out := make([]Dir, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		name := path.Base(e.Key)
		// 隐藏目录（含 .meta.json 所在层）不进树
		if strings.HasPrefix(name, ".") {
			continue
		}
		rel := name
		if parent != "" {
			rel = parent + "/" + name
		}

		d := Dir{SpaceID: spaceID, Path: rel, Name: name}
		// 系统空间根下的 skills / tools / mcps 是内置目录，前端据此隐藏改名/删除
		if spaceType == SpaceTypeSystem && parent == "" && isSystemBuiltinDir(name) {
			d.Builtin = true
		}
		// 插件目录的展示名与描述来自 plugin.json（单一定义点）；
		// 兼容老目录：仍带 .meta.json 的按旧技能识别。
		if slug, inDraft := draftSlugOf(rel); inDraft {
			// 已安装副本的只读状态要覆盖整棵子树：references/ 之类的子目录
			// 若只按「顶层目录」判断，前端仍会给出新建/删除入口。
			if enabled, ok := installed[slug]; ok {
				d.Installed = true
				d.Mutable = false
				d.Enabled = enabled
			} else if parent == DraftDirName {
				d.Local = true
				d.Mutable = true
				d.Enabled = enabledLocal[slug]
			}
			if parent == DraftDirName {
				if pm, ok := s.readPluginMeta(ctx, spaceID, rel); ok {
					d.PluginKind = pm.Kind
					d.IsSkill = pm.Kind == "skill"
					d.Description = pm.Description
					if pm.Name != "" {
						d.Name = pm.Name
					}
				}
			}
		} else if meta, ok := s.readSkillMeta(ctx, spaceID, rel); ok {
			d.IsSkill = true
			d.Description = meta.Description
			// skill 的展示名以 .meta.json 为准，物理目录名可能被改过
			if meta.Name != "" {
				d.Name = meta.Name
			}
		}
		out = append(out, d)
		out = append(out, s.walkDirs(ctx, spaceID, spaceType, rel, depth+1, installed, enabledLocal)...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// pluginMeta 是 plugin.json 里目录展示需要的字段（其余字段由插件层校验）。
type pluginMeta struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// readPluginMeta 读取插件目录下的 plugin.json。
func (s *Service) readPluginMeta(ctx context.Context, spaceID, rel string) (pluginMeta, bool) {
	raw, err := s.readAt(ctx, spaceID, path.Join(rel, "plugin.json"), 32<<10)
	if err != nil {
		return pluginMeta{}, false
	}
	var meta pluginMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return pluginMeta{}, false
	}
	return meta, true
}

// readSkillMeta 读取目录下的 .meta.json；不存在或 type 不是 skill 时返回 false。
func (s *Service) readSkillMeta(ctx context.Context, spaceID, rel string) (SkillMeta, bool) {
	f, err := s.storage.Get(ctx, path.Join(s.filesKey(spaceID), rel, skillMetaFile))
	if err != nil {
		return SkillMeta{}, false
	}
	defer func() { _ = f.Close() }()

	var meta SkillMeta
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&meta); err != nil {
		s.log.Warn("skill 元数据解析失败", "space_id", spaceID, "dir", rel, "err", err)
		return SkillMeta{}, false
	}
	if meta.Type != skillType {
		return SkillMeta{}, false
	}
	return meta, true
}

// writeSkillMeta 覆盖写目录下的 .meta.json。
func (s *Service) writeSkillMeta(ctx context.Context, spaceID, rel string, meta SkillMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.storage.Put(ctx, path.Join(s.filesKey(spaceID), rel, skillMetaFile),
		strings.NewReader(string(data)), int64(len(data)))
	return err
}

// CreateDir 在空间（或指定父目录）下建一个真实目录，名称即物理目录名。
func (s *Service) CreateDir(ctx context.Context, userID, spaceID, rawParent, rawName string) (Dir, error) {
	name, err := normalizeDirName(rawName)
	if err != nil {
		return Dir{}, err
	}
	parent, err := normalizeRelPath(rawParent)
	if err != nil {
		return Dir{}, err
	}
	if parent != "" {
		if _, err := s.storage.Stat(ctx, path.Join(s.filesKey(spaceID), parent)); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return Dir{}, ErrDirNotFound
			}
			return Dir{}, err
		}
		if depthOf(parent)+1 > maxDirDepth {
			return Dir{}, ErrDirTooDeep
		}
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}
	// 已安装插件是只读副本，不能在它里面建目录/文件
	if parent != "" && s.isInstalledSubtree(ctx, sp.ID, parent) {
		return Dir{}, ErrReserved
	}

	rel := name
	if parent != "" {
		rel = parent + "/" + name
	}
	key := path.Join(s.filesKey(sp.ID), rel)
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return Dir{}, ErrDirExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Dir{}, err
	}

	if err := s.storage.EnsureDir(ctx, key); err != nil {
		return Dir{}, err
	}
	return Dir{SpaceID: sp.ID, Path: rel, Name: name}, nil
}

// CreateSkill 在空间里建一个技能插件草稿（skill 类型的特例）。
func (s *Service) CreateSkill(ctx context.Context, userID, spaceID, rawName, description string) (Dir, error) {
	return s.CreateDraft(ctx, userID, spaceID, draftKindSkill, rawName, description)
}

// 可新建草稿的插件类型，与插件体系的 kind 一一对应。
const (
	draftKindSkill     = "skill"
	draftKindTool      = "tool"
	draftKindMCP       = "mcp"
	draftKindComponent = "component"
	draftKindAgent     = "agent"
	draftKindSource    = "source"
)

// DraftKinds 返回可新建的草稿类型（顺序即 UI 展示顺序）：
// 技能 / 工具 / MCP / 组件 / 智能体 / 搜索源。
var DraftKinds = []string{
	draftKindSkill, draftKindTool, draftKindMCP,
	draftKindComponent, draftKindAgent, draftKindSource,
}

// CreateDraft 在 files/plugins/<slug>/ 下按类型建一个可编辑的插件草稿。
//
// 建出来的必须是「立刻能试跑」的最小合法件：清单字段齐全、入口文件存在，
// 只有业务地址留占位（MCP 的 transport.url、搜索源的 endpoint），作者替换即可。
// 元数据只在 plugin.json：正文文件没有 frontmatter，也没有第二份 .meta.json。
func (s *Service) CreateDraft(ctx context.Context, userID, spaceID, rawKind, rawName, description string) (Dir, error) {
	kind := strings.ToLower(strings.TrimSpace(rawKind))
	if !slices.Contains(DraftKinds, kind) {
		return Dir{}, fmt.Errorf("不支持的插件类型 %q，只能是 %s", rawKind, strings.Join(DraftKinds, " / "))
	}
	name, err := normalizeName(rawName)
	if err != nil {
		return Dir{}, err
	}
	desc := strings.TrimSpace(description)
	if utf8.RuneCountInString(desc) > 500 {
		return Dir{}, errors.New("插件描述不能超过 500 个字符")
	}
	// 描述是模型判断「何时使用」的依据，空着等于不可用：给一句可替换的占位。
	if desc == "" {
		desc = "待补充：说明这个插件解决什么问题、什么时候该用"
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}

	slug := skillSlug(name)
	rel := path.Join(DraftDirName, slug)
	key := path.Join(s.filesKey(sp.ID), rel)
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return Dir{}, ErrDirExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Dir{}, err
	}

	files, err := draftScaffold(kind, slug, name, desc)
	if err != nil {
		return Dir{}, err
	}
	if err := s.storage.EnsureDir(ctx, key); err != nil {
		return Dir{}, err
	}
	for relPath, body := range files {
		if err := s.putFile(ctx, path.Join(key, relPath), body); err != nil {
			_ = s.storage.Delete(ctx, key)
			return Dir{}, err
		}
	}

	s.refreshIndex(ctx, sp.ID)
	s.log.Info("插件草稿已创建", "space_id", sp.ID, "kind", kind, "dir", rel, "by", userID)
	return Dir{
		SpaceID: sp.ID, Path: rel, Name: name, Description: desc,
		PluginKind: kind, IsSkill: kind == draftKindSkill,
	}, nil
}

// draftScaffold 生成某一类插件的最小合法草稿：plugin.json + 入口文件。
//
// 清单字段与 plugin.Validate 的硬约束对齐（tool 要 runtime.entry 与 input_schema，
// mcp 要 transport.url 与非空 tools.allow，source 要 endpoint 等），
// 否则建完立刻试跑就会失败——那比不建更糟。
func draftScaffold(kind, slug, name, desc string) (map[string]string, error) {
	manifest := map[string]any{
		"protocol":    "wa-plugin/1",
		"kind":        kind,
		"slug":        slug,
		"name":        name,
		"description": desc,
	}
	files := map[string]string{}

	switch kind {
	case draftKindSkill:
		manifest["capability"] = "skill.use"
		manifest["skill"] = map[string]string{"entry": "SKILL.md"}
		// 正文骨架先落一份：建完就有东西可编辑，不用从空文件开始。
		files["SKILL.md"] = "# " + name + "\n\n## 何时使用\n\n- 待补充：用户说什么话、处在什么场景时触发。\n\n## 步骤\n\n1. 待补充。\n"

	case draftKindTool:
		manifest["capability"] = "plugin.invoke"
		manifest["runtime"] = map[string]any{"entry": "main.ts", "timeout_ms": 8000}
		manifest["input_schema"] = map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		}
		files["main.ts"] = `/** 工具入口：input 已被 input_schema 校验，返回值就是模型拿到的结果。
 * 默认每次调用都是新进程；需要跨调用保留连接/缓存时在 plugin.json 写
 * "runtime": {"entry": "main.ts", "resident": true}，否则不要依赖模块级变量。 */
// host 与 wa 由平台挂在全局，不需要 import。
export default async function (input: Record<string, unknown>) {
  // 出网：先在 plugin.json 的 capabilities 里声明 hosts，再用 wa.fetch（走宿主代理）
  //   const res = await wa.fetch("https://api.example.com/x");
  // 读文件：沙箱里已经按 fs 段挂载，直接读
  //   const text = await Deno.readTextFile(wa.path("data/x.json"));
  return { ok: true, input };
}
`

	case draftKindMCP:
		manifest["capability"] = "http.request"
		// 地址与工具名单必须由作者替换：默认不信任远端全部工具。
		manifest["transport"] = map[string]any{"type": "http", "url": "https://mcp.example.com/mcp"}
		manifest["tools"] = map[string]any{"allow": []string{"list_tools"}}

	case draftKindComponent:
		manifest["capability"] = "ui.render"
		manifest["ui"] = map[string]string{"entry": "ui.json"}
		files["ui.json"] = `{
  "dsl": "wa-ui/1",
  "view": {
    "type": "Card",
    "title": "` + name + `",
    "children": [
      { "type": "Text", "text": "在 plugin.json 的 data_sources 里声明数据，再在这里绑定。" }
    ]
  }
}
`

	case draftKindAgent:
		manifest["capability"] = "agent.use"
		manifest["agent"] = map[string]string{"entry": "AGENT.md"}
		files["AGENT.md"] = "# " + name + "\n\n## 角色\n\n待补充：这个智能体负责什么、面向谁。\n\n## 工作方式\n\n1. 待补充：先做什么、再做什么。\n\n## 输出格式\n\n待补充。\n"

	case draftKindSource:
		manifest["capability"] = "search.query"
		// endpoint 决定 Guard 白名单，必须在发布前可见；占位地址只是为了让草稿能立刻试跑。
		manifest["source"] = map[string]any{
			"protocol":        "spec",
			"scope":           "external",
			"entry":           "search.json",
			"endpoint":        "https://search.example.com/api",
			"timeout_seconds": 8,
		}
		files["search.json"] = `{
  "request": {
    "method": "GET",
    "params": { "q": "{{query}}", "size": "{{limit}}" }
  },
  "response": {
    "items": "items",
    "title": "title",
    "url": "url",
    "snippet": "snippet"
  }
}
`

	default:
		return nil, fmt.Errorf("不支持的插件类型 %q", kind)
	}

	metaRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files["plugin.json"] = string(metaRaw)
	return files, nil
}

func (s *Service) putFile(ctx context.Context, key, body string) error {
	_, err := s.storage.Put(ctx, key, strings.NewReader(body), int64(len(body)))
	return err
}

// skillSlug 把技能名转成合法 slug：只保留 ASCII 字母数字，中文等退化为内容哈希。
// 与 plugin.slugify 保持同一规则（slug 是插件身份，不随展示名变化）。
func skillSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('_')
		}
	}
	slug := strings.Trim(b.String(), "_")
	if len(slug) < 2 {
		sum := sha1.Sum([]byte(name))
		slug = "s_" + hex.EncodeToString(sum[:])[:8]
	}
	if len(slug) > 32 {
		slug = slug[:32]
	}
	// 必须小写字母开头（与 plugin.json 校验一致）
	if slug[0] < 'a' || slug[0] > 'z' {
		sum := sha1.Sum([]byte(name))
		slug = "s_" + hex.EncodeToString(sum[:])[:8]
	}
	return slug
}

// RenameDir 重命名目录（可多级）：目录名就是真实路径，所以这里是一次 fs 的 rename。
// skill 目录改名时同步更新 .meta.json 的 name，并把目录下会话的 dir 前缀一起改掉。
func (s *Service) RenameDir(ctx context.Context, userID, spaceID, rawPath, rawName string) (Dir, error) {
	rel, err := normalizeRelPath(rawPath)
	if err != nil || rel == "" {
		return Dir{}, ErrDirNotFound
	}
	newName, err := normalizeDirName(rawName)
	if err != nil {
		return Dir{}, err
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}
	// 系统空间内置目录是 Agent 的全局能力源，不允许改名
	if s.isBuiltinDir(sp, rel) {
		return Dir{}, ErrReserved
	}
	// 插件目录名就是 slug（身份 + 发布件路径），改名等于换插件：
	// 已安装记录、derived_from 都会指向不存在的 slug，因此只允许改 plugin.json 里的展示名。
	if rel == DraftDirName || path.Dir(rel) == DraftDirName {
		return Dir{}, errors.New("插件目录名即 slug，不能重命名；要改展示名请编辑 plugin.json 的 name")
	}
	// 已安装副本整棵子树只读，子目录也不能改名
	if s.isInstalledSubtree(ctx, sp.ID, rel) {
		return Dir{}, ErrReserved
	}

	oldKey := path.Join(s.filesKey(sp.ID), rel)
	if _, err := s.storage.Stat(ctx, oldKey); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Dir{}, ErrDirNotFound
		}
		return Dir{}, err
	}

	// skill 信息必须在 rename 之前读：rename 后旧路径已经不存在
	oldMeta, wasSkill := s.readSkillMeta(ctx, sp.ID, rel)

	parent, _ := path.Split(rel)
	newRel := strings.TrimSuffix(parent, "/")
	if newRel == "" {
		newRel = newName
	} else {
		newRel = newRel + "/" + newName
	}

	if rel != newRel {
		newKey := path.Join(s.filesKey(sp.ID), newRel)
		if _, err := s.storage.Stat(ctx, newKey); err == nil {
			return Dir{}, ErrDirExists
		} else if !errors.Is(err, storage.ErrNotFound) {
			return Dir{}, err
		}
		if err := s.storage.Rename(ctx, oldKey, newKey); err != nil {
			return Dir{}, err
		}
		// 目录里的会话跟着走，否则会话会指向不存在的目录
		if err := s.store.MoveSessionsDir(ctx, sp.ID, rel, newRel); err != nil {
			return Dir{}, err
		}
	}

	dir := Dir{SpaceID: sp.ID, Path: newRel, Name: newName}
	if wasSkill {
		meta := SkillMeta{
			Name:        newName,
			Description: oldMeta.Description,
			Type:        skillType,
			CreatedAt:   oldMeta.CreatedAt,
		}
		if err := s.writeSkillMeta(ctx, sp.ID, newRel, meta); err != nil {
			return Dir{}, err
		}
		dir.IsSkill = true
		dir.Description = meta.Description
	}

	if rel != newRel {
		s.refreshIndex(ctx, sp.ID)
		s.log.Info("目录已重命名", "space_id", sp.ID, "from", rel, "to", newRel, "by", userID)
	}
	return dir, nil
}

// DeleteDir 删除目录及其内容；目录不进回收站（§4.2）。
// 目录下的会话不删除，而是移回空间根，避免误删对话记录。
func (s *Service) DeleteDir(ctx context.Context, userID, spaceID, rawPath string) error {
	rel, err := normalizeRelPath(rawPath)
	if err != nil || rel == "" {
		return ErrDirNotFound
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}
	// 系统空间内置目录不可删除
	if s.isBuiltinDir(sp, rel) {
		return ErrReserved
	}
	// 整个插件区不能一把删掉：卸载要走插件接口，否则留下悬空的安装记录。
	if rel == DraftDirName {
		return ErrPluginRoot
	}
	// 已安装副本整棵子树只读：删子目录一样会破坏安装内容
	if s.isInstalledSubtree(ctx, sp.ID, rel) {
		return ErrPluginReadOnly
	}

	key := path.Join(s.filesKey(sp.ID), rel)
	if _, err := s.storage.Stat(ctx, key); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrDirNotFound
		}
		return err
	}

	if err := s.store.ClearSessionsDir(ctx, sp.ID, rel); err != nil {
		return err
	}
	if err := s.storage.Delete(ctx, key); err != nil {
		return err
	}
	// 删的是插件草稿目录时，DB 里的候选记录与插件运行时都要跟着失效：
	// 市场「待发布」直接读候选表，不清会留下一个目录已不存在的条目；
	// 运行时缓存不清则模型还能调用已删插件的工具。
	if path.Dir(rel) == DraftDirName {
		slug := path.Base(rel)
		if err := s.store.DeletePluginCandidate(ctx, sp.ID, slug); err != nil {
			s.log.Warn("清理插件候选失败", "space_id", sp.ID, "slug", slug, "err", err)
		}
		if s.pluginChanged != nil {
			if err := s.pluginChanged(ctx, sp.ID); err != nil {
				s.log.Warn("重载插件失败", "space_id", sp.ID, "err", err)
			}
		}
	}
	s.refreshIndex(ctx, sp.ID)
	s.log.Info("目录已删除", "space_id", sp.ID, "dir", rel, "by", userID)
	return nil
}

// CheckDir 校验会话要挂载的目录存在；空路径表示空间根，直接通过。
func (s *Service) CheckDir(ctx context.Context, spaceID, rawPath string) error {
	rel, err := normalizeRelPath(rawPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil
	}
	if _, err := s.storage.Stat(ctx, path.Join(s.filesKey(spaceID), rel)); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrDirNotFound
		}
		return err
	}
	return nil
}

func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrNameInvalid
	}
	if utf8.RuneCountInString(name) > 64 {
		return "", ErrNameInvalid
	}
	if strings.HasPrefix(name, ".") {
		return "", ErrNameInvalid
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsAny(name, "\r\n\t") {
		return "", ErrNameInvalid
	}
	return name, nil
}

// normalizeDirName 校验目录名：目录名即物理路径，额外禁止 . 与 ..。
func normalizeDirName(raw string) (string, error) {
	name, err := normalizeName(raw)
	if err != nil {
		return "", err
	}
	if name == "." || name == ".." {
		return "", ErrNameInvalid
	}
	return name, nil
}

// normalizeRelPath 校验多级目录路径（相对 files/），禁止空段、. 与 ..。
func normalizeRelPath(raw string) (string, error) {
	rel := strings.Trim(strings.TrimSpace(raw), "/")
	if rel == "" {
		return "", nil
	}
	segs := strings.Split(rel, "/")
	if len(segs) > maxDirDepth {
		return "", ErrDirTooDeep
	}
	for _, seg := range segs {
		if _, err := normalizeDirName(seg); err != nil {
			return "", err
		}
	}
	return strings.Join(segs, "/"), nil
}

func depthOf(rel string) int {
	if rel == "" {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}

// isLocked 判断是否为系统预置空间（个人空间根、系统空间）。
func isLocked(sp store.Space) bool {
	return sp.IsSystemReserved || sp.Type == SpaceTypeSystem || (sp.Type == "personal" && sp.ParentID == "")
}

// isSystemBuiltinDir 判断名称是否属于系统空间的内置目录。
func isSystemBuiltinDir(name string) bool {
	for _, dir := range systemBuiltinDirs {
		if dir == name {
			return true
		}
	}
	return false
}

// isBuiltinDir 判断 rel 是否为系统空间根下的内置目录（含其自身，不含内部子目录）。
func (s *Service) isBuiltinDir(sp store.Space, rel string) bool {
	return sp.Type == SpaceTypeSystem && isSystemBuiltinDir(strings.Trim(rel, "/"))
}
