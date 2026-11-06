package sandbox

import (
	"context"
	"errors"
	"fmt"
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

func TestSpecNormalize(t *testing.T) {
	if _, err := (Spec{}).normalize(); err == nil {
		t.Fatal("空命令应该报错")
	}
	got, err := (Spec{Cmd: []string{"echo"}, Dir: t.TempDir()}).normalize()
	if err != nil {
		t.Fatalf("normalize 失败: %v", err)
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
}

func TestEnsure(t *testing.T) {
	// 开发机默认允许降级
	if err := Ensure(NewLocal(), false); err != nil {
		t.Errorf("非强制模式不该因为 local 报错: %v", err)
	}
	// 生产必须挡住
	err := Ensure(NewLocal(), true)
	if !errors.Is(err, ErrNotIsolated) {
		t.Fatalf("强制模式下 local 应返回 ErrNotIsolated，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Errorf("错误信息要告诉运维装什么: %v", err)
	}
	// 有隔离后端时两侧都放行
	isolated := &runner{name: NameBwrap}
	if err := Ensure(isolated, true); err != nil {
		t.Errorf("有隔离后端时不该报错: %v", err)
	}
	if err := Ensure(nil, false); !errors.Is(err, ErrNotIsolated) {
		t.Errorf("空执行器应报错: %v", err)
	}
}

func TestBwrapArgs(t *testing.T) {
	bin := "/usr/bin/bwrap"
	ws := "/work/space"
	tmp := "/tmp/sandbox-1"
	env := []string{"PATH=/bin", "HOME=" + tmp}

	cases := []struct {
		name    string
		spec    Spec
		want    []string
		notWant []string
	}{
		{
			name:    "只读档默认禁网",
			spec:    Spec{Cmd: []string{"sh", "-c", "ls"}, Dir: ws, Mode: ModeReadOnly},
			want:    []string{"--ro-bind / /", "--unshare-net", "--clearenv", "--setenv PATH /bin", "--chdir " + ws},
			notWant: []string{"--bind " + ws},
		},
		{
			name: "workspace 档多挂一个可写目录",
			spec: Spec{Cmd: []string{"sh"}, Dir: ws, Mode: ModeWorkspaceWrite},
			want: []string{"--bind " + ws + " " + ws, "--bind " + tmp + " " + tmp},
		},
		{
			name:    "显式允许网络时不隔离 netns",
			spec:    Spec{Cmd: []string{"sh"}, Dir: ws, Mode: ModeWorkspaceWrite, Network: true},
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

func TestSeatbeltProfile(t *testing.T) {
	ws := "/work/space"
	tmp := "/private/tmp/sandbox-1"

	ro, err := seatbeltProfile(Spec{Dir: ws, Mode: ModeReadOnly}, tmp)
	if err != nil {
		t.Fatalf("生成 profile 失败: %v", err)
	}
	for _, w := range []string{"(deny default)", `(subpath "` + tmp + `")`, "(deny network*)"} {
		if !strings.Contains(ro, w) {
			t.Errorf("只读 profile 缺少 %q\n%s", w, ro)
		}
	}
	if strings.Contains(ro, ws) {
		t.Errorf("只读档不该给 workspace 写权限\n%s", ro)
	}

	rw, err := seatbeltProfile(Spec{Dir: ws, Mode: ModeWorkspaceWrite, Network: true}, tmp)
	if err != nil {
		t.Fatalf("生成 profile 失败: %v", err)
	}
	if !strings.Contains(rw, `(subpath "`+ws+`")`) {
		t.Errorf("workspace 档应放开 workspace 写权限\n%s", rw)
	}
	if strings.Contains(rw, "(deny network*)") {
		t.Errorf("Network=true 时不该写死禁网\n%s", rw)
	}

	if _, err := seatbeltProfile(Spec{Dir: `/evil"path`, Mode: ModeWorkspaceWrite}, "/tmp"); err == nil {
		t.Error("含引号的路径应该被拒绝，而不是拼进 profile")
	}
}

func TestLocalEcho(t *testing.T) {
	r := NewLocal()
	res, err := r.Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "echo hello"},
		Dir:     t.TempDir(),
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
	if res.Sandbox != "local" {
		t.Errorf("后端名应为 local，实际 %q", res.Sandbox)
	}
}

func TestExitCodeIsData(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Spec{
		Cmd:     []string{shBin(t), "-c", "exit 7"},
		Dir:     t.TempDir(),
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
		Dir:     t.TempDir(),
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
		Dir:       t.TempDir(),
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
		Dir:     t.TempDir(),
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
		Dir:     t.TempDir(),
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

// 只读档下全盘只读：临时目录之外写不进去。这条只有真沙箱才成立。
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

	res, err := r.Run(context.Background(), Spec{
		Cmd:     []string{sh, "-c", "echo hi > inside.txt && cat inside.txt"},
		Dir:     ws,
		Mode:    ModeWorkspaceWrite,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("[%s] workspace 内写入失败: %v\nstderr: %s", r.Name(), err, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("[%s] workspace 内写入后读不回: %q / %s", r.Name(), res.Stdout, res.Stderr)
	}

	outside := filepath.Join(os.TempDir(), fmt.Sprintf("sandbox-escape-%d.txt", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(outside) })
	res, err = r.Run(context.Background(), Spec{
		Cmd:     []string{sh, "-c", "echo escaped > " + outside},
		Dir:     ws,
		Mode:    ModeWorkspaceWrite,
		Timeout: 15 * time.Second,
	})
	if err == nil && res.ExitCode == 0 {
		t.Errorf("[%s] 越界写入居然成功了: %s", r.Name(), outside)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Errorf("[%s] 越界文件真的被创建了: %s", r.Name(), outside)
	}
}

func TestDetectReturnsSomething(t *testing.T) {
	r := Detect()
	if r == nil || r.Name() == "" {
		t.Fatal("Detect 必须返回一个可用 Runner")
	}
	t.Logf("当前后端: %s", r.Name())
}
