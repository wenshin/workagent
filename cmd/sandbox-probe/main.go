// Command sandbox-probe 对执行体跑一组固定的隔离检查。
//
// 它不是单元测试，而是「部署后验」：检查项全部来自线上真实要求
// （能跑、能写自己的空间、写不了系统目录、断网、看不到别的空间）。
// 任何一项失败都说明这套执行面不能上线，而不是「环境有点问题」。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/sandbox/remote"
)

type check struct {
	name   string
	ok     bool
	detail string
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8323", "执行体地址（与执行体同 Pod 时用默认值）")
	token := flag.String("token", os.Getenv("SANDBOX_RUNNER_TOKEN"), "执行体令牌")
	space := flag.String("space", "spaces/probe/files", "探针工作区（相对执行体存储根的 key）")
	flag.Parse()

	client, err := remote.New(*addr, *token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接执行体失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("执行体: %s  backend=%s  isolated=%v\n\n", *addr, client.Name(), client.Isolated())

	// 工作区用过线的 key 表达，宿主路径由执行体自己解析并建目录：
	// 探针不需要、也不应该知道执行体把存储挂在哪里。
	mounts := []sandbox.Mount{{
		Key:    strings.Trim(filepath.ToSlash(*space), "/"),
		Target: sandbox.WorkspaceDir,
		Write:  true,
	}}

	var results []check
	results = append(results, check{name: "不是 local 后端", ok: client.Name() != sandbox.NameLocal, detail: client.Name()})

	// 1. 能跑
	res, err := run(client, "echo hello", sandbox.ModeReadOnly, mounts, false)
	results = append(results, verdict("能执行命令", err == nil && strings.Contains(res.Stdout, "hello"), detail(res, err)))

	// 2. 能写自己的工作区
	res, err = run(client, "echo data > .probe && cat .probe", sandbox.ModeWorkspaceWrite, mounts, false)
	results = append(results, verdict("可写空间工作区", err == nil && strings.TrimSpace(res.Stdout) == "data", detail(res, err)))

	// 3. 写不了系统目录（基线只读）
	res, err = run(client, "echo x > /etc/wa-probe 2>/dev/null; echo $?", sandbox.ModeWorkspaceWrite, mounts, false)
	results = append(results, verdict("系统目录只读", err == nil && strings.TrimSpace(res.Stdout) != "0", detail(res, err)))

	// 4. 不给网就真的没网：Network=false 时连不出去。
	//    默认档位由 shell_exec 的策略决定（其他命令有网），这一项只验证执行体听从 Spec。
	res, err = run(client, "curl -sS -m 3 -o /dev/null https://example.com; echo $?", sandbox.ModeWorkspaceWrite, mounts, false)
	results = append(results, verdict("无网档位不可达", err == nil && strings.TrimSpace(res.Stdout) != "0", detail(res, err)))

	// 5. 看不到宿主目录结构：/workspace 的父目录（沙箱根）里不该出现 spaces
	res, err = run(client, "test -e /workspace/../spaces; echo $?", sandbox.ModeReadOnly, mounts, false)
	results = append(results, verdict("宿主路径不可见", err == nil && strings.TrimSpace(res.Stdout) == "1", detail(res, err)))

	// 6. 超时会被整组回收
	start := time.Now()
	_, err = run(client, "sleep 30", sandbox.ModeWorkspaceWrite, mounts, false)
	results = append(results, verdict("超时生效", err != nil && time.Since(start) < 20*time.Second, fmt.Sprintf("cost=%s err=%v", time.Since(start).Round(time.Millisecond), err)))

	// 7. 出网可达：出网档位下能连公网。这里只验证「通」，
	//    私网封禁由 NetworkPolicy 负责，沙箱这一层看不到那条规则。
	res, err = run(client, "curl -s -m 8 -o /dev/null -w '%{http_code}' https://cdn.npmmirror.com/binaries/deno/", sandbox.ModeWorkspaceWrite, mounts, true)
	code := strings.TrimSpace(res.Stdout)
	results = append(results, verdict("出网可达", err == nil && (code == "200" || code == "404"), detail(res, err)))

	// 8. 出网不改变文件可见面：第 2 项写的 .probe 依然看得见。
	//    网络工具与普通命令跑在同一个工作区里，下载完才能就地处理；
	//    外发风险由审批与 NetworkPolicy 拦，不靠遮蔽。
	res, err = run(client, "ls -a /workspace | tr '\n' ' '", sandbox.ModeWorkspaceWrite, mounts, true)
	out := strings.TrimSpace(res.Stdout)
	results = append(results, verdict("出网仍可见工作区",
		err == nil && strings.Contains(out, ".probe"), detail(res, err)))

	// 9. 私网不可达：容器内 iptables 兜底，不依赖 CNI（OrbStack 上 NetworkPolicy
	//    不生效，实测能直接访问同集群 Pod）。目标选执行体自己的 Service：
	//    规则生效时是 connection refused（exit 7），没生效是 200。
	//    这一条同时也验证了「只拒主动发起」没把回包一起打掉——否则 kubelet 探针会先挂。
	res, err = run(client, "curl -s -m 4 -o /dev/null http://sandbox-runner:8323/healthz; echo $?", sandbox.ModeWorkspaceWrite, mounts, true)
	results = append(results, verdict("私网不可达", err == nil && strings.TrimSpace(res.Stdout) != "0", detail(res, err)))

	// 10. 系统安装可写：system-write 档位下 apt 能装进执行体的可写层。
	//     写的是执行体自己的 /usr（不是空间目录），所以用完立刻删掉探针文件，
	//     不在镜像可写层里留垃圾。
	res, err = run(client, "echo ok > /usr/.wa-probe && rm -f /usr/.wa-probe && echo done",
		sandbox.ModeSystemWrite, mounts, true)
	results = append(results, verdict("系统安装可写",
		err == nil && strings.Contains(res.Stdout, "done"), detail(res, err)))

	failed := 0
	for _, r := range results {
		mark := "PASS"
		if !r.ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-16s %s\n", mark, r.name, r.detail)
	}
	if failed > 0 {
		fmt.Printf("\n%d 项失败：这套执行面不能上线\n", failed)
		os.Exit(1)
	}
	fmt.Printf("\n全部 %d 项通过\n", len(results))
}

// run 用与线上一致的 Spec 形状执行一条命令：超时走默认档（30s），
// 探针自己的墙钟比它短一点，避免「探针卡住」和「执行体卡住」混在一起。
func run(c *remote.Client, script string, mode sandbox.Mode, mounts []sandbox.Mount, network bool) (sandbox.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	return c.Run(ctx, sandbox.Spec{
		Cmd:     []string{"sh", "-c", script},
		Dir:     sandbox.WorkspaceDir,
		Mode:    mode,
		Mounts:  mounts,
		Network: network,
		Timeout: 10 * time.Second,
	})
}

func verdict(name string, ok bool, detail string) check {
	return check{name: name, ok: ok, detail: detail}
}

func detail(res sandbox.Result, err error) string {
	if err != nil {
		return "err=" + err.Error()
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = "(empty)"
	}
	return fmt.Sprintf("exit=%d out=%q", res.ExitCode, truncate(out, 60))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
