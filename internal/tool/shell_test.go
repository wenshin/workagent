package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// 命令前拼的是平台固定的 venv 自愈脚本，不能改变用户命令的语义：
// heredoc、管道、退出码都要照旧（模型写的多行脚本全靠这个）。
func TestVenvBootstrapKeepsCommandSemantics(t *testing.T) {
	env := newShellEnv(t, sandbox.NewLocal())
	out, err := runShell(t, env, `{"command":"cat <<'EOF'\nhello\nEOF\necho done"}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "退出码 0") || !strings.Contains(out, "hello") || !strings.Contains(out, "done") {
		t.Fatalf("前缀不该影响 heredoc 与后续命令: %s", out)
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
		{"cd /workspace && ls", policy.RiskReadOnly},
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

// 等级只有四档，脚本、出网、写文件、删文件不该都显示成「写入」。
func TestShellRiskLabelByCommand(t *testing.T) {
	sh := shellExec()
	cases := []struct {
		command string
		want    string
	}{
		// 只读：整条命令都在白名单里
		{"ls -la", RiskLabelReadOnly},
		{"cat a.log | grep error", RiskLabelReadOnly},
		{"find . -name '*.md'", RiskLabelReadOnly},

		// 出网
		{"curl -sS https://example.com/api", RiskLabelNetwork},
		{"wget -q https://example.com/a.tgz", RiskLabelNetwork},

		// 装依赖
		{"pip install requests", RiskLabelInstall},
		{"npm i lodash", RiskLabelInstall},

		// 跑代码：解释器与脚本文件
		{"python3 train.py", RiskLabelScript},
		{"bash ./build.sh", RiskLabelScript},

		// 落地文件
		{"echo hi > out.txt", RiskLabelWriteFile},
		{"mkdir -p build/cache", RiskLabelWriteFile},
		{"sed -i 's/a/b/' conf.yaml", RiskLabelWriteFile},

		// 删除比写入更该被看见
		{"rm -rf build", RiskLabelDeleteFile},
		{"find . -name '*.tmp' -delete", RiskLabelDeleteFile},

		// 整篇写代码：heredoc 正文里的 python 是数据，不是在跑脚本
		{"cat > app.py <<'EOF'\nimport os\nos.system('ls')\nEOF", RiskLabelWriteFile},

		// cd 只是换目录，不该把整条命令带成写入
		{"cd /workspace && ls -la", RiskLabelReadOnly},

		// git 看子命令：出网 / 改工作区 / 只读
		{"git push origin main", RiskLabelNetwork},
		{"git clone https://example.com/a.git", RiskLabelNetwork},
		{"git checkout -- src/", RiskLabelWriteFile},
		{"git status", RiskLabelReadOnly},

		// 判不准就不给标签，前端退回等级文案「写入」
		{"docker ps", ""},
	}
	for _, tc := range cases {
		args, err := json.Marshal(map[string]string{"command": tc.command})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := sh.EffectiveRiskLabel(args); got != tc.want {
			t.Errorf("%q 的标签 = %q，期望 %q", tc.command, got, tc.want)
		}
	}
}

// 没有 RiskLabelFor 的工具必须返回空串，由前端退回等级文案。
func TestEffectiveRiskLabelFallback(t *testing.T) {
	if got := (Tool{Name: "x"}).EffectiveRiskLabel(json.RawMessage(`{}`)); got != "" {
		t.Fatalf("未声明标签的工具应返回空串，得到 %q", got)
	}
}

// 墙钟按命令类别给默认值：装依赖与 `ls` 的合理耗时差两个数量级，
// 一个常量只能取最小值（长命令全被截断）或最大值（挂住的命令也要等半小时）。
func TestShellTimeout(t *testing.T) {
	cases := []struct {
		name    string
		command string
		seconds int
		want    time.Duration
	}{
		{"普通命令用默认两分钟", "ls -la", 0, defaultShellTimeout},
		{"纯装依赖给五分钟", "pip install -q dingtalk-stream", 0, installShellTimeout},
		{"deno 装依赖走脚本档（要审批）", "deno cache --vendor main.ts", 0, defaultShellTimeout},
		{"模型显式声明优先", "sleep 1", 900, 900 * time.Second},
		{"超过上限按上限", "sleep 1", 99999, maxShellTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shellTimeout(c.command, c.seconds); got != c.want {
				t.Fatalf("shellTimeout(%q,%d)=%s，期望 %s", c.command, c.seconds, got, c.want)
			}
		})
	}
}

// 插件作用域：只挂 plugins/<slug>，命令的 cwd 就是它。
// 装依赖（npm postinstall 尤甚）是平台上最不该看见业务数据的一步。
func TestShellExecPluginScope(t *testing.T) {
	env := newShellEnv(t, sandbox.NewLocal())
	local, ok := env.Storage.(localPathStore)
	if !ok {
		t.Fatal("测试用的存储应支持 LocalPath")
	}
	dir, err := local.LocalPath("spaces/sp1/files/plugins/demo")
	if err != nil {
		t.Fatalf("解析插件目录失败: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}

	out, err := runShell(t, env, `{"command":"echo dep > dep.txt","plugin":"demo"}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "退出码 0") {
		t.Fatalf("命令应成功: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "dep.txt")); err != nil {
		t.Errorf("命令应落在插件目录里: %v", err)
	}
	workDir, err := local.LocalPath("spaces/sp1/files")
	if err != nil {
		t.Fatalf("解析工作区失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "dep.txt")); err == nil {
		t.Error("插件作用域下不应写到工作区根目录")
	}
}

// plugin 是模型写的，必须当成不可信输入：带 ../ 的 slug 能把挂载点指到空间里
// 任何一个目录，等于绕开「只挂插件目录」这条边界。
func TestShellPluginScopeRejectsUntrustedSlug(t *testing.T) {
	env := newShellEnv(t, sandbox.NewLocal())
	for _, slug := range []string{"../etc", "a/b", ".hidden", ""} {
		if slug == "" {
			continue // 空串是「不要插件作用域」，合法
		}
		if _, err := runShell(t, env, `{"command":"ls","plugin":"`+slug+`"}`); err == nil {
			t.Errorf("非法 slug %q 应被拒绝", slug)
		}
	}
	if _, err := runShell(t, env, `{"command":"ls","plugin":"missing"}`); err == nil {
		t.Fatal("插件目录不存在时应报错，而不是退回工作区")
	}
}
