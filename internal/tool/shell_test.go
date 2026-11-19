package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/policy"
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

// 徽标按命令判：只读命令显示「只读」，写操作与判不准的显示「写入」。
func TestShellRiskByCommand(t *testing.T) {
	sh := shellExec()
	cases := []struct {
		command string
		want    int
	}{
		// 只读：白名单命令、无重定向
		{"ls -la", policy.RiskReadOnly},
		{"cat data.csv | wc -l", policy.RiskReadOnly},
		{"grep -rn foo . 2>/dev/null", policy.RiskReadOnly},
		{"find . -name '*.md'", policy.RiskReadOnly},
		{"sort a.txt", policy.RiskReadOnly},
		{"git status", policy.RiskReadOnly},
		{"git log --oneline -5", policy.RiskReadOnly},
		{"git diff --stat", policy.RiskReadOnly},
		{"echo hello", policy.RiskReadOnly},
		// 线上真实出现过的形态（会话 d72ce277 的三条 grep）
		{`grep -n "^### Q1[3-8]\|^## \|^### Q1[1-9]" 面试题.md`, policy.RiskReadOnly},
		{`grep -n "Q1[3-9]\|Q2[0-9]" 面试题.md | grep -v "^.*### Q" ; echo "---目录检查---"; grep -n "目录\|题号" 面试题.md`, policy.RiskReadOnly},
		{`grep -n "分布式事务\|2PC\|XA" 面试题.md | head -30`, policy.RiskReadOnly},
		{`grep -n "Q19\|Q30" 面试题.md | grep -v "^[0-9]*:### Q"; echo "exit=$?"`, policy.RiskReadOnly},

		// 写入：重定向 / 白名单外 / 解释器 / 把读变成写的参数
		{"echo hello > a.txt", policy.RiskWrite},
		{"echo hello >> a.txt", policy.RiskWrite},
		{"rm -rf data", policy.RiskWrite},
		{"mkdir -p out", policy.RiskWrite},
		{"cp a.txt out/", policy.RiskWrite},
		{"python3 analyze.py", policy.RiskWrite},
		{"find . -name '*.tmp' -delete", policy.RiskWrite},
		{"find . -exec grep -l foo {} +", policy.RiskWrite},
		{"sort -o b.txt a.txt", policy.RiskWrite},
		{"git commit -m 'x'", policy.RiskWrite},
		{"git push origin main", policy.RiskWrite},
		{"cat a.txt | tee out.txt", policy.RiskWrite},
		{"echo $(date)", policy.RiskWrite},
		{"ls | xargs rm", policy.RiskWrite},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": tc.command})
			if got := sh.EffectiveRisk(args); got != tc.want {
				t.Fatalf("命令 %q 展示等级 %d，期望 %d", tc.command, got, tc.want)
			}
		})
	}
}

// 判不出来就退回声明等级，不猜「大概是只读」。
func TestEffectiveRiskFallback(t *testing.T) {
	sh := shellExec()
	for _, args := range []string{`{}`, `{"command":""}`, `not-json`} {
		if got := sh.EffectiveRisk(json.RawMessage(args)); got != policy.RiskWrite {
			t.Fatalf("参数 %s 应退回声明的写入等级，实际 %d", args, got)
		}
	}
	plain := Tool{Name: "file_read", Risk: policy.RiskReadOnly}
	if got := plain.EffectiveRisk(json.RawMessage(`{"path":"a"}`)); got != policy.RiskReadOnly {
		t.Fatalf("没有 RiskFor 的工具应直接用声明等级，实际 %d", got)
	}
	// RiskFor 返回越界值时同样退回声明，不把非法值传给前端
	bad := Tool{Name: "x", Risk: policy.RiskWrite, RiskFor: func(Tool, json.RawMessage) int { return 9 }}
	if got := bad.EffectiveRisk(nil); got != policy.RiskWrite {
		t.Fatalf("非法细化值应退回声明等级，实际 %d", got)
	}
}
