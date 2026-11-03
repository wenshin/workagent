package space

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
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
	// 系统空间下的三个内置目录：Agent 默认全局可见的 skills / tools / mcps 搜索源。
	// 这三个目录不可重命名、不可删除。
	systemSkillsDir = "skills"
	systemToolsDir  = "tools"
	systemMCPsDir   = "mcps"
)

// systemBuiltinDirs 是系统空间根下受保护的内置目录。
var systemBuiltinDirs = []string{systemSkillsDir, systemToolsDir, systemMCPsDir}

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
	// Builtin 标记系统空间的内置目录（skills / tools / mcps）：不可重命名、不可删除。
	Builtin     bool
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
		out = append(out, s.walkDirs(ctx, sp.ID, sp.Type, "", 1)...)
	}
	return out, nil
}

// walkDirs 深度优先列目录：父目录在前，子目录紧随其后。
func (s *Service) walkDirs(ctx context.Context, spaceID, spaceType, parent string, depth int) []Dir {
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
		if meta, ok := s.readSkillMeta(ctx, spaceID, rel); ok {
			d.IsSkill = true
			d.Description = meta.Description
			// skill 的展示名以 .meta.json 为准，物理目录名可能被改过
			if meta.Name != "" {
				d.Name = meta.Name
			}
		}
		out = append(out, d)
		out = append(out, s.walkDirs(ctx, spaceID, spaceType, rel, depth+1)...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
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

// CreateSkill 在空间根下建一个 skill：本质是特殊目录 + 隐藏的 .meta.json。
func (s *Service) CreateSkill(ctx context.Context, userID, spaceID, rawName, description string) (Dir, error) {
	name, err := normalizeName(rawName)
	if err != nil {
		return Dir{}, err
	}
	desc := strings.TrimSpace(description)
	if utf8.RuneCountInString(desc) > 500 {
		return Dir{}, errors.New("技能描述不能超过 500 个字符")
	}

	sp, err := s.Get(ctx, userID, spaceID)
	if err != nil {
		return Dir{}, err
	}
	if !canWrite(sp.Perm) {
		return Dir{}, ErrForbidden
	}

	key := path.Join(s.filesKey(sp.ID), name)
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return Dir{}, ErrDirExists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Dir{}, err
	}

	if err := s.storage.EnsureDir(ctx, key); err != nil {
		return Dir{}, err
	}

	meta := SkillMeta{
		Name:        name,
		Description: desc,
		Type:        skillType,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.writeSkillMeta(ctx, sp.ID, name, meta); err != nil {
		// 元数据写失败就把目录删掉，避免留下没有身份的普通目录
		if delErr := s.storage.Delete(ctx, key); delErr != nil {
			s.log.Warn("创建 skill 失败后清理目录失败", "space_id", sp.ID, "dir", name, "err", delErr)
		}
		return Dir{}, err
	}

	s.refreshIndex(ctx, sp.ID)
	s.log.Info("skill 已创建", "space_id", sp.ID, "dir", name, "by", userID)
	return Dir{SpaceID: sp.ID, Path: name, Name: name, IsSkill: true, Description: desc}, nil
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
