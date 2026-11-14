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
// 默认边界：整个空间工作区**只读**，空间之外一律不可见；写权限必须显式声明。
// 清单里的 fs.read / fs.write 只在这个边界内收紧，永远不可能扩出去——
// 所以这个字段不需要审批，也不可能被作者用来提权。
//
// 插件自己的目录总是可读：已发布但不可扩展的插件，运行目录在 published/ 下，
// 不在空间工作区里，不加进去连入口脚本都读不到。
// 返回值里的 spaceRoot 是挂载后的空间工作区绝对路径，会以 WA_SPACE_DIR
// 注入沙箱——脚本要直接读文件，就得知道自己的相对路径挂在哪个前缀下。
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
	return fs, realPath(spaceRoot), nil
}

// buildSandboxFS 是纯路径计算，便于单测：调用方负责给出两个真实目录。
func buildSandboxFS(spaceRoot, pluginDir string, meta Manifest) (sandbox.FS, error) {
	// 插件自己的目录总是可读：入口脚本与随包资源都在这里，
	// 已发布件的运行目录还在 published/ 下，不在空间工作区里。
	read := []string{realPath(pluginDir)}
	if meta.FS == nil || len(meta.FS.Read) == 0 {
		read = append(read, realPath(spaceRoot))
	} else {
		for _, rel := range meta.FS.Read {
			p, err := resolveUnder(spaceRoot, rel)
			if err != nil {
				return sandbox.FS{}, err
			}
			read = append(read, p)
		}
	}

	// 写权限只在声明时给：没写 fs.write 就是完全只读（含自己的工作区）。
	var write []string
	if meta.FS != nil && !meta.FS.ReadOnly {
		for _, rel := range meta.FS.Write {
			p, err := resolveUnder(spaceRoot, rel)
			if err != nil {
				return sandbox.FS{}, err
			}
			write = append(write, p)
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
