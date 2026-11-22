package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/sandbox"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 服务端不接受本机执行面：那是「镜像恰好装了 bwrap」的赌博。
func TestServerRunnerRejectsLocalBackend(t *testing.T) {
	for _, backend := range []string{"local", "", "bwrap"} {
		_, err := newShellRunner(&config.Config{
			Role:           config.RoleServer,
			SandboxBackend: backend,
		}, discardLogger())
		if err == nil {
			t.Fatalf("服务端配 %q 应该拒绝", backend)
		}
		if !strings.Contains(err.Error(), "WA_ROLE=server") {
			t.Fatalf("错误应点明形态约束，实际 %v", err)
		}
	}
}

// 服务端没配执行体地址时不能悄悄退回本机。
func TestServerRunnerNeedsRunnerAddr(t *testing.T) {
	_, err := newShellRunner(&config.Config{
		Role:           config.RoleServer,
		SandboxBackend: "remote",
	}, discardLogger())
	if err == nil {
		t.Fatal("remote 形态缺地址应该失败")
	}
}

// 客户端不接受远端执行体：用户目录不能交给另一个进程。
func TestClientRunnerRejectsRemoteBackend(t *testing.T) {
	_, err := newShellRunner(&config.Config{
		Role:           config.RoleClient,
		SandboxBackend: "remote",
	}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "WA_ROLE=client") {
		t.Fatalf("客户端配 remote 应拒绝，实际 %v", err)
	}
}

// 客户端只有 bwrap 一个选项：探测不到就启动失败，不退回 sandbox-exec / 裸跑。
func TestClientRunnerOnlyAcceptsBwrap(t *testing.T) {
	runner, err := newShellRunner(&config.Config{
		Role:           config.RoleClient,
		SandboxBackend: "local",
	}, discardLogger())
	if err != nil {
		if !strings.Contains(err.Error(), "bwrap") {
			t.Fatalf("错误应点明缺 bwrap，实际 %v", err)
		}
		return
	}
	if runner.Name() != sandbox.NameBwrap {
		t.Fatalf("客户端只能用 bwrap，实际 %s", runner.Name())
	}
}
