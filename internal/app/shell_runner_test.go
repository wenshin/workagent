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
// remote 也已删除：所有空间共用一个 /usr，开发与生产的隔离形态会不一致。
func TestServerRunnerRejectsLocalBackend(t *testing.T) {
	for _, backend := range []string{"local", "bwrap", "remote"} {
		_, err := newShellRunner(&config.Config{
			Role:           config.RoleServer,
			SandboxBackend: backend,
		}, nil, discardLogger())
		if err == nil {
			t.Fatalf("服务端配 %q 应该拒绝", backend)
		}
		if !strings.Contains(err.Error(), "WA_ROLE=server") {
			t.Fatalf("错误应点明形态约束，实际 %v", err)
		}
	}
}

// 服务端没配令牌时不能连执行体：容器内的 agent 与调用方之间必须有鉴权。
func TestServerRunnerNeedsToken(t *testing.T) {
	_, err := newShellRunner(&config.Config{
		Role:           config.RoleServer,
		SandboxBackend: "k8s",
	}, nil, discardLogger())
	if err == nil {
		t.Fatal("k8s 形态缺令牌应该失败")
	}
	if !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("错误应点明缺令牌，实际 %v", err)
	}
}

// dev 已删除：服务端与开发机不再有「本机执行面」这一档，配置层与装配层都要拒绝。
func TestShellRunnerRejectsDevRole(t *testing.T) {
	_, err := newShellRunner(&config.Config{
		Role:           "dev",
		SandboxBackend: "remote",
	}, nil, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "未知运行形态") {
		t.Fatalf("dev 形态应拒绝，实际 %v", err)
	}
}

// 客户端不接受容器化执行体：用户目录不能交给另一个进程。
func TestClientRunnerRejectsRemoteBackend(t *testing.T) {
	_, err := newShellRunner(&config.Config{
		Role:           config.RoleClient,
		SandboxBackend: "k8s",
	}, nil, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "WA_ROLE=client") {
		t.Fatalf("客户端配 k8s 应拒绝，实际 %v", err)
	}
}

// 客户端只有 bwrap 一个选项：探测不到就启动失败，不退回裸跑。
func TestClientRunnerOnlyAcceptsBwrap(t *testing.T) {
	runner, err := newShellRunner(&config.Config{
		Role:           config.RoleClient,
		SandboxBackend: "local",
	}, nil, discardLogger())
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
