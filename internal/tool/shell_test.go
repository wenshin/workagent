package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/storage/localfs"
)

// nonLocalStorage 模拟「将来换成 S3」：接口齐全但没有本地路径。
type nonLocalStorage struct{ storage.Storage }

func newShellEnv(t *testing.T, runner sandbox.Runner) Env {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	return Env{SpaceID: "sp1", Storage: fs, Shell: runner}
}

func runShell(t *testing.T, env Env, args string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh，跳过命令执行测试")
	}
	return shellExec().Invoke(context.Background(), json.RawMessage(args), env)
}

func TestShellExecWritesInWorkspace(t *testing.T) {
	env := newShellEnv(t, sandbox.NewLocal())
	out, err := runShell(t, env, `{"command":"echo hello > a.txt && cat a.txt"}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "退出码 0") {
		t.Errorf("缺少退出码摘要: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("缺少 stdout: %s", out)
	}

	local, ok := env.Storage.(localPathStore)
	if !ok {
		t.Fatal("测试用的存储应支持 LocalPath")
	}
	path, err := local.LocalPath("spaces/sp1/files/a.txt")
	if err != nil {
		t.Fatalf("解析路径失败: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("命令应写进空间文件目录: %v", statErr)
	}
}

func TestShellExecNonZeroIsResult(t *testing.T) {
	out, err := runShell(t, newShellEnv(t, sandbox.NewLocal()), `{"command":"echo boom >&2; exit 3"}`)
	if err != nil {
		t.Fatalf("非零退出不该是 error: %v", err)
	}
	if !strings.Contains(out, "退出码 3") {
		t.Errorf("缺少退出码: %s", out)
	}
	if !strings.Contains(out, "[stderr]") || !strings.Contains(out, "boom") {
		t.Errorf("stderr 应单独成段: %s", out)
	}
}

func TestShellExecTimeout(t *testing.T) {
	out, err := runShell(t, newShellEnv(t, sandbox.NewLocal()), `{"command":"sleep 5","timeout_seconds":1}`)
	if err != nil {
		t.Fatalf("超时应作为结果回给模型: %v", err)
	}
	if !strings.Contains(out, "超时") {
		t.Errorf("缺少超时说明: %s", out)
	}
}

func TestShellExecUnavailable(t *testing.T) {
	env := newShellEnv(t, nil)
	if _, err := runShell(t, env, `{"command":"echo hi"}`); err == nil {
		t.Fatal("没有沙箱后端时应报错")
	}
	if _, err := runShell(t, env, `{"command":""}`); err == nil {
		t.Fatal("空命令应报错")
	}
}

func TestShellExecNeedsLocalStorage(t *testing.T) {
	env := Env{SpaceID: "sp1", Storage: &nonLocalStorage{}, Shell: sandbox.NewLocal()}
	if _, err := runShell(t, env, `{"command":"echo hi"}`); err == nil {
		t.Fatal("存储不支持本地路径时应给出明确错误")
	}
}
