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

// sandboxFS 把「安装位置 + plugin.json 的 fs 声明」解析成沙箱白名单。
//
// 默认边界：**空间工作区不挂**，插件目录可读写。扩展面只能来自清单的 fs 声明，
// 且永远落在空间工作区内，越界一律拒绝——所以这个字段不需要审批，也不可能被
// 作者用来提权。
//
// 插件自己的目录总是挂：已发布但不可扩展的插件，运行目录在 published/ 下，
// 不在空间工作区里，不加进去连入口脚本都读不到。
//
// 返回值里的 spaceRoot 只在「确实挂了空间路径」时非空：它对应 WA_SPACE_DIR，
// 没挂还注入的话 wa.path() 会拼出一个沙箱里不存在的路径，作者看到的只是
// ENOENT，看不出「这个插件根本没有文件直连权限」。没声明 fs 就拿不到这个变量，
// 读业务数据请用 file_read 这类工具。
func (m *Manager) sandboxFS(spaceID, dirKey string, meta Manifest) (sandbox.FS, string, error) {
	lp, ok := m.o.Storage.(localPathStore)
	if !ok {
		return sandbox.FS{}, "", errors.New("当前存储不提供本地路径，无法建立文件白名单")
	}
	spaceRoot, err := lp.LocalPath(m.o.Storage.SpaceKey(spaceID))
	if err != nil {
		return sandbox.FS{}, "", fmt.Errorf("解析空间目录失败: %w", err)
	}
	spaceRoot = filepath.Join(spaceRoot, spaceFilesDir)
	pluginDir, err := lp.LocalPath(dirKey)
	if err != nil {
		return sandbox.FS{}, "", fmt.Errorf("解析插件目录失败: %w", err)
	}
	fs, err := buildSandboxFS(spaceRoot, pluginDir, meta)
	if err != nil {
		return sandbox.FS{}, "", err
	}
	// 只有声明了空间路径才给 WA_SPACE_DIR：见上面的注释。
	mounted := ""
	if meta.FS != nil && (len(meta.FS.Read) > 0 || (!meta.FS.ReadOnly && len(meta.FS.Write) > 0)) {
		mounted = realPath(spaceRoot)
	}
	return fs, mounted, nil
}

// buildSandboxFS 是纯路径计算，便于单测：调用方负责给出两个真实目录。
//
// 默认可见面只有插件自己的目录，**空间工作区不挂**：插件读业务数据走工具调用
// （file_read 等经 Invoker），不靠文件系统直连。一个插件不应该因为「被装进了
// 这个空间」就顺带看见这个空间的所有文件——那是安装带来的副作用，不是作者
// 的显式要求。
//
// 清单里的 fs.read / fs.write 是显式例外：作者声明要直连某个子目录时才挂，
// 路径仍然解析到空间工作区内，越界一律拒绝。
func buildSandboxFS(spaceRoot, pluginDir string, meta Manifest) (sandbox.FS, error) {
	// 插件自己的目录读写都要：入口脚本与随包资源在这里，运行时缓存（临时文件、
	// 下载中转）也落这里——它是这个插件唯一「属于自己」的可写位置。
	// 已发布件的运行目录在 published/ 下，同样按这个规则处理。
	read := []string{realPath(pluginDir)}
	var write []string
	if meta.FS == nil || !meta.FS.ReadOnly {
		write = append(write, realPath(pluginDir))
	}
	if meta.FS != nil {
		for _, rel := range meta.FS.Read {
			p, err := resolveUnder(spaceRoot, rel)
			if err != nil {
				return sandbox.FS{}, err
			}
			read = append(read, p)
		}
		if !meta.FS.ReadOnly {
			for _, rel := range meta.FS.Write {
				p, err := resolveUnder(spaceRoot, rel)
				if err != nil {
					return sandbox.FS{}, err
				}
				write = append(write, p)
			}
		}
	}
	return sandbox.FS{Read: read, Write: write}, nil
}

// resolveUnder 把清单里的相对前缀解析成空间工作区内的绝对路径。
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
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("fs 路径不能越出空间工作区: %q", rel)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	back, err := filepath.Rel(root, target)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fs 路径不能越出空间工作区: %q", rel)
	}
	return realPath(target), nil
}

// realPath 解析符号链接；解析不了就原样返回（目录可能还没建）。
//
// macOS 上 /var → /private/var，Seatbelt 按真实路径匹配，
// 不展开会得到「策略放开了却报 Operation not permitted」。
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}
