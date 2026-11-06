//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

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
// 放开的都是「进程能跑起来」的最低条件：读全盘、写本次临时目录（+ workspace）、
// 不碰网络。mach-lookup 放开对隔离强度有损，但收紧会让大量二进制直接起不来，
// 这里先取可用性。
func seatbeltProfile(spec Spec, tmp string) (string, error) {
	writePaths := []string{tmp}
	if spec.Mode == ModeWorkspaceWrite {
		writePaths = append(writePaths, spec.Dir)
	}
	// 调用方可能绕过 Run 直接构造 profile（测试、后续的其它入口），
	// 这里再补一次符号链接解析后的路径，避免 /var → /private/var 这类不匹配。
	for _, p := range writePaths {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			writePaths = append(writePaths, real)
		}
	}

	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-fork)\n(allow process-exec*)\n(allow signal)\n")
	b.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n")
	b.WriteString("(allow file-read*)\n")
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
	return b.String(), nil
}

// sbplQuote 给 SBPL 字符串加引号。SBPL 的转义规则不保证与 Go 一致，
// 所以含引号 / 反斜杠 / 换行的路径直接拒绝，而不是冒险拼进去。
func sbplQuote(p string) (string, error) {
	if strings.ContainsAny(p, "\"\\\n\r") {
		return "", fmt.Errorf("路径含 sandbox profile 不支持的字符: %q", p)
	}
	return `"` + p + `"`, nil
}
