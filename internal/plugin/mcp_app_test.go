package plugin

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/store"
)

type appStore struct {
	store.Store
	owners map[string]store.MCPApp
	users  map[string]store.User
}

func (s *appStore) GetMCPApp(_ context.Context, app string) (store.MCPApp, error) {
	if rec, ok := s.owners[app]; ok {
		return rec, nil
	}
	return store.MCPApp{}, store.ErrNotFound
}

func (s *appStore) ClaimMCPApp(_ context.Context, app, userID, _ string) (store.MCPApp, bool, error) {
	if rec, ok := s.owners[app]; ok {
		return rec, false, nil
	}
	rec := store.MCPApp{App: app, OwnerUserID: userID}
	s.owners[app] = rec
	return rec, true, nil
}

func (s *appStore) GetUser(_ context.Context, id string) (store.User, error) {
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

// appSecrets 只实现 confirmAppKey 用到的 Enabled / Get。
type appSecrets struct {
	secret.Store
	values map[string]string
}

func (s *appSecrets) Enabled() bool { return true }

func (s *appSecrets) Get(_ context.Context, scope, name string) (string, error) {
	if v, ok := s.values[scope+"/"+name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func newAppManager(owners map[string]store.MCPApp, users map[string]store.User, keys map[string]string) *Manager {
	return &Manager{o: Options{
		Store:   &appStore{owners: owners, users: users},
		Secrets: &appSecrets{values: keys},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

// 发布 jwt 模式的 MCP 插件前必须确认应用名归属：
// 自己的直接放行，别人的必须出示密钥，否则发布响应等于把别人的密钥交出去。
func TestConfirmAppKey(t *testing.T) {
	ctx := context.Background()

	t.Run("自己的应用不用再输", func(t *testing.T) {
		m := newAppManager(map[string]store.MCPApp{
			"crm_mcp": {App: "crm_mcp", OwnerUserID: "u1"},
		}, nil, map[string]string{"mcp_jwt/crm_mcp": "k1"})
		if err := m.confirmAppKey(ctx, "u1", "s1", "crm_mcp", ""); err != nil {
			t.Fatalf("应当放行: %v", err)
		}
	})

	t.Run("别人的应用：不出示密钥拒绝", func(t *testing.T) {
		m := newAppManager(map[string]store.MCPApp{
			"crm_mcp": {App: "crm_mcp", OwnerUserID: "u2"},
		}, map[string]store.User{"u2": {ID: "u2", Name: "张三"}},
			map[string]string{"mcp_jwt/crm_mcp": "k1"})
		err := m.confirmAppKey(ctx, "u1", "s1", "crm_mcp", "")
		if err == nil {
			t.Fatal("没有密钥应当拒绝")
		}
		if !strings.Contains(err.Error(), "张三") {
			t.Fatalf("错误里应当说明创建者，实际: %v", err)
		}
	})

	t.Run("别人的应用：密钥正确放行且不改归属", func(t *testing.T) {
		owners := map[string]store.MCPApp{"crm_mcp": {App: "crm_mcp", OwnerUserID: "u2"}}
		m := newAppManager(owners, nil, map[string]string{"mcp_jwt/crm_mcp": "k1"})
		if err := m.confirmAppKey(ctx, "u1", "s1", "crm_mcp", " k1 "); err != nil {
			t.Fatalf("密钥正确应当放行: %v", err)
		}
		if owners["crm_mcp"].OwnerUserID != "u2" {
			t.Fatal("归属不应被发布者改写")
		}
	})

	t.Run("老数据：出示密钥后补归属", func(t *testing.T) {
		owners := map[string]store.MCPApp{}
		m := newAppManager(owners, nil, map[string]string{"mcp_jwt/legacy": "old"})
		if err := m.confirmAppKey(ctx, "u1", "s1", "legacy", "bad"); err == nil {
			t.Fatal("密钥不对应当拒绝")
		}
		if err := m.confirmAppKey(ctx, "u1", "s1", "legacy", "old"); err != nil {
			t.Fatalf("密钥正确应当放行: %v", err)
		}
		if owners["legacy"].OwnerUserID != "u1" {
			t.Fatal("应当补上归属")
		}
	})

	t.Run("全新应用名直接放行", func(t *testing.T) {
		m := newAppManager(map[string]store.MCPApp{}, nil, map[string]string{})
		if err := m.confirmAppKey(ctx, "u1", "s1", "brand_new", ""); err != nil {
			t.Fatalf("首次创建不需要确认: %v", err)
		}
	})
}
