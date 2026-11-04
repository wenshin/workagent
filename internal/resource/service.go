// Package resource 实现资源发布与「资源管理」：把空间里的草稿按版本快照发布到
// 独立存储（published/），供其他用户按需检索使用。
//
// 设计取舍（见 docs/资源说明.md）：
//   - 发布件不可变，按版本快照存储，改名/改内容必须发新版本；
//   - 发布范围只有「用户范围」（public / private），没有「加载范围」：
//     是否使用由模型按需检索决定；
//   - 发布后不再暴露原始定义（MCP 配置、tool 文件、DB 连接串），
//     资源管理只展示名称、版本与描述。
package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

const (
	// KindSkill 是当前唯一支持发布的资源类型。
	KindSkill = "skill"
	// publishedRoot 是发布产物在存储根下的目录名，与工作空间完全分离。
	publishedRoot = "published"
	// LatestVersion 是路径里表示「最新版本」的占位符。
	LatestVersion = "latest"

	// 发布快照的体积上限，避免把空间里的超大目录复制进发布区。
	maxPublishFiles = 200
	maxPublishBytes = 5 << 20
	// skillMetaFile 是技能目录的元数据文件名，与 space 包保持一致。
	skillMetaFile = ".meta.json"
)

var (
	// ErrNoPermission 表示当前用户无权执行该操作。
	ErrNoPermission = errors.New("没有权限")
	// ErrBadVersion 表示版本号格式不合法。
	ErrBadVersion = errors.New("版本号必须是 x.y.z 形式")
	// ErrVersionExists 表示该版本已发布（版本快照不可变）。
	ErrVersionExists = errors.New("该版本已发布，版本快照不可变，请使用新的版本号")
	// ErrUnsupportedKind 表示该资源类型暂不支持发布。
	ErrUnsupportedKind = errors.New("暂不支持发布该类型资源")
	// ErrNotSkill 表示目标路径不是技能目录（缺少 .meta.json 标记）。
	ErrNotSkill = errors.New("只能发布技能目录（需要带 .meta.json 的技能）")
	// ErrNoDoc 表示技能还没有 SKILL.md 正文。
	ErrNoDoc = errors.New("该技能还没有 SKILL.md 正文，请先补齐内容再发布")
	// ErrLastVersion 表示只剩最后一个版本，应删除整个资源而不是删版本。
	ErrLastVersion = errors.New("这是唯一版本，如需下架请直接删除资源")
	// ErrTooLarge 表示发布快照超过体积上限。
	ErrTooLarge = errors.New("资源体积超过发布上限")
)

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Service 承载资源发布业务规则：权限、版本、快照复制与可见性。
type Service struct {
	store   store.Store
	storage storage.Storage
	spaces  *space.Service
	log     *slog.Logger
}

// NewService 构造资源服务。
func NewService(st store.Store, fs storage.Storage, spaces *space.Service, log *slog.Logger) *Service {
	return &Service{store: st, storage: fs, spaces: spaces, log: log}
}

// PublishInput 是发布入参。
type PublishInput struct {
	// SpaceID 是草稿所在空间。
	SpaceID string
	// Path 是草稿路径，相对空间 files/，可以是技能目录或其中的 SKILL.md。
	Path string
	// Kind 资源类型，默认 skill。
	Kind string
	// Version 版本号，x.y.z。
	Version string
	// Notes 发布说明。
	Notes string
	// Visibility 用户范围：public（全部可见）/ private（仅自己可见）。
	Visibility string
}

// Entry 是给 Agent 检索用的已发布资源条目。
type Entry struct {
	Kind        string
	Name        string
	Description string
	Path        string
	Version     string
}

// Publish 把空间草稿发布为一个不可变版本。
func (s *Service) Publish(ctx context.Context, user store.User, in PublishInput) (store.Resource, error) {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = KindSkill
	}
	if kind != KindSkill {
		return store.Resource{}, fmt.Errorf("%w：%s", ErrUnsupportedKind, kind)
	}

	spaceID := strings.TrimSpace(in.SpaceID)
	if spaceID == "" {
		return store.Resource{}, errors.New("space_id 不能为空")
	}
	perm, err := s.store.SpacePerm(ctx, spaceID, user.ID)
	if err != nil {
		return store.Resource{}, err
	}
	if perm != "write" && perm != "manage" {
		return store.Resource{}, fmt.Errorf("%w：发布需要该空间的 write 及以上权限", ErrNoPermission)
	}

	visibility := strings.TrimSpace(in.Visibility)
	if visibility == "" {
		visibility = store.ResourceVisibilityPrivate
	}
	if visibility != store.ResourceVisibilityPublic && visibility != store.ResourceVisibilityPrivate {
		return store.Resource{}, errors.New("visibility 只能是 public 或 private")
	}
	// 公开发布会进入所有人的检索范围，要求空间 manage（或系统管理员）
	if visibility == store.ResourceVisibilityPublic && perm != "manage" && !s.spaces.IsAdmin(user) {
		return store.Resource{}, fmt.Errorf("%w：公开发布需要该空间的 manage 权限", ErrNoPermission)
	}

	version := strings.TrimSpace(in.Version)
	if !versionRe.MatchString(version) {
		return store.Resource{}, ErrBadVersion
	}
	if len([]rune(in.Notes)) > 500 {
		return store.Resource{}, errors.New("发布说明过长，上限 500 字符")
	}

	dir, name, desc, err := s.loadSkillDraft(ctx, spaceID, in.Path)
	if err != nil {
		return store.Resource{}, err
	}

	// 同名资源（同一发布者）再次发布 = 追加版本
	var existing store.Resource
	isNew := false
	switch found, ferr := s.store.FindResourceByName(ctx, user.ID, kind, name); {
	case ferr == nil:
		existing = found
		if _, verr := s.store.GetResourceVersion(ctx, existing.ID, version); verr == nil {
			return store.Resource{}, fmt.Errorf("%w：%s", ErrVersionExists, version)
		} else if !errors.Is(verr, store.ErrNotFound) {
			return store.Resource{}, verr
		}
	case errors.Is(ferr, store.ErrNotFound):
		// 存储路径含资源 id，首次发布也要先固定 id
		existing = store.Resource{ID: platform.NewUUID()}
		isNew = true
	default:
		return store.Resource{}, ferr
	}

	target := publishedKey(kind, existing.ID, version)
	fileCount, totalBytes, err := s.copyDraft(ctx, spaceID, dir, target)
	if err != nil {
		return store.Resource{}, err
	}
	// 任一后续步骤失败都要回滚已复制的文件，避免留下无人引用的快照
	cleanup := func() {
		if derr := s.storage.Delete(ctx, target); derr != nil {
			s.log.Warn("清理发布快照失败", "path", target, "err", derr)
		}
	}

	if err := s.writePublishedMeta(ctx, target, publishedMeta{
		Kind:          kind,
		Name:          name,
		Description:   desc,
		Version:       version,
		Visibility:    visibility,
		ResourceID:    existing.ID,
		SourceSpaceID: spaceID,
		PublishedAt:   time.Now().Format(time.RFC3339),
		PublishedBy:   user.ID,
	}); err != nil {
		cleanup()
		return store.Resource{}, err
	}
	fileCount++

	ver := store.ResourceVersion{
		Version:     version,
		Notes:       strings.TrimSpace(in.Notes),
		StoragePath: target,
		FileCount:   fileCount,
		TotalBytes:  totalBytes,
		CreatedBy:   user.ID,
	}

	if isNew {
		res, err := s.store.CreateResource(ctx, store.Resource{
			ID:            existing.ID,
			Kind:          kind,
			Name:          name,
			Description:   desc,
			Visibility:    visibility,
			Status:        store.ResourceStatusEnabled,
			OwnerID:       user.ID,
			SourceSpaceID: spaceID,
		}, ver)
		if err != nil {
			cleanup()
			return store.Resource{}, err
		}
		return res, nil
	}

	if err := s.store.AddResourceVersion(ctx, existing.ID, ver, s.nextLatest(ctx, existing.ID, version)); err != nil {
		cleanup()
		return store.Resource{}, err
	}
	if err := s.store.UpdateResourceMeta(ctx, existing.ID, desc, visibility); err != nil {
		return store.Resource{}, err
	}
	return s.store.GetResource(ctx, existing.ID)
}

// List 返回对该用户可见的资源（全部 public + 自己的 private）。
func (s *Service) List(ctx context.Context, userID string) ([]store.Resource, error) {
	return s.store.ListResources(ctx, userID)
}

// Detail 返回资源与全部版本；private 资源仅发布者可见。
func (s *Service) Detail(ctx context.Context, userID, id string) (store.Resource, []store.ResourceVersion, error) {
	res, err := s.visible(ctx, userID, id)
	if err != nil {
		return store.Resource{}, nil, err
	}
	versions, err := s.store.ListResourceVersions(ctx, id)
	if err != nil {
		return store.Resource{}, nil, err
	}
	return res, versions, nil
}

// SetStatus 启用 / 停用资源；停用后不再进入 Agent 检索范围。
func (s *Service) SetStatus(ctx context.Context, user store.User, id, status string) error {
	res, err := s.manageable(ctx, user, id)
	if err != nil {
		return err
	}
	if status != store.ResourceStatusEnabled && status != store.ResourceStatusDisabled {
		return errors.New("status 只能是 enabled 或 disabled")
	}
	return s.store.SetResourceStatus(ctx, res.ID, status)
}

// Delete 删除资源及其全部版本文件。
func (s *Service) Delete(ctx context.Context, user store.User, id string) error {
	res, err := s.manageable(ctx, user, id)
	if err != nil {
		return err
	}
	versions, err := s.store.ListResourceVersions(ctx, res.ID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteResource(ctx, res.ID); err != nil {
		return err
	}
	// 记录先删、文件后删：即使文件清理失败也不会再被检索到
	for _, v := range versions {
		if err := s.storage.Delete(ctx, v.StoragePath); err != nil {
			s.log.Warn("清理发布文件失败", "resource_id", res.ID, "version", v.Version, "err", err)
		}
	}
	// 顺带清掉 published/{kind}/{id} 空目录，避免资源删除后留下空壳
	if len(versions) > 0 {
		if err := s.storage.Delete(ctx, path.Dir(versions[0].StoragePath)); err != nil {
			s.log.Warn("清理发布目录失败", "resource_id", res.ID, "err", err)
		}
	}
	return nil
}

// DeleteVersion 删除指定版本；version 为空表示最新版本。
func (s *Service) DeleteVersion(ctx context.Context, user store.User, id, version string) error {
	res, err := s.manageable(ctx, user, id)
	if err != nil {
		return err
	}
	versions, err := s.store.ListResourceVersions(ctx, res.ID)
	if err != nil {
		return err
	}
	if len(versions) <= 1 {
		return ErrLastVersion
	}
	// 先解析出真实版本号，再删记录、最后删文件
	target := strings.TrimSpace(version)
	if target == "" {
		target = res.LatestVersion
	}
	if err := s.store.DeleteResourceVersion(ctx, res.ID, target); err != nil {
		return err
	}
	for _, v := range versions {
		if v.Version != target {
			continue
		}
		if err := s.storage.Delete(ctx, v.StoragePath); err != nil {
			s.log.Warn("清理发布文件失败", "resource_id", res.ID, "version", v.Version, "err", err)
		}
	}
	return nil
}

// Sources 返回当前用户可用（enabled）的已发布资源条目，供 Agent 检索。
func (s *Service) Sources(ctx context.Context, userID string) []Entry {
	list, err := s.store.ListResources(ctx, userID)
	if err != nil {
		s.log.Warn("读取已发布资源失败", "err", err)
		return nil
	}
	out := make([]Entry, 0, len(list))
	for _, r := range list {
		if r.Status != store.ResourceStatusEnabled || r.Kind != KindSkill || r.LatestVersion == "" {
			continue
		}
		out = append(out, Entry{
			Kind:        r.Kind,
			Name:        r.Name,
			Description: r.Description,
			Path:        path.Join(publishedRoot, r.Kind, r.ID, r.LatestVersion, "SKILL.md"),
			Version:     r.LatestVersion,
		})
	}
	return out
}

// ReadPublished 读取已发布资源的文件正文，路径取自 Sources 返回的 Path。
func (s *Service) ReadPublished(ctx context.Context, userID, rawPath string, limit int64) (string, error) {
	parts := strings.Split(strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(rawPath)), "/"), "/")
	if len(parts) < 5 || parts[0] != publishedRoot {
		return "", fmt.Errorf("路径不是已发布资源：%s", rawPath)
	}
	kind, id, version := parts[1], parts[2], parts[3]
	rel := strings.Join(parts[4:], "/")

	res, err := s.visible(ctx, userID, id)
	if err != nil {
		return "", err
	}
	if res.Kind != kind {
		return "", fmt.Errorf("资源类型不匹配：%s", kind)
	}
	if version == LatestVersion || version == "" {
		version = res.LatestVersion
	}
	ver, err := s.store.GetResourceVersion(ctx, res.ID, version)
	if err != nil {
		return "", err
	}

	f, err := s.storage.Get(ctx, path.Join(ver.StoragePath, rel))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("发布文件不存在：%s", rel)
		}
		return "", err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// visible 校验资源对该用户可见（public 或本人发布）。
func (s *Service) visible(ctx context.Context, userID, id string) (store.Resource, error) {
	res, err := s.store.GetResource(ctx, id)
	if err != nil {
		return store.Resource{}, err
	}
	if res.Visibility != store.ResourceVisibilityPublic && res.OwnerID != userID {
		return store.Resource{}, fmt.Errorf("%w：该资源未公开", ErrNoPermission)
	}
	return res, nil
}

// manageable 校验用户可管理该资源：发布者本人或系统管理员。
func (s *Service) manageable(ctx context.Context, user store.User, id string) (store.Resource, error) {
	res, err := s.store.GetResource(ctx, id)
	if err != nil {
		return store.Resource{}, err
	}
	if res.OwnerID != user.ID && !s.spaces.IsAdmin(user) {
		return store.Resource{}, fmt.Errorf("%w：只有发布者或系统管理员可以管理该资源", ErrNoPermission)
	}
	return res, nil
}

// loadSkillDraft 解析草稿技能目录，返回目录、展示名与描述。
func (s *Service) loadSkillDraft(ctx context.Context, spaceID, rawPath string) (dir, name, desc string, err error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", "", "", errors.New("path 不能为空")
	}
	clean := path.Clean("/" + strings.TrimPrefix(trimmed, "/"))
	if strings.Contains(clean, "..") {
		return "", "", "", errors.New("path 不合法")
	}
	clean = strings.TrimPrefix(clean, "/")
	// 允许直接指向 SKILL.md
	if base := path.Base(clean); strings.EqualFold(base, "SKILL.md") {
		clean = path.Dir(clean)
	}
	if clean == "." || clean == "/" || clean == "" {
		return "", "", "", errors.New("path 需要指向技能目录")
	}

	root := s.spaceFilesKey(spaceID)
	dirKey := path.Join(root, clean)
	// 技能身份由 .meta.json 标记（与 space 包一致）
	meta, ok := s.readSkillMeta(ctx, path.Join(dirKey, skillMetaFile))
	if !ok || (meta.Type != "" && meta.Type != KindSkill) {
		return "", "", "", ErrNotSkill
	}
	// 没有正文的技能发布出去也无法被 Agent 使用
	if _, err := s.storage.Stat(ctx, path.Join(dirKey, "SKILL.md")); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", "", "", ErrNoDoc
		}
		return "", "", "", err
	}

	name = path.Base(clean)
	if strings.TrimSpace(meta.Name) != "" {
		name = strings.TrimSpace(meta.Name)
	}
	desc = strings.TrimSpace(meta.Description)
	return clean, name, desc, nil
}

type draftMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

func (s *Service) readSkillMeta(ctx context.Context, key string) (draftMeta, bool) {
	f, err := s.storage.Get(ctx, key)
	if err != nil {
		return draftMeta{}, false
	}
	defer func() { _ = f.Close() }()

	var meta draftMeta
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&meta); err != nil {
		return draftMeta{}, false
	}
	return meta, true
}

// publishedMeta 是发布快照里的 .meta.json：比草稿多出版本与来源信息。
type publishedMeta struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Type          string `json:"type"`
	Version       string `json:"version"`
	Visibility    string `json:"visibility"`
	ResourceID    string `json:"resource_id"`
	SourceSpaceID string `json:"source_space_id,omitempty"`
	PublishedAt   string `json:"published_at"`
	PublishedBy   string `json:"published_by,omitempty"`
}

func (s *Service) writePublishedMeta(ctx context.Context, target string, meta publishedMeta) error {
	meta.Type = meta.Kind
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	key := path.Join(target, skillMetaFile)
	_, err = s.storage.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)))
	return err
}

// copyDraft 递归复制草稿目录到发布目录，跳过草稿的 .meta.json（由发布元数据替代）。
func (s *Service) copyDraft(ctx context.Context, spaceID, dir, target string) (int, int64, error) {
	from := path.Join(s.spaceFilesKey(spaceID), dir)
	files, bytes, err := s.copyTree(ctx, from, target, true)
	if err != nil {
		return 0, 0, err
	}
	if files == 0 {
		return 0, 0, ErrNotSkill
	}
	return files, bytes, nil
}

func (s *Service) copyTree(ctx context.Context, from, to string, skipMeta bool) (int, int64, error) {
	entries, err := s.storage.List(ctx, from)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, 0, ErrNotSkill
		}
		return 0, 0, err
	}

	var files int
	var bytes int64
	for _, e := range entries {
		name := path.Base(e.Key)
		if e.IsDir {
			sub, n, err := s.copyTree(ctx, e.Key, path.Join(to, name), skipMeta)
			if err != nil {
				return files, bytes, err
			}
			files += sub
			bytes += n
			continue
		}
		if skipMeta && name == skillMetaFile {
			continue
		}
		if files+1 > maxPublishFiles || bytes+e.Size > maxPublishBytes {
			return files, bytes, fmt.Errorf("%w：最多 %d 个文件 / %d 字节",
				ErrTooLarge, maxPublishFiles, maxPublishBytes)
		}

		src, err := s.storage.Get(ctx, e.Key)
		if err != nil {
			return files, bytes, err
		}
		dst := path.Join(to, name)
		_, copyErr := s.storage.Put(ctx, dst, src, e.Size)
		_ = src.Close()
		if copyErr != nil {
			return files, bytes, copyErr
		}
		files++
		bytes += e.Size
	}
	return files, bytes, nil
}

// nextLatest 返回加上新版本后的最高版本号。
func (s *Service) nextLatest(ctx context.Context, resourceID, candidate string) string {
	latest := candidate
	versions, err := s.store.ListResourceVersions(ctx, resourceID)
	if err != nil {
		return latest
	}
	for _, v := range versions {
		if compareVersion(v.Version, latest) > 0 {
			latest = v.Version
		}
	}
	return latest
}

// compareVersion 按 x.y.z 数值比较，返回 -1 / 0 / 1。
func compareVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
	}
	return 0
}

func (s *Service) spaceFilesKey(spaceID string) string {
	return path.Join(s.storage.SpaceKey(spaceID), "files")
}

func publishedKey(kind, id, version string) string {
	return path.Join(publishedRoot, kind, id, version)
}
