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
	}
	for _, p := range valid {
		if _, err := workspaceFilePath(p); err != nil {
			t.Errorf("%q 应合法，却被拒绝: %v", p, err)
		}
	}

	invalid := []string{
		"", "../a.md", "/etc/passwd", ".meta.json", "skills/.meta.json",
		"a.png", "a/b.pdf", "..", "a/../../b.md",
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
	} {
		if _, err := env.Storage.Put(ctx, workspaceKey(env, key), strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}

	out := callTool(t, fileList(), env, `{"path":"skills/demo"}`)
	if strings.Contains(out, ".meta.json") {
		t.Fatalf("不应暴露隐藏元数据: %s", out)
	}
	if !strings.Contains(out, "skills/demo/SKILL.md") {
		t.Fatalf("应返回可直接用于 file_read 的相对路径: %s", out)
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
