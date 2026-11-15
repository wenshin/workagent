package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/secret"
)

// fakeSecrets 只实现 secret_request 需要的两个方法；Put/Get/Delete 留空，
// 因为工具链路上根本不该出现写值或取值。
type fakeSecrets struct {
	enabled bool
	items   map[string][]secret.Item
}

func (f fakeSecrets) Enabled() bool { return f.enabled }

func (f fakeSecrets) Put(ctx context.Context, scope, name, value string) error { return nil }

func (f fakeSecrets) Get(ctx context.Context, scope, name string) (string, error) {
	return "", secret.ErrNotFound
}

func (f fakeSecrets) Status(ctx context.Context, scope string) ([]secret.Item, error) {
	return f.items[scope], nil
}

func (f fakeSecrets) SecretNames(ctx context.Context, scope string) ([]string, error) {
	out := make([]string, 0, len(f.items[scope]))
	for _, it := range f.items[scope] {
		out = append(out, it.Name)
	}
	return out, nil
}

func (f fakeSecrets) Delete(ctx context.Context, scope, name string) error { return nil }

func callSecretRequest(t *testing.T, env Env, args string) secretRequestResult {
	t.Helper()
	out, err := secretRequest().Invoke(context.Background(), json.RawMessage(args), env)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var res secretRequestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("回执不是 JSON: %v (%s)", err, out)
	}
	return res
}

func TestSecretRequestReportsConfiguredWithFingerprint(t *testing.T) {
	env := Env{
		CanWriteSecrets: true,
		Secrets: fakeSecrets{enabled: true, items: map[string][]secret.Item{
			"search": {{Name: "bocha", Fingerprint: "a1b2c3d4"}},
		}},
	}
	res := callSecretRequest(t, env, `{"scope":"search","name":"bocha","label":"博查搜索 API Key"}`)

	if !res.Request.Configured || res.Request.Fingerprint != "a1b2c3d4" {
		t.Fatalf("没读到已配置状态: %+v", res.Request)
	}
	if res.Request.Available != true || res.Request.Label != "博查搜索 API Key" {
		t.Fatalf("卡片描述不对: %+v", res.Request)
	}
	// 模型容易把上游 401/403 归因为「缺凭证」，回执要直接给出结论与纠正
	if res.Request.State != "configured" {
		t.Fatalf("已配置时 state 应为 configured: %q", res.Request.State)
	}
	for _, want := range []string{"用户不需要做任何操作", "不要提示用户录入", "401/403"} {
		if !strings.Contains(res.Next, want) {
			t.Fatalf("缺少「不必再问」的明确指令 %q: %s", want, res.Next)
		}
	}
}

func TestSecretRequestAsksUserToFillWhenAdmin(t *testing.T) {
	env := Env{CanWriteSecrets: true, Secrets: fakeSecrets{enabled: true}}
	res := callSecretRequest(t, env, `{"scope":"search","name":"bocha"}`)

	if res.Request.Configured {
		t.Fatal("没配过却报告已配置")
	}
	if !res.Request.CanWrite {
		t.Fatal("管理员应能自己录")
	}
	if res.Request.State != "missing" {
		t.Fatalf("未配置时 state 应为 missing: %q", res.Request.State)
	}
	if !strings.Contains(res.Next, "录入卡片") {
		t.Fatalf("应说明卡片已展示: %s", res.Next)
	}
}

func TestSecretRequestTellsNonAdminToAskAdmin(t *testing.T) {
	env := Env{CanWriteSecrets: false, Secrets: fakeSecrets{enabled: true}}
	res := callSecretRequest(t, env, `{"scope":"search","name":"bocha"}`)

	if res.Request.CanWrite {
		t.Fatal("普通用户不该拿到输入框")
	}
	for _, want := range []string{"不是系统管理员", "scope=search", "bocha"} {
		if !strings.Contains(res.Next, want) {
			t.Fatalf("缺少转给管理员的指引 %q: %s", want, res.Next)
		}
	}
}

func TestSecretRequestWhenStorageDisabled(t *testing.T) {
	// 存储没启用时不能让用户白填：填了也存不住，等于变相索要明文
	res := callSecretRequest(t, Env{CanWriteSecrets: true}, `{"scope":"search","name":"bocha"}`)

	if res.Request.Available {
		t.Fatal("存储未启用却报告可用")
	}
	if !strings.Contains(res.Next, "SECRET_KEK") || !strings.Contains(res.Next, "不要改用明文") {
		t.Fatalf("应说明部署侧原因并阻止走明文: %s", res.Next)
	}
}

func TestSecretRequestRejectsIllegalName(t *testing.T) {
	env := Env{CanWriteSecrets: true, Secrets: fakeSecrets{enabled: true}}
	if _, err := secretRequest().Invoke(context.Background(),
		json.RawMessage(`{"scope":"search","name":"博查 key"}`), env); err == nil {
		t.Fatal("非法凭证名应报错")
	}
}

// 工具契约里不能出现接收明文的口子：schema 只要名字与用途。
func TestSecretRequestSchemaHasNoValueField(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(secretRequest().Parameters, &schema); err != nil {
		t.Fatalf("参数 schema 解析失败: %v", err)
	}
	for name := range schema.Properties {
		switch name {
		case "scope", "name", "label", "hint":
		default:
			t.Fatalf("schema 里出现了不该有的字段: %s", name)
		}
	}
	for _, req := range schema.Required {
		if req != "scope" && req != "name" {
			t.Fatalf("必填项应只有 scope 与 name，多了 %s", req)
		}
	}
}
