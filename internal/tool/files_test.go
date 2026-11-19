package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
)

func newTestEnv(t *testing.T) Env {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化本地存储失败: %v", err)
	}
	return Env{UserID: "u1", SpaceID: "sp1", SpacePerm: "manage", Storage: fs}
}

func callTool(t *testing.T, tl Tool, env Env, args string) string {
	t.Helper()
	out, err := tl.Invoke(context.Background(), json.RawMessage(args), env)
	if err != nil {
		t.Fatalf("%s 执行失败: %v", tl.Name, err)
	}
	return out
}

func TestWorkspaceFilePath(t *testing.T) {
	valid := []string{
		"a.md", "skills/空间管理/SKILL.md", "tools/x.json", "a/b.txt", "a/b.yaml",
		// 插件脚本：工具插件入口 index.ts、脚本型搜索源 main.ts
		"plugins/dingtalk_notify/plugin.json", "plugins/dingtalk_notify/index.ts",
		"plugins/x/main.js",
		// 用户自己的隐藏文件：没有常规后缀的配置同样按文本处理
		".env", ".gitignore", ".github/workflows/ci.yml", "config/.env.local",
	}
	for _, p := range valid {
		if _, err := workspaceFilePath(p); err != nil {
			t.Errorf("%q 应合法，却被拒绝: %v", p, err)
		}
	}

	invalid := []string{
		"", "../a.md", "/etc/passwd", "a.png", "a/b.pdf", "..", "a/../../b.md",
		// 平台保留的隐藏项对用户与 Agent 都不可访问
		".meta.json", "skills/.meta.json", ".install.json", "plugins/x/.install.json",
		".history/backup.md",
	}
	for _, p := range invalid {
		if _, err := workspaceFilePath(p); err == nil {
			t.Errorf("%q 应被拒绝", p)
		}
	}
}

func TestFileWriteReadRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	write, read := fileWrite(), fileRead()

	callTool(t, write, env, `{"path":"skills/演示/SKILL.md","content":"# 标题\n内容"}`)

	out := callTool(t, read, env, `{"path":"skills/演示/SKILL.md"}`)
	if !strings.Contains(out, "# 标题") || !strings.Contains(out, "内容") {
		t.Fatalf("读取内容不符: %s", out)
	}

	// 目录路径交给 file_read 时给出提示而不是报错
	hint := callTool(t, read, env, `{"path":"skills/演示/SKILL.md"}`)
	if !strings.Contains(hint, "共") {
		t.Fatalf("缺少文件元信息: %s", hint)
	}

	missing := callTool(t, read, env, `{"path":"不存在.md"}`)
	if !strings.Contains(missing, "文件不存在") {
		t.Fatalf("不存在文件应返回提示: %s", missing)
	}
}

func TestFileWriteModes(t *testing.T) {
	env := newTestEnv(t)
	write := fileWrite()

	callTool(t, write, env, `{"path":"a.md","content":"x","mode":"create"}`)

	if _, err := write.Invoke(context.Background(), json.RawMessage(`{"path":"a.md","content":"y","mode":"create"}`), env); err == nil {
		t.Fatal("create 已存在文件应失败")
	}

	callTool(t, write, env, `{"path":"a.md","content":"y","mode":"append"}`)

	out := callTool(t, fileRead(), env, `{"path":"a.md"}`)
	if !strings.Contains(out, "xy") {
		t.Fatalf("append 结果不符: %s", out)
	}

	if _, err := write.Invoke(context.Background(), json.RawMessage(`{"path":"a.md","content":"z","mode":"xxx"}`), env); err == nil {
		t.Fatal("未知 mode 应失败")
	}
}

func TestFileWriteRejectsOversize(t *testing.T) {
	env := newTestEnv(t)
	body, err := json.Marshal(map[string]string{
		"path":    "big.md",
		"content": strings.Repeat("a", maxWriteBytes+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWrite().Invoke(context.Background(), body, env); err == nil {
		t.Fatal("超过单文件上限应失败")
	}
}

func TestFileReadSupportsOffset(t *testing.T) {
	env := newTestEnv(t)
	callTool(t, fileWrite(), env, `{"path":"a.md","content":"0123456789"}`)

	out := callTool(t, fileRead(), env, `{"path":"a.md","offset":4,"limit":3}`)
	if !strings.Contains(out, "456") {
		t.Fatalf("分段读取结果不符: %s", out)
	}
	if !strings.Contains(out, "还有 3 字节未读") {
		t.Fatalf("应提示剩余字节: %s", out)
	}
}

func TestFileListHidesMetaAndReturnsPath(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 模拟平台写入的元数据，绕过工具校验
	for key, body := range map[string]string{
		"skills/demo/.meta.json": "{}",
		"skills/demo/SKILL.md":   "# demo",
		"skills/demo/.env":       "TOKEN=x",
	} {
		if _, err := env.Storage.Put(ctx, workspaceKey(env, key), strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}

	out := callTool(t, fileList(), env, `{"path":"skills/demo"}`)
	if strings.Contains(out, ".meta.json") {
		t.Fatalf("不应暴露平台保留的隐藏元数据: %s", out)
	}
	if !strings.Contains(out, "skills/demo/SKILL.md") {
		t.Fatalf("应返回可直接用于 file_read 的相对路径: %s", out)
	}
	// 用户自己的隐藏文件不是平台元数据，必须能看到
	if !strings.Contains(out, "skills/demo/.env") {
		t.Fatalf("用户隐藏文件应正常列出: %s", out)
	}
}

func TestFileToolsAllowUserHiddenFiles(t *testing.T) {
	env := newTestEnv(t)
	write, read := fileWrite(), fileRead()
	ctx := context.Background()

	callTool(t, write, env, `{"path":".env","content":"TOKEN=x"}`)
	if out := callTool(t, read, env, `{"path":".env"}`); !strings.Contains(out, "TOKEN=x") {
		t.Fatalf("隐藏文件应可读: %s", out)
	}

	// 平台保留项任何一条路径都写不进去
	for _, p := range []string{".meta.json", "skills/demo/.meta.json", "plugins/x/.install.json"} {
		body, err := json.Marshal(map[string]string{"path": p, "content": "{}"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := write.Invoke(ctx, body, env); err == nil {
			t.Fatalf("%q 是平台保留文件，应拒绝写入", p)
		}
	}
}

func TestFileToolsRequireSpace(t *testing.T) {
	env := newTestEnv(t)
	env.SpaceID = ""
	if _, err := fileList().Invoke(context.Background(), json.RawMessage(`{}`), env); err == nil {
		t.Fatal("缺少空间上下文时应失败")
	}
	if _, err := fileWrite().Invoke(context.Background(), json.RawMessage(`{"path":"a.md","content":"x"}`), env); err == nil {
		t.Fatal("缺少空间上下文时应失败")
	}
}

// 工具描述是模型选工具的直接依据，也是提示词精简后的规则承载体。
//
// 基础规则只写策略（默认不产文件、什么时候必须检索），各工具怎么用留在 description：
// 能力面按插件安装与智能体裁剪，规则跟着工具一起出现或消失，不会出现「模型看得见
// 12KB 分块的规则、却看不到 file_write 这个工具」。这些约束一旦从 description 里
// 消失就彻底没了——提示词那边已经不重复了。
func TestToolDescriptionsCarryUsageRules(t *testing.T) {
	cases := []struct {
		tool string
		desc string
		want []string
	}{
		{"file_write", fileWrite().Description, []string{"12KB", "file_read 读一遍", "根目录之外"}},
		{"file_read", fileRead().Description, []string{"根目录之外", "offset"}},
		{"shell_exec", shellExec().Description, []string{"沙箱", "stderr"}},
		{"wiki_write", wikiWrite().Description, []string{"if_version", "knowledge/", "未绑定智能体"}},
		{"secret_request", secretRequest().Description, []string{"state=configured", "state=missing"}},
	}
	for _, c := range cases {
		for _, want := range c.want {
			if !strings.Contains(c.desc, want) {
				t.Errorf("%s 的描述缺少 %q：%s", c.tool, want, c.desc)
			}
		}
	}
}
