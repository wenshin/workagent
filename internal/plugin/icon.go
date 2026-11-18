package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"path"
	"strings"

	// 头像原图允许 JPEG：前端会先裁成 PNG，这里兜底「手写清单直接放 jpg」的情况。
	_ "image/jpeg"

	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

const (
	// maxAgentIconBytes 是落盘头像的上限。256×256 的 PNG 通常 20~60KB，
	// 512KB 已经足够；再大只会拖慢每次会话列表与选择器。
	maxAgentIconBytes = 512 << 10
	// maxAgentIconSourceBytes 是上传原图的上限：前端已经裁过，这里只防「传一张 20MB 原图」。
	maxAgentIconSourceBytes = 4 << 20
	// maxAgentIconSourceSide 是原图边长上限，防止解压炸弹把内存打爆。
	maxAgentIconSourceSide = 8192
	// agentIconSide 是落盘头像的边长：列表里最大 32px，256 足够 4x 屏。
	agentIconSide = 256
	// agentIconFile 是上传后使用的文件名，写回清单的 agent.icon 与它一致。
	agentIconFile = "icon.png"
)

var (
	// ErrAgentIconUnsupported 表示上传的东西不是可识别的位图，或压缩后仍然超标。
	ErrAgentIconUnsupported = errors.New("头像必须是 PNG / JPEG / WebP 图片")
	// ErrAgentIconReadOnly 表示目标是安装副本：副本只读，头像只能在草稿上改。
	ErrAgentIconReadOnly = errors.New("已安装插件是只读副本，头像请在草稿上修改")
)

// AgentIcon 读取智能体头像的字节与内容类型。
//
// 头像和插件其余文件一样从「空间副本」读：草稿改完立刻生效，安装副本是发布快照的拷贝。
func (m *Manager) AgentIcon(ctx context.Context, spaceID, slug string) ([]byte, string, error) {
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		return nil, "", err
	}
	inst, ok := m.snapshot(spaceID)[slug]
	if !ok || inst.Manifest.Kind != KindAgent || inst.Manifest.Agent == nil || inst.Manifest.Agent.Icon == "" {
		return nil, "", store.ErrNotFound
	}
	icon := inst.Manifest.Agent.Icon
	f, err := m.o.Storage.Get(ctx, path.Join(inst.Dir, icon))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxAgentIconBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxAgentIconBytes {
		return nil, "", fmt.Errorf("%w：头像文件超过 %d 字节", ErrAgentIconUnsupported, maxAgentIconBytes)
	}
	return data, agentIconContentType(icon), nil
}

// SetAgentIcon 把上传的图片居中裁成正方形、压到 256px 后写进草稿，并记进清单。
//
// 只允许改草稿：安装副本是只读的发布快照，写进去会在重装时被覆盖，也会让
// 「装的是哪个版本」说不清。
func (m *Manager) SetAgentIcon(ctx context.Context, spaceID, slug string, raw []byte) (string, error) {
	if _, err := m.mutableAgentDraft(ctx, spaceID, slug); err != nil {
		return "", err
	}

	icon, err := normalizeAgentIcon(raw)
	if err != nil {
		return "", err
	}
	dir := DraftKey(m.o.Storage, spaceID, slug)
	if _, err := m.o.Storage.Put(ctx, path.Join(dir, agentIconFile), bytes.NewReader(icon), int64(len(icon))); err != nil {
		return "", err
	}
	if err := m.writeAgentIconField(ctx, dir, agentIconFile); err != nil {
		return "", err
	}
	if err := m.reloadSpace(ctx, spaceID); err != nil {
		// 文件与清单已经写好，缓存没刷掉不该让用户以为上传失败
		m.o.Logger.Warn("头像已更新，但插件缓存重载失败", "space", spaceID, "slug", slug, "err", err)
	}
	m.notify(ctx, spaceID)
	return m.agentIconURL(ctx, spaceID, slug, dir, agentIconFile), nil
}

// ClearAgentIcon 删除头像文件并从清单里摘掉 agent.icon。
func (m *Manager) ClearAgentIcon(ctx context.Context, spaceID, slug string) error {
	meta, err := m.mutableAgentDraft(ctx, spaceID, slug)
	if err != nil {
		return err
	}
	dir := DraftKey(m.o.Storage, spaceID, slug)
	icon := agentIconFile
	if meta.Agent != nil && meta.Agent.Icon != "" {
		icon = meta.Agent.Icon
	}
	if err := m.o.Storage.Delete(ctx, path.Join(dir, icon)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := m.writeAgentIconField(ctx, dir, ""); err != nil {
		return err
	}
	if err := m.reloadSpace(ctx, spaceID); err != nil {
		m.o.Logger.Warn("头像已清除，但插件缓存重载失败", "space", spaceID, "slug", slug, "err", err)
	}
	m.notify(ctx, spaceID)
	return nil
}

// mutableAgentDraft 取一个可写的智能体草稿：安装副本与不存在的草稿都直接拒绝。
func (m *Manager) mutableAgentDraft(ctx context.Context, spaceID, slug string) (Manifest, error) {
	if _, err := m.installOf(ctx, spaceID, slug); err == nil {
		return Manifest{}, fmt.Errorf("%w：%s", ErrAgentIconReadOnly, slug)
	}
	meta, err := m.readDraft(ctx, spaceID, slug)
	if err != nil {
		return Manifest{}, err
	}
	if meta.Kind != KindAgent {
		return Manifest{}, fmt.Errorf("只有智能体插件支持头像：%s 是 %s", slug, meta.Kind)
	}
	return meta, nil
}

// agentIconURL 返回头像地址；清单没声明头像或文件不存在时返回空串。
//
// v 取文件修改时间：草稿改一次头像版本号不变，必须靠它让浏览器重新取图。
func (m *Manager) agentIconURL(ctx context.Context, spaceID, slug, dir, icon string) string {
	if icon == "" {
		return ""
	}
	info, err := m.o.Storage.Stat(ctx, path.Join(dir, icon))
	if err != nil || info.IsDir {
		return ""
	}
	return fmt.Sprintf("/spaces/%s/plugins/%s/icon?v=%d", spaceID, slug, info.ModTime.Unix())
}

// writeAgentIconField 只改清单里的 agent.icon，其余字段按原始 JSON 原样保留。
//
// 用 map[string]json.RawMessage 而不是反序列化成 Manifest 再写回：后者会把
// 作者手写的可选字段顺序与写法全部重排，diff 里看不出真正改了什么。
func (m *Manager) writeAgentIconField(ctx context.Context, dir, icon string) error {
	key := path.Join(dir, ManifestFile)
	f, err := m.o.Storage.Get(ctx, key)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleBytes+1))
	_ = f.Close()
	if err != nil {
		return err
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("清单解析失败：%w", err)
	}
	agent := map[string]json.RawMessage{}
	if v, ok := top["agent"]; ok {
		if err := json.Unmarshal(v, &agent); err != nil {
			return fmt.Errorf("agent 段解析失败：%w", err)
		}
	}
	if icon == "" {
		delete(agent, "icon")
	} else {
		v, err := json.Marshal(icon)
		if err != nil {
			return err
		}
		agent["icon"] = v
	}
	av, err := json.Marshal(agent)
	if err != nil {
		return err
	}
	top["agent"] = av

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	// 写回前按严格模式解析一遍：清单是唯一事实来源，坏一次整个插件就没了。
	if _, err := Parse(out, false); err != nil {
		return err
	}
	_, err = m.o.Storage.Put(ctx, key, bytes.NewReader(out), int64(len(out)))
	return err
}

func agentIconContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// normalizeAgentIcon 把任意尺寸的位图居中裁成正方形，再缩到 agentIconSide 后编码为 PNG。
//
// 不引入 golang.org/x/image：只为一处头像加依赖不划算，盒式采样在「缩小」场景
// 已经足够，而这里永远只会缩小。
func normalizeAgentIcon(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w：文件为空", ErrAgentIconUnsupported)
	}
	if len(raw) > maxAgentIconSourceBytes {
		return nil, fmt.Errorf("%w：原图超过 %d 字节", ErrAgentIconUnsupported, maxAgentIconSourceBytes)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrAgentIconUnsupported, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("%w：图片尺寸不合法", ErrAgentIconUnsupported)
	}
	if cfg.Width > maxAgentIconSourceSide || cfg.Height > maxAgentIconSourceSide {
		return nil, fmt.Errorf("%w：原图不能超过 %d×%d", ErrAgentIconUnsupported, maxAgentIconSourceSide, maxAgentIconSourceSide)
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w：%v", ErrAgentIconUnsupported, err)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, agentIconSide, agentIconSide))
	scaleSquare(dst, src)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	if buf.Len() > maxAgentIconBytes {
		return nil, fmt.Errorf("%w：压缩后仍超过 %d 字节", ErrAgentIconUnsupported, maxAgentIconBytes)
	}
	return buf.Bytes(), nil
}

// scaleSquare 居中裁剪 + 4×4 盒式采样缩放。
func scaleSquare(dst *image.NRGBA, src image.Image) {
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	if side <= 0 {
		return
	}
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2

	const sample = 4
	total := uint64(sample * sample)
	for dy := 0; dy < agentIconSide; dy++ {
		for dx := 0; dx < agentIconSide; dx++ {
			var r, g, bl, a uint64
			for sy := 0; sy < sample; sy++ {
				for sx := 0; sx < sample; sx++ {
					px := x0 + (dx*sample+sx)*side/(agentIconSide*sample)
					py := y0 + (dy*sample+sy)*side/(agentIconSide*sample)
					c := color.NRGBAModel.Convert(src.At(px, py)).(color.NRGBA)
					r += uint64(c.R)
					g += uint64(c.G)
					bl += uint64(c.B)
					a += uint64(c.A)
				}
			}
			dst.SetNRGBA(dx, dy, color.NRGBA{
				R: uint8(r / total), G: uint8(g / total), B: uint8(bl / total), A: uint8(a / total),
			})
		}
	}
}
