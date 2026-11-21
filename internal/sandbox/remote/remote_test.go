package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// fakeRunner 是执行体的替身：记录收到的 Spec，返回预设结果。
type fakeRunner struct {
	name string
	res  sandbox.Result
	err  error
	got  sandbox.Spec
}

func (f *fakeRunner) Name() string { return f.name }

func (f *fakeRunner) Run(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	f.got = spec
	res := f.res
	if res.Sandbox == "" {
		res.Sandbox = f.name
	}
	return res, f.err
}

func (f *fakeRunner) Start(context.Context, sandbox.Spec) (*sandbox.Proc, error) {
	return nil, errors.New("not implemented")
}

func newTestClient(t *testing.T, runner sandbox.Runner, token, root string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(Handler(runner, token, root, sandbox.EnvFacts{}, nil, nil))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, token)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c, srv
}

// passthroughRunner 用本地后端真正起进程：常驻通道要验证的是「流能不能双向走」，
// 用假 Runner 只能测到序列化，测不到 EOF 与背压。
type passthroughRunner struct {
	name string
}

func (p *passthroughRunner) Name() string { return p.name }

func (p *passthroughRunner) Run(context.Context, sandbox.Spec) (sandbox.Result, error) {
	return sandbox.Result{Sandbox: p.name}, nil
}

func (p *passthroughRunner) Start(ctx context.Context, spec sandbox.Spec) (*sandbox.Proc, error) {
	return sandbox.NewLocal().Start(ctx, spec)
}

// Spec 必须原样穿过协议：Mode / Timeout / FS / Stdin 任何一项丢了，
// 远端执行和本地执行就不是同一个语义了。
func TestRunRoundTrip(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameBwrap, res: sandbox.Result{ExitCode: 0, Stdout: "ok"}}
	c, _ := newTestClient(t, runner, "tk", "/var/lib/workagent")

	start := time.Now()
	res, err := c.Run(context.Background(), sandbox.Spec{
		Cmd:     []string{"sh", "-c", "echo ok"},
		Dir:     "/var/lib/workagent/storage/spaces/s1/files",
		FS:      sandbox.FS{Read: []string{"/var/lib/workagent/storage/spaces/s1/files"}},
		Mode:    sandbox.ModeWorkspaceWrite,
		Timeout: 5 * time.Second,
		Stdin:   []byte("input"),
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Stdout != "ok" || res.Sandbox != sandbox.NameBwrap {
		t.Fatalf("结果不符: %+v", res)
	}
	if got := runner.got; got.Mode != sandbox.ModeWorkspaceWrite ||
		got.Timeout != 5*time.Second ||
		string(got.Stdin) != "input" ||
		len(got.FS.Read) != 1 {
		t.Fatalf("Spec 在传输中被改写: %+v", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("往返耗时异常")
	}
}

// 超时与输出超限必须以 errors.Is 的形式回到调用方：
// tool 层靠这个判断「命令没跑完，是结果不是平台故障」。
func TestErrorKindsArePreserved(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"timeout", fmtErr(sandbox.ErrTimeout), sandbox.ErrTimeout},
		{"output", fmtErr(sandbox.ErrOutputTooLarge), sandbox.ErrOutputTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{name: sandbox.NameBwrap, err: tc.err}
			c, _ := newTestClient(t, runner, "tk", "")
			_, err := c.Run(context.Background(), sandbox.Spec{
				Cmd: []string{"sh", "-c", "x"},
				Dir: "/tmp",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误类型丢失: %v", err)
			}
		})
	}
}

func fmtErr(base error) error { return &wrapped{base: base} }

type wrapped struct{ base error }

func (w *wrapped) Error() string { return "远程执行失败: " + w.base.Error() }
func (w *wrapped) Unwrap() error { return w.base }

func TestAuthRequired(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameBwrap}
	srv := httptest.NewServer(Handler(runner, "right", "", sandbox.EnvFacts{}, nil, nil))
	t.Cleanup(srv.Close)

	// 令牌不对：握手就失败
	if _, err := New(srv.URL, "wrong"); err == nil {
		t.Fatal("错误令牌应该握手失败")
	}
	// 空令牌：根本不允许构造客户端
	if _, err := New(srv.URL, ""); err == nil {
		t.Fatal("空令牌应该直接拒绝")
	}
}

// 执行体是特权容器：请求里的路径必须落在允许的根目录之内。
func TestPathEscapesRootRejected(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameBwrap}
	c, _ := newTestClient(t, runner, "tk", "/var/lib/workagent")

	_, err := c.Run(context.Background(), sandbox.Spec{
		Cmd: []string{"sh", "-c", "id"},
		Dir: "/etc",
	})
	if err == nil {
		t.Fatal("越界路径应该被拒绝")
	}
	if !strings.Contains(err.Error(), "允许的根目录") {
		t.Fatalf("错误信息应说明越界原因: %v", err)
	}
}

// 常驻通道必须是真正的双向流：写一帧、读回同一帧。
//
// 这条坏掉的表现是插件「启动成功但永远不返回」——所以不能只测
// 「Start 不报错」，必须跑一次往返。
func TestStartDuplexRoundTrip(t *testing.T) {
	c, _ := newTestClient(t, &passthroughRunner{name: sandbox.NameLocal}, "tk", "")
	proc, err := c.Start(context.Background(), sandbox.Spec{
		Cmd:      []string{"/bin/cat"},
		Dir:      "/tmp",
		Resident: true,
	})
	if err != nil {
		t.Fatalf("启动常驻进程失败: %v", err)
	}
	defer proc.Close()

	if err := proc.Write([]byte("ping\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	line, err := proc.Stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if line != "ping\n" {
		t.Fatalf("往返内容不符: %q", line)
	}

	// Close 必须真的把远端进程断掉并能返回，否则常驻池会一直挂着连接。
	done := make(chan struct{})
	go func() {
		proc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 没有返回")
	}
}

// 执行体起不来进程时，错误必须原样回到控制面，而不是变成一个空结果。
func TestStartPropagatesError(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameBwrap}
	c, _ := newTestClient(t, runner, "tk", "")
	_, err := c.Start(context.Background(), sandbox.Spec{Cmd: []string{"sh"}})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("错误没有透传: %v", err)
	}
}

// 出网代理路径是执行体要 bind 的位置：不在白名单目录里必须拒绝，
// 否则等于允许控制面在任意路径建文件。
func TestEgressPathRejected(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameBwrap}
	c, _ := newTestClient(t, runner, "tk", "")
	_, err := c.Run(context.Background(), sandbox.Spec{
		Cmd:    []string{"sh", "-c", "true"},
		Dir:    "/tmp",
		Proxy:  "/etc/evil.sock",
		Egress: &sandbox.EgressSpec{Hosts: []string{"example.com"}},
	})
	if err == nil || !strings.Contains(err.Error(), "允许的位置") {
		t.Fatalf("非法代理路径应被拒绝: %v", err)
	}
}

// 执行体报告 local 时，控制面必须能看出来。
func TestLocalBackendVisible(t *testing.T) {
	runner := &fakeRunner{name: sandbox.NameLocal}
	c, _ := newTestClient(t, runner, "tk", "")
	if c.Name() != sandbox.NameLocal || c.Isolated() {
		t.Fatalf("local 后端没有如实上报: name=%s isolated=%v", c.Name(), c.Isolated())
	}
	if err := sandbox.Ensure(c, true); err == nil {
		t.Fatal("要求隔离时 local 必须被拒绝")
	}
}

// 环境自述随握手一起拿到：少一次往返，也避免「后端名」和「环境」两次请求
// 落在不同的执行体实例上（重建期间会真的发生）。
func TestEnvFactsTravelWithHandshake(t *testing.T) {
	facts := sandbox.EnvFacts{
		OS:      "debian 12 (linux/arm64)",
		Tools:   []sandbox.ToolFact{{Name: "git", Version: "2.39.5"}},
		Network: "默认断网",
	}
	srv := httptest.NewServer(Handler(&fakeRunner{name: sandbox.NameBwrap}, "t", "", facts, nil, nil))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "t")
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	got := c.Env()
	if got.OS != facts.OS || len(got.Tools) != 1 || got.Tools[0].Name != "git" {
		t.Fatalf("环境未随握手带回: %+v", got)
	}
	if c.EnvHash() == "" {
		t.Fatal("应能算出环境摘要，用于日志对账")
	}
}

// 没有自述文件时不能凭空造一个环境：空事实要原样透传，由提示词层整段省略。
func TestEnvFactsEmptyWhenNotReported(t *testing.T) {
	c, _ := newTestClient(t, &fakeRunner{name: sandbox.NameBwrap}, "t", "")
	if !c.Env().Empty() {
		t.Fatalf("未上报时 Env() 应为空: %+v", c.Env())
	}
}
