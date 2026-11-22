package plugin

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// spaceFilesDir 是空间工作区在空间根下的目录名，与 space 服务保持一致。
const spaceFilesDir = "files"

// sandboxMounts 把「安装位置 + plugin.json 的 fs 声明」解析成沙箱挂载表。
//
// 默认边界：**空间工作区不挂**，插件目录可读写。扩展面只能来自清单的 fs 声明，
// 且永远落在空间工作区内，越界一律拒绝——所以这个字段不需要审批，也不可能被
// 作者用来提权。
//
// 插件自己的目录总是挂：已发布但不可扩展的插件，运行目录在 published/ 下，
// 不在空间工作区里，不加进去连入口脚本都读不到。
//
// 返回值 mounted 只在「确实挂了空间路径」时为真：它决定要不要给 WA_SPACE_DIR，
// 没挂还注入的话 wa.path() 会拼出一个沙箱里不存在的路径，作者看到的只是
// ENOENT，看不出「这个插件根本没有文件直连权限」。读业务数据请用 file_read 这类工具。
func (m *Manager) sandboxMounts(spaceID, dirKey string, meta Manifest) ([]sandbox.Mount, bool, error) {
	lp, ok := m.o.Storage.(localPathStore)
	if !ok {
		return nil, false, errors.New("当前存储不提供本地路径，无法建立文件白名单")
	}
	spaceKey := m.o.Storage.SpaceKey(spaceID)
	spaceRoot, err := lp.LocalPath(spaceKey)
	if err != nil {
		return nil, false, fmt.Errorf("解析空间目录失败: %w", err)
	}
	pluginDir, err := lp.LocalPath(dirKey)
	if err != nil {
		return nil, false, fmt.Errorf("解析插件目录失败: %w", err)
	}
	return buildSandboxMounts(spaceKey, filepath.Join(spaceRoot, spaceFilesDir), dirKey, pluginDir, meta)
}

// buildSandboxMounts 是纯路径计算，便于单测：调用方负责给出存储 key 与真实目录。
//
// 默认可见面只有插件自己的目录（挂在 /plugin），**空间工作区不挂**：插件读业务
// 数据走工具调用（file_read 等经 Invoker），不靠文件系统直连。一个插件不应该
// 因为「被装进了这个空间」就顺带看见这个空间的所有文件——那是安装带来的副作用，
// 不是作者的显式要求。
//
// 清单里的 fs.read / fs.write 是显式例外：作者声明要直连某个子目录时才挂，
// 路径解析到空间工作区内的 /space/<rel>，越界一律拒绝。
func buildSandboxMounts(spaceKey, spaceRoot, pluginKey, pluginDir string, meta Manifest) ([]sandbox.Mount, bool, error) {
	// 插件自己的目录读写都要：入口脚本与随包资源在这里，运行时缓存（临时文件、
	// 下载中转）也落这里——它是这个插件唯一「属于自己」的可写位置。
	mounts := []sandbox.Mount{{
		Source: pluginDir,
		Key:    pluginKey,
		Target: sandbox.PluginDir,
		Write:  meta.FS == nil || !meta.FS.ReadOnly,
	}}
	mounted := false
	if meta.FS == nil {
		return mounts, mounted, nil
	}
	add := func(rel string, write bool) error {
		clean, err := resolveUnder(spaceRoot, rel)
		if err != nil {
			return err
		}
		mounts = append(mounts, sandbox.Mount{
			Source: filepath.Join(spaceRoot, filepath.FromSlash(clean)),
			Key:    path.Join(spaceKey, spaceFilesDir, clean),
			Target: path.Join(sandbox.SpaceDir, clean),
			Write:  write,
		})
		mounted = true
		return nil
	}
	for _, rel := range meta.FS.Read {
		if err := add(rel, false); err != nil {
			return nil, false, err
		}
	}
	if !meta.FS.ReadOnly {
		for _, rel := range meta.FS.Write {
			if err := add(rel, true); err != nil {
				return nil, false, err
			}
		}
	}
	return mounts, mounted, nil
}

// resolveUnder 校验清单里的相对前缀，返回相对空间工作区的干净路径。
//
// 两道检查：Clean 去掉 ./ 与多余分隔符，Rel 再确认结果确实落在 root 之内。
// 只做字符串前后缀判断会被 a/../../b 这类写法绕过。
func resolveUnder(root, rel string) (string, error) {
	raw := strings.TrimSpace(rel)
	if raw == "" {
		return "", errors.New("fs 路径不能为空")
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("fs 路径必须是相对空间工作区的路径: %q", rel)
	}
	clean := path.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("fs 路径不能越出空间工作区: %q", rel)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	back, err := filepath.Rel(root, target)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fs 路径不能越出空间工作区: %q", rel)
	}
	return clean, nil
}
