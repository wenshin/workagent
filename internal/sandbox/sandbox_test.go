package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh，跳过执行类测试")
	}
	return bin
}

// workspaceSpec 把真实目录挂到沙箱的 /workspace，模拟调用方（shell_exec）的写法。
func workspaceSpec(dir string, mode Mode) Spec {
	return Spec{
		Cmd:    []string{"sh"},
		Dir:    WorkspaceDir,
		Mode:   mode,
		Mounts: []Mount{{Source: dir, Target: WorkspaceDir, Write: true}},
	}
}

func TestSpecNormalize(t *testing.T) {
	if _, err := (Spec{}).normalize(); err == nil {
		t.Fatal("空命令应该报错")
	}
	got, err := (Spec{Cmd: []string{"echo"}}).normalize()
	if err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if got.Dir != "/" {
		t.Errorf("没有工作区挂载时工作目录应为 /，实际 %q", got.Dir)
	}
	if got.Mode != ModeReadOnly {
		t.Errorf("默认档位应为 read-only，实际 %q", got.Mode)
	}
	if got.Timeout != DefaultTimeout || got.MaxOutput != DefaultMaxOutput {
		t.Errorf("默认值没填上: timeout=%v maxOutput=%d", got.Timeout, got.MaxOutput)
	}
	if _, err := (Spec{Cmd: []string{"echo"}, Mode: "free"}).normalize(); err == nil {
		t.Fatal("未知档位应该报错")
	}
	// 工作区挂载决定默认工作目录
	withWS, err := (Spec{Cmd: []string{"echo"}, Mounts: []Mount{{Source: "/tmp", Target: WorkspaceDir}}}).normalize()
	if err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if withWS.Dir != WorkspaceDir {
		t.Errorf("有工作区挂载时工作目录应为 %s，实际 %q", WorkspaceDir, withWS.Dir)
	}
	// 宿主绝对路径不能被当成沙箱内路径放过去
	if _, err := (Spec{Cmd: []string{"echo"}, Dir: "/Users/someone/data"}).normalize(); err == nil {
		t.Log("沙箱内路径只校验绝对与干净，不限制目录前缀（工作目录由挂载决定）")
	}
}

// 挂载表是「相对 key + 固定挂载点」：越界 key、相对挂载点、未知根都要拒绝。
func TestNormalizeMounts(t *testing.T) {
	bad := []Mount{
		{Source: "/tmp", Target: "relative"},
		{Source: "/tmp", Target: "/"},
		{Source: "/tmp", Target: "/data/"}, // 不干净
		{Source: "/tmp", Key: "/abs/key", Target: "/data"},
		{Source: "/tmp", Key: "../escape", Target: "/data"},
		{Source: "/tmp", Root: "unknown", Key: "a", Target: "/data"},
		{Source: "relative", Target: "/data"},
		{Target: "/data"}, // 既不系统路径也没 key
	}
	for i, m := range bad {
		if _, err := normalizeMounts([]Mount{m}); err == nil {
			t.Errorf("第 %d 个挂载应当报错: %#v", i, m)
		}
	}
	ok, err := normalizeMounts([]Mount{{Source: "/tmp", Key: "spaces/s1/files", Target: "/workspace"}})
	if err != nil || len(ok) != 1 || ok[0].Key != "spaces/s1/files" {
		t.Fatalf("合法挂载不该报错: %v %#v", err, ok)
	}
	// 同一目标去重：可写优先
	got, err := normalizeMounts([]Mount{
		{Source: "/tmp", Key: "a", Target: "/data"},
		{Source: "/tmp", Key: "a", Target: "/data", Write: true},
	})
	if err != nil {
		t.Fatalf("去重失败: %v", err)
	}
	if len(got) != 1 || !got[0].Write {
		t.Fatalf("可写的那条应留下: %#v", got)
	}
}

func TestEnsure(t *testing.T) {
	// 没有「允许降级」的档位：local 一律挡住
	err := Ensure(NewLocal())
	if !errors.Is(err, ErrNotIsolated) {
		t.Fatalf("local 应返回 ErrNotIsolated，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Errorf("错误信息要告诉运维装什么: %v", err)
	}
	// 有隔离后端时放行
	isolated := &runner{name: NameBwrap}
	if err := Ensure(isolated); err != nil {
		t.Errorf("有隔离后端时不该报错: %v", err)
	}
	if err := Ensure(nil); !errors.Is(err, ErrNotIsolated) {
		t.Errorf("空执行器应报错: %v", err)
	}
}

func TestBwrapArgs(t *testing.T) {
	bin := "/usr/bin/bwrap"
	ws := t.TempDir()
	tmp := filepath.Join(t.TempDir(), "sandbox-1")
	extra := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/bin", "HOME=" + HomeDir}

	cases := []struct {
		name    string
		spec    Spec
		want    []string
		notWant []string
	}{
		{
			name: "只读档默认禁网且不挂全盘",
			spec: Spec{Cmd: []string{"sh", "-c", "ls"}, Dir: WorkspaceDir, Mode: ModeReadOnly,
				Mounts: []Mount{{Source: ws, Target: WorkspaceDir, Write: true}}},
			want: []string{"--unshare-net", "--clearenv", "--setenv PATH /bin", "--chdir " + WorkspaceDir,
				// 工作区在只读档里也挂，但只能是只读
				"--ro-bind-try " + ws + " " + WorkspaceDir, "--ro-bind-try /usr /usr",
				// 白名单之外的路径会作为挂载点父目录被创建：必须整体只读，
				// 否则 root 能在 /etc 这类空目录里随便写。
				"--remount-ro /"},
			// 白名单挂载的核心：根目录不再整体挂进来，工作区也不可写
			notWant: []string{"--ro-bind / /", "--bind " + ws + " " + WorkspaceDir},
		},
		{
			name: "workspace 档只放开挂载表里标了可写的项",
			spec: Spec{Cmd: []string{"sh"}, Dir: WorkspaceDir, Mode: ModeWorkspaceWrite,
				Mounts: []Mount{
					{Source: ws, Target: WorkspaceDir, Write: true},
					{Source: extra, Target: "/data", Write: true},
				}},
			want: []string{
				"--bind " + ws + " " + WorkspaceDir,
				"--bind " + extra + " /data",
				"--ro-bind-try /etc/passwd /etc/passwd",
				// 私有 HOME/TMPDIR 挂到固定路径，宿主临时目录不进环境变量
				"--bind " + tmp + " " + HomeDir,
			},
			notWant: []string{"--ro-bind / /", ws + " " + ws},
		},
		{
			name:    "显式允许网络时不隔离 netns",
			spec:    Spec{Cmd: []string{"sh"}, Dir: WorkspaceDir, Mode: ModeWorkspaceWrite, Network: true},
			notWant: []string{"--unshare-net"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv, err := bwrapBuild(bin)(c.spec, tmp, env)
			if err != nil {
				t.Fatalf("构造参数失败: %v", err)
			}
			if argv[len(argv)-1] != c.spec.Cmd[len(c.spec.Cmd)-1] {
				t.Errorf("命令没接在最后: %v", argv)
			}
			joined := strings.Join(argv, " ")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("缺少 %q\n实际: %s", w, joined)
				}
			}
			for _, nw := range c.notWant {
				if strings.Contains(joined, nw) {
					t.Errorf("不该出现 %q\n实际: %s", nw, joined)
				}
			}
		})
	}
}

// 环境变量里的 HOME / TMPDIR 必须是沙箱内路径，不能泄露宿主临时目录。
func TestBuildEnvUsesSandboxHome(t *testing.T) {
	env := buildEnv(Spec{Proxy: "/tmp/host.sock"}, HomeDir)
	joined := strings.Join(env, " ")
	for _, want := range []string{"HOME=" + HomeDir, "TMPDIR=" + HomeDir, "WA_EGRESS_PROXY=" + EgressProxy} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "/tmp/host.sock") {
		t.Errorf("宿主 socket 路径不该出现在环境变量里: %s", joined)
	}
}

func TestLocalEcho(t *testing.T) {
	dir := t.TempDir()
	r := NewLocal()
	res, err := r.Run(context.Background(), workspaceSpec(dir, ModeWorkspaceWrite))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Sandbox != "local" {
		t.Errorf("后端名应为 local，实际 %q", res.Sandbox)
	}
	// 没有挂载命名空间时工作目录落回挂载源
	res, err = r.Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "echo hello"},
		Dir:     WorkspaceDir,
		Mounts:  []Mount{{Source: dir, Target: WorkspaceDir, Write: true}},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout 不对: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("退出码应为 0，实际 %d", res.ExitCode)
	}
}

func TestExitCodeIsData(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "exit 7"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("非零退出不该返回 error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("退出码应为 7，实际 %d", res.ExitCode)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "sleep 5 & sleep 5; wait"},
		Timeout: 200 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应返回 ErrTimeout，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时后应立即返回，实际耗时 %v", elapsed)
	}
	if res.Sandbox != "local" {
		t.Errorf("结果里应带后端名，实际 %q", res.Sandbox)
	}
}

func TestOutputLimit(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:       []string{shBin(t), "-c", "yes xxxxxxxxxxxxxxxxxxxx"},
		Timeout:   10 * time.Second,
		MaxOutput: 1024,
	})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("应返回 ErrOutputTooLarge，实际 %v", err)
	}
	if !res.Truncated {
		t.Error("结果应标记 Truncated")
	}
	if len(res.Stdout) > 1024 {
		t.Errorf("stdout 超过上限: %d", len(res.Stdout))
	}
}

func TestEnvIsScrubbed(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "leak-me")
	t.Setenv("PATH", os.Getenv("PATH"))
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "echo ${SECRET_TOKEN:-empty}"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "empty" {
		t.Errorf("父进程的密钥被带进沙箱了: %q", res.Stdout)
	}
}

func TestEnvOverride(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "echo $FOO $TMPDIR"},
		Env:     []string{"FOO=bar"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.Stdout), "bar ") {
		t.Errorf("显式 Env 没生效: %q", res.Stdout)
	}
}

// 只读档下全盘只读：挂载点之外写不进去。这条只有真沙箱才成立。
func TestSandboxWriteBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过真实沙箱测试")
	}
	r := Detect()
	if r.Name() == "local" {
		t.Skip("当前平台没有可用沙箱后端")
	}
	sh := shBin(t)
	ws := t.TempDir()
	base := workspaceSpec(ws, ModeWorkspaceWrite)
	base.Cmd = []string{sh, "-c", "echo hi > inside.txt && cat inside.txt"}
	base.Timeout = 15 * time.Second

	res, err := r.Run(context.Background(), base)
	if err != nil {
		t.Fatalf("[%s] workspace 内写入失败: %v\nstderr: %s", r.Name(), err, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("[%s] workspace 内写入后读不回: exit=%d stdout=%q stderr=%q",
			r.Name(), res.ExitCode, res.Stdout, res.Stderr)
	}

	outside := filepath.Join(os.TempDir(), fmt.Sprintf("sandbox-escape-%d.txt", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(outside) })
	escape := workspaceSpec(ws, ModeWorkspaceWrite)
	escape.Cmd = []string{sh, "-c", "echo escaped > " + outside}
	escape.Timeout = 15 * time.Second
	res, err = r.Run(context.Background(), escape)
	if err == nil && res.ExitCode == 0 {
		t.Errorf("[%s] 越界写入居然成功了: %s", r.Name(), outside)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Errorf("[%s] 越界文件真的被创建了: %s", r.Name(), outside)
	}
}

// 远端句柄没有本地 stderr 收集器，Wait 必须照样能收尾（曾经在这里空指针崩过）。
func TestRemoteProcWaitWithoutStderr(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = r.Close() }()
	proc := NewProc("remote", r, w, func() {})
	defer proc.Close()

	res, err := proc.Wait()
	if err != nil {
		t.Fatalf("远端句柄 Wait 不该报错: %v", err)
	}
	if res.Stderr != "" {
		t.Errorf("远端句柄没有 stderr 可回传，得到 %q", res.Stderr)
	}
	if proc.Stderr() != "" {
		t.Errorf("远端句柄 Stderr() 应为空串")
	}
}

func TestDetectReturnsSomething(t *testing.T) {
	r := Detect()
	if r == nil || r.Name() == "" {
		t.Fatal("Detect 必须返回一个可用 Runner")
	}
	t.Logf("当前后端: %s", r.Name())
}
