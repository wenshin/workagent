//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// seatbeltBaselineRead 是 macOS 上「进程能跑起来」的最小只读集合。
//
// 注意用的是真实路径（/private/...），Seatbelt 按解析后的路径匹配，
// /etc 这类符号链接不展开会对不上。
var seatbeltBaselineRead = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/System",
	"/Library/Apple",
	"/Library/Preferences/SystemConfiguration",
	"/private/etc/passwd",
	"/private/etc/group",
	"/private/etc/hosts",
	"/private/etc/ssl",
	"/private/etc/resolv.conf",
	"/private/var/db/timezone",
	"/private/var/select",
	"/dev",
}

// NewSeatbelt 检测 macOS 的 sandbox-exec（Seatbelt）。
//
// 它已被 Apple 标记废弃，但仍是系统自带、无需安装的唯一进程级隔离手段；
// 探针失败就返回 ErrUnavailable，由 Detect 降级到 local。
func NewSeatbelt() (Runner, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("%w: sandbox-exec 只在 macOS 上可用", ErrUnavailable)
	}
	bin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("%w: 未找到 sandbox-exec（新系统可能已移除）", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, bin, "-p", "(version 1)(allow default)", "/usr/bin/true")
	if out, probeErr := probe.CombinedOutput(); probeErr != nil {
		return nil, fmt.Errorf("%w: sandbox-exec 探针失败: %v %s",
			ErrUnavailable, probeErr, strings.TrimSpace(string(out)))
	}
	return &runner{name: NameSeatbelt, build: seatbeltBuild(bin)}, nil
}

func seatbeltBuild(bin string) buildFunc {
	return func(spec Spec, tmp string, env []string) ([]string, error) {
		profile, err := seatbeltProfile(spec, tmp)
		if err != nil {
			return nil, err
		}
		// 注意不能加 "--"：sandbox-exec 把它当成程序名。
		return append([]string{bin, "-p", profile}, spec.Cmd...), nil
	}
}

// seatbeltProfile 生成 SBPL 策略：默认全拒，再逐项放开。
//
// 文件读取也走白名单（原来是 (allow file-read*) 全盘可读）：
// 没列出的路径在沙箱里不可读，插件读不到同机其它空间、密钥文件和其它业务数据。
func seatbeltProfile(spec Spec, tmp string) (string, error) {
	read, write := spec.fsPaths(seatbeltBaselineRead)
	writePaths := append([]string{tmp}, write...)
	// 代理 socket 既不在 FS 白名单里也不是临时目录：必须单独放行读写，
	// 否则沙箱内的进程连 socket 都打不开（表现为 connect: operation not permitted）。
	if spec.Proxy != "" {
		read = append(read, spec.Proxy)
		writePaths = append(writePaths, spec.Proxy)
	}

	// 调用方可能绕过 Run 直接构造 profile（测试、后续的其它入口），
	// 这里再补一次符号链接解析后的路径，避免 /var → /private/var 这类不匹配。
	read = withRealPaths(read)
	writePaths = withRealPaths(writePaths)

	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-fork)\n(allow process-exec*)\n(allow signal)\n")
	b.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n")

	// 父目录的元数据必须可读：任何解释器（node / deno / python）启动时都会
	// realpath 自己，路径上每一级目录都要 stat。缺这条的表现是
	// `EPERM: lstat '/Users'`，或者干脆 abort 且没有任何输出。
	b.WriteString("(allow file-read-metadata\n")
	for _, p := range ancestors(read, writePaths) {
		q, err := sbplQuote(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  (literal %s)\n", q)
	}
	b.WriteString(")\n")

	b.WriteString("(allow file-read*\n")
	// 根目录本身要单独放行：subpath 不覆盖它，缺这条会让被 exec 的程序
	// 在启动阶段直接 abort（macOS 实测 exit 134，且没有任何输出）。
	b.WriteString("  (literal \"/\")\n")
	for _, p := range read {
		q, err := sbplQuote(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  (subpath %s)\n", q)
	}
	// 私有临时目录必须可读可写：HOME / TMPDIR 都指向它。
	if q, err := sbplQuote(tmp); err == nil {
		fmt.Fprintf(&b, "  (subpath %s)\n", q)
	}
	b.WriteString(")\n")

	b.WriteString("(allow file-write*\n")
	for _, p := range writePaths {
		q, err := sbplQuote(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  (subpath %s)\n", q)
	}
	b.WriteString("  (literal \"/dev/null\")\n")
	b.WriteString("  (literal \"/dev/stdout\")\n")
	b.WriteString("  (literal \"/dev/stderr\")\n")
	b.WriteString(")\n")

	if !spec.Network {
		// (deny default) 已经覆盖，显式写一条是为了让读到 profile 的人一眼看到边界。
		b.WriteString("(deny network*)\n")
	}
	// SBPL 后写的规则优先，所以代理的放行必须写在 deny 之后，
	// 否则「允许 socket」会被前面的 deny 覆盖掉。
	if spec.Proxy != "" {
		q, err := sbplQuote(spec.Proxy)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(allow network-outbound (literal %s))\n", q)
	}
	return b.String(), nil
}

// ancestors 返回这些路径的所有父目录（含 "/"，不含路径自身）。
//
// 只用于授权 file-read-metadata：能 stat 目录不等于能列出内容，
// 但它是每个解释器启动的必需品。
func ancestors(groups ...[]string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, list := range groups {
		for _, p := range list {
			for d := filepath.Dir(p); ; d = filepath.Dir(d) {
				if _, ok := seen[d]; !ok {
					seen[d] = struct{}{}
					out = append(out, d)
				}
				if d == "/" || d == "." {
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// withRealPaths 追加每个路径的 EvalSymlinks 结果（去重）。
func withRealPaths(in []string) []string {
	out := make([]string, 0, len(in)*2)
	out = append(out, in...)
	for _, p := range in {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			out = append(out, real)
		}
	}
	return sortedUnique(out)
}

// sbplQuote 给 SBPL 字符串加引号。SBPL 的转义规则不保证与 Go 一致，
// 所以含引号 / 反斜杠 / 换行的路径直接拒绝，而不是冒险拼进去。
func sbplQuote(p string) (string, error) {
	if strings.ContainsAny(p, "\"\\\n\r") {
		return "", fmt.Errorf("路径含 sandbox profile 不支持的字符: %q", p)
	}
	return `"` + p + `"`, nil
}
