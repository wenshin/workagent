package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/store"
)

// fakeAppStore 只实现归属查询；其余方法走嵌入接口，测试里不会调到。
type fakeAppStore struct {
	store.Store
	owners map[string]store.MCPApp
	users  map[string]store.User
}

func (f *fakeAppStore) GetMCPApp(_ context.Context, app string) (store.MCPApp, error) {
	if rec, ok := f.owners[app]; ok {
		return rec, nil
	}
	return store.MCPApp{}, store.ErrNotFound
}

func (f *fakeAppStore) GetUser(_ context.Context, id string) (store.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

// fakeMCPSecrets 只实现工具用到的 Enabled / Get。
type fakeMCPSecrets struct {
	secret.Store
	enabled bool
	values  map[string]string
}

func (f *fakeMCPSecrets) Enabled() bool { return f.enabled }

func (f *fakeMCPSecrets) Get(_ context.Context, scope, name string) (string, error) {
	if v, ok := f.values[scope+"/"+name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func runMCPApp(t *testing.T, env Env, args string) mcpAppResult {
	t.Helper()
	tool, ok := Find(builtinTools(), "mcp_app_key")
	if !ok {
		t.Fatal("mcp_app_key 没有注册到内置工具里")
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(args), env)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var res mcpAppResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	// 密钥永远不该出现在回执里：它是给浏览器卡片的，进上下文就是泄漏
	if res.MCPApp.State != "" && json.Valid([]byte(out)) {
		var raw map[string]any
		_ = json.Unmarshal([]byte(out), &raw)
		if _, ok := raw["key"]; ok {
			t.Fatal("回执里出现了 key 字段")
		}
	}
	return res
}

func TestMCPAppKeyTool(t *testing.T) {
	secrets := &fakeMCPSecrets{enabled: true, values: map[string]string{}}
	base := Env{UserID: "u1", SpaceID: "s1", Secrets: secrets}

	t.Run("未填应用名时只摆卡片", func(t *testing.T) {
		env := base
		env.Store = &fakeAppStore{}
		if got := runMCPApp(t, env, `{}`); got.State != "needs_app" {
			t.Fatalf("state = %q, 期望 needs_app", got.State)
		}
	})

	t.Run("应用名不存在", func(t *testing.T) {
		env := base
		env.Store = &fakeAppStore{}
		if got := runMCPApp(t, env, `{"app":"crm_mcp"}`); got.State != "new" {
			t.Fatalf("state = %q, 期望 new", got.State)
		}
	})

	t.Run("自己创建的可以取回", func(t *testing.T) {
		env := base
		env.Store = &fakeAppStore{owners: map[string]store.MCPApp{
			"crm_mcp": {App: "crm_mcp", OwnerUserID: "u1"},
		}}
		if got := runMCPApp(t, env, `{"app":"crm_mcp"}`); got.State != "owned" {
			t.Fatalf("state = %q, 期望 owned", got.State)
		}
	})

	t.Run("别人创建的只回姓名", func(t *testing.T) {
		env := base
		env.Store = &fakeAppStore{
			owners: map[string]store.MCPApp{"crm_mcp": {App: "crm_mcp", OwnerUserID: "u2"}},
			users:  map[string]store.User{"u2": {ID: "u2", Name: "张三"}},
		}
		got := runMCPApp(t, env, `{"app":"crm_mcp"}`)
		if got.State != "taken" || got.MCPApp.OwnerName != "张三" {
			t.Fatalf("state/owner = %q/%q, 期望 taken/张三", got.State, got.MCPApp.OwnerName)
		}
	})

	t.Run("有密钥但没有归属记录：找管理员，不认领", func(t *testing.T) {
		env := base
		secrets.values["mcp_jwt/crm_mcp"] = "old-key"
		defer delete(secrets.values, "mcp_jwt/crm_mcp")
		env.Store = &fakeAppStore{}
		if got := runMCPApp(t, env, `{"app":"crm_mcp"}`); got.State != "unowned" {
			t.Fatalf("state = %q, 期望 unowned", got.State)
		}
	})

	t.Run("凭证存储未启用", func(t *testing.T) {
		env := base
		env.Secrets = &fakeMCPSecrets{enabled: false}
		env.Store = &fakeAppStore{}
		if got := runMCPApp(t, env, `{"app":"crm_mcp"}`); got.State != "disabled" {
			t.Fatalf("state = %q, 期望 disabled", got.State)
		}
	})

	t.Run("应用名非法直接报错", func(t *testing.T) {
		env := base
		env.Store = &fakeAppStore{}
		tt, _ := Find(builtinTools(), "mcp_app_key")
		if _, err := tt.Invoke(context.Background(), json.RawMessage(`{"app":"crm mcp"}`), env); err == nil {
			t.Fatal("带空格的应用名应当报错")
		}
	})
}
