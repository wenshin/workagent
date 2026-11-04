package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"testing"
)

// stubCreateSkill 模拟 space.Service.CreateSkill：建目录、记录入参。
func stubCreateSkill(t *testing.T, env *Env, calls *[]string) {
	t.Helper()
	env.CreateSkill = func(ctx context.Context, name, description string) (string, error) {
		*calls = append(*calls, name+"|"+description)
		if err := env.Storage.EnsureDir(ctx, path.Join(env.Storage.SpaceKey(env.SpaceID), workspaceFilesDir, name)); err != nil {
			return "", err
		}
		return name, nil
	}
}

func readWorkspaceFile(t *testing.T, env Env, rel string) string {
	t.Helper()
	f, err := env.Storage.Get(context.Background(), path.Join(env.Storage.SpaceKey(env.SpaceID), workspaceFilesDir, rel))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", rel, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestSkillCreateWritesSkeleton(t *testing.T) {
	env := newTestEnv(t)
	var calls []string
	stubCreateSkill(t, &env, &calls)

	out := callTool(t, skillCreate(), env, `{"name":"周报生成","description":"每周汇总进展并生成周报"}`)
	if !strings.Contains(out, "周报生成") {
		t.Fatalf("返回缺少技能名: %s", out)
	}
	if len(calls) != 1 || calls[0] != "周报生成|每周汇总进展并生成周报" {
		t.Fatalf("CreateSkill 入参不符: %v", calls)
	}

	doc := readWorkspaceFile(t, env, "周报生成/SKILL.md")
	for _, want := range []string{"---\nname:", "description:", "# 周报生成", "每周汇总进展并生成周报", "## 步骤"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("骨架缺少 %q: %s", want, doc)
		}
	}
}

func TestSkillCreateUsesProvidedContent(t *testing.T) {
	env := newTestEnv(t)
	var calls []string
	stubCreateSkill(t, &env, &calls)

	callTool(t, skillCreate(), env, `{"name":"日报","description":"每日进展","content":"# 日报\n\n固定模板"}`)

	doc := readWorkspaceFile(t, env, "日报/SKILL.md")
	// 缺少 frontmatter 时按规范补齐，正文保持不变
	if !strings.HasPrefix(doc, "---\nname: \"日报\"\n") {
		t.Fatalf("缺少规范 frontmatter: %q", doc)
	}
	if !strings.HasSuffix(doc, "# 日报\n\n固定模板") {
		t.Fatalf("正文应保留: %q", doc)
	}
}

func TestSkillCreateKeepsExistingFrontMatter(t *testing.T) {
	env := newTestEnv(t)
	var calls []string
	stubCreateSkill(t, &env, &calls)

	content := "---\nname: 日报\ndescription: 自定义\n---\n\n# 日报"
	callTool(t, skillCreate(), env, `{"name":"日报","description":"每日进展","content":"---\nname: 日报\ndescription: 自定义\n---\n\n# 日报"}`)

	doc := readWorkspaceFile(t, env, "日报/SKILL.md")
	if doc != content {
		t.Fatalf("已带 frontmatter 的内容不应被改写: %q", doc)
	}
}

func TestSkillCreateValidation(t *testing.T) {
	env := newTestEnv(t)
	var calls []string
	stubCreateSkill(t, &env, &calls)

	cases := []string{
		`{"name":"","description":"x"}`,
		`{"name":"缺少描述"}`,
		`{"name":"   ","description":"x"}`,
	}
	for _, args := range cases {
		if _, err := skillCreate().Invoke(context.Background(), json.RawMessage(args), env); err == nil {
			t.Fatalf("%s 应被拒绝", args)
		}
	}

	big := strings.Repeat("a", maxSkillDocBytes+1)
	body, err := json.Marshal(map[string]string{"name": "大技能", "description": "x", "content": big})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := skillCreate().Invoke(context.Background(), body, env); err == nil {
		t.Fatal("超过 SKILL.md 上限应被拒绝")
	}
	if len(calls) != 0 {
		t.Fatalf("校验失败不应调用 CreateSkill: %v", calls)
	}
}

func TestSkillCreateSurfacesDuplicate(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSkill = func(context.Context, string, string) (string, error) {
		return "", errors.New("同名目录已存在：周报生成")
	}
	_, err := skillCreate().Invoke(context.Background(),
		json.RawMessage(`{"name":"周报生成","description":"x"}`), env)
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("应透传重名错误: %v", err)
	}
}

func TestSkillCreateRequiresInjection(t *testing.T) {
	env := newTestEnv(t)
	if _, err := skillCreate().Invoke(context.Background(),
		json.RawMessage(`{"name":"x","description":"y"}`), env); err == nil {
		t.Fatal("未注入 CreateSkill 时应失败")
	}
}
