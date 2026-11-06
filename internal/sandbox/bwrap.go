//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// NewBwrap 检测 bubblewrap 是否真的可用——不只看有没有装。
//
// 很多发行版（尤其加固过的内核）会禁用 user namespace，此时 bwrap 二进制在、
// 但一跑就失败。探针能提前发现，让 Detect 老老实实降级并让调用方告警。
func NewBwrap() (Runner, error) {
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: 未找到 bubblewrap", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, bin,
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "--", "/bin/true")
	if out, probeErr := probe.CombinedOutput(); probeErr != nil {
		return nil, fmt.Errorf("%w: bwrap 探针失败（常见原因：user namespace 被禁用）: %v %s",
			ErrUnavailable, probeErr, strings.TrimSpace(string(out)))
	}
	return &runner{name: "bwrap", build: bwrapBuild(bin)}, nil
}

func bwrapBuild(bin string) buildFunc {
	return func(spec Spec, tmp string, env []string) ([]string, error) {
		args := []string{
			bin,
			// 整棵树只读挂载：能读，不能改。
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			// 与宿主隔开 PID / IPC / UTS，--new-session 断开控制终端。
			"--unshare-pid", "--unshare-ipc", "--unshare-uts",
			"--new-session", "--die-with-parent",
			// 本次调用的私有目录：同时充当 TMPDIR 与 HOME。
			"--bind", tmp, tmp,
			"--clearenv",
		}
		if spec.Mode == ModeWorkspaceWrite {
			args = append(args, "--bind", spec.Dir, spec.Dir)
		}
		if !spec.Network {
			args = append(args, "--unshare-net")
		}
		// 环境变量走 --setenv：父进程环境已被 --clearenv 清掉，只剩这里列的。
		for _, kv := range env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			args = append(args, "--setenv", k, v)
		}
		args = append(args, "--chdir", spec.Dir, "--")
		return append(args, spec.Cmd...), nil
	}
}
