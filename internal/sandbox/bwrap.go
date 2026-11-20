//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// bwrapBaselineRead 是「进程能跑起来」所需的最小只读系统路径。
//
// 这张表是白名单而不是黑名单：不在表里、也没被 Spec.FS.Read 列出的路径，
// 在沙箱里是空目录且不可写（根最后会 --remount-ro）。所以漏项的表现是
// 「空目录」或 ENOENT（跑不起来，立刻暴露），而不是「悄悄读到了不该读的东西」。
var bwrapBaselineRead = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/lib32",
	// 动态链接器与用户名解析：缺了会得到与权限无关、极难排查的报错
	"/etc/ld.so.cache",
	"/etc/ld.so.conf",
	"/etc/ld.so.conf.d",
	"/etc/passwd",
	"/etc/group",
	"/etc/nsswitch.conf",
	"/etc/hosts",
	// DNS 配置：走代理时不需要（域名由宿主侧解析），直连时必须，
	// 缺了的表现是 curl exit 6、所有域名解析失败。
	"/etc/resolv.conf",
	"/etc/localtime",
	"/etc/timezone",
	// 证书：沙箱内自己做 TLS 时要用（走 host.call 时不需要）
	"/etc/ssl",
	"/etc/pki",
	"/etc/ca-certificates",
	"/etc/ca-certificates.conf",
	"/etc/alternatives",
}

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
	// 探针必须带上 --unshare-net：容器化部署里「有 SYS_ADMIN 但没有 NET_ADMIN」
	// 时，普通命令能跑、一旦断网就报 loopback 失败。探针不覆盖真实用法，
	// 就等于把第一次失败推迟到线上第一条命令。
	probe := exec.CommandContext(ctx, bin,
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "--unshare-net", "--", "/bin/true")
	if out, probeErr := probe.CombinedOutput(); probeErr != nil {
		return nil, fmt.Errorf("%w: bwrap 探针失败（常见原因：user namespace 被禁用）: %v %s",
			ErrUnavailable, probeErr, strings.TrimSpace(string(out)))
	}
	return &runner{name: NameBwrap, build: bwrapBuild(bin)}, nil
}

func bwrapBuild(bin string) buildFunc {
	return func(spec Spec, tmp string, env []string) ([]string, error) {
		read, write := spec.fsPaths(bwrapBaselineRead)

		args := []string{bin}
		// 白名单挂载：只挂运行所需，不再 --ro-bind / /。
		// -try 用来兼容不同发行版的路径差异（缺一个不能让沙箱整个起不来）。
		for _, p := range read {
			args = append(args, "--ro-bind-try", p, p)
		}
		args = append(args,
			"--dev", "/dev",
			// 某些运行时（worker、某些 GC 配置）需要 /dev/shm，给一个私有 tmpfs
			"--tmpfs", "/dev/shm",
			// /tmp 给一个私有 tmpfs：TMPDIR 已经指向本次调用的私有目录，但
			// fnm / cargo 这类工具会硬编码 /tmp 做下载中转，只读档位下的表现是
			// "Read-only file system (os error 30)"，和网络失败长得完全不像。
			"--tmpfs", "/tmp",
			"--proc", "/proc",
			// 与宿主隔开 PID / IPC / UTS，--new-session 断开控制终端
			"--unshare-pid", "--unshare-ipc", "--unshare-uts",
			"--new-session", "--die-with-parent",
			"--clearenv",
		)
		// 出网档位：整个工作目录先变成空 tmpfs，可写表里只剩下 .deps，
		// 随后再被 --bind 接回来。不加这条的话，父目录虽然也会被 bwrap
		// 自动建成空目录，但读挂载表将来万一又列进 Dir，就会静默退化成全可见。
		if spec.Network && spec.Dir != "" {
			args = append(args, "--tmpfs", spec.Dir)
		}
		// 可写路径后挂，覆盖前面同路径的只读挂载。
		for _, p := range write {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return nil, fmt.Errorf("准备可写目录 %s 失败: %w", p, err)
			}
			args = append(args, "--bind", p, p)
		}
		// 本次调用的私有目录：同时充当 TMPDIR 与 HOME。
		args = append(args, "--bind", tmp, tmp)

		// 出网必须 share-net：bwrap 给不了 veth，--unshare-net 之后是一个空 netns，
		// 连 loopback 都不通。所以 Network 一旦为 true，网络边界就只能靠
		// NetworkPolicy 在网络层封禁私网段，而不是靠这里的命名空间。
		if !spec.Network {
			args = append(args, "--unshare-net")
		}
		// 代理 socket 是断网沙箱唯一的对外通道：单文件 bind 挂进来，
		// 只暴露这一条路径，父目录里的其它东西照旧不可见。
		// 必须可写（rw）：连 unix socket 需要 socket 文件的写权限。
		if spec.Proxy != "" {
			args = append(args, "--bind", spec.Proxy, spec.Proxy)
		}
		// 所有挂载做完之后再把根重新挂成只读。
		//
		// 不加这一条的话，白名单没列出的路径并不是「不存在」，而是会被 bwrap
		// 作为挂载点的父目录创建出来——一个空的、可写的 tmpfs 目录。沙箱里是
		// root，于是 `echo x > /etc/passwd.bak` 会成功，白名单形同虚设。
		// 已 bind 的目录是独立挂载，不受父级 remount 影响，可写区仍然可写。
		args = append(args, "--remount-ro", "/")
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
