package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/admin"
	"github.com/workagent/auto-agent/internal/plugin"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
)

// 同一 scope 下必须能存多条：换密钥时要能先存新的再删旧的，
// 只允许一条会让轮换变成「先删后加」的空窗。
func TestSecretsAllowMultipleItems(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs, err := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets"), "0123456789abcdef")
	if err != nil {
		t.Fatalf("构造凭证存储失败: %v", err)
	}
	spaces := space.NewService(nil, nil, log, []string{"E001"})
	h := admin.NewHandler(spaces, fs,
		[]admin.SecretRef{{Scope: "search", Name: "bocha", Label: "web_search"}}, nil,
		func(*http.Request) (store.User, error) {
			return store.User{ID: "u1", JobNumber: "E001"}, nil
		}, log)
	mux := http.NewServeMux()
	h.Register(mux)

	put := func(path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s 返回 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	put("/admin/secrets/search/bocha", `{"value":"sk-old"}`)
	put("/admin/secrets/search/bocha_next", `{"value":"sk-new"}`)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/settings 返回 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SecretsEnabled bool              `json:"secrets_enabled"`
		Refs           []admin.SecretRef `json:"refs"`
		Items          []admin.SecretItem
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("期望 2 条凭证，实际 %d 条: %+v", len(out.Items), out.Items)
	}

	inUse := map[string]bool{}
	for _, it := range out.Items {
		if it.Fingerprint == "" {
			t.Errorf("%s 缺少指纹", it.Name)
		}
		inUse[it.Name] = it.InUse
	}
	if !inUse["bocha"] {
		t.Error("bocha 应标记为使用中")
	}
	if inUse["bocha_next"] {
		t.Error("bocha_next 未被引用，不应标记为使用中")
	}
}

// refs 为空是正常状态（引用关系由插件清单在运行期声明），不能因为没引用就
// 不列出已经保存的凭证——那会让管理员以为密钥丢了。
func TestSettingsListsStoredSecretsWithoutRefs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs, err := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets"), "0123456789abcdef")
	if err != nil {
		t.Fatalf("构造凭证存储失败: %v", err)
	}
	if err := fs.Put(context.Background(), "search", "bocha", "sk-1"); err != nil {
		t.Fatalf("写入凭证失败: %v", err)
	}
	if err := fs.Put(context.Background(), "plugin", "erp_token", "tk-1"); err != nil {
		t.Fatalf("写入凭证失败: %v", err)
	}

	spaces := space.NewService(nil, nil, log, []string{"E001"})
	usage := func(context.Context) map[string]bool {
		// 搜索源插件声明的凭证：装了就「使用中」，与 refs 无关
		return map[string]bool{"search/bocha": true}
	}
	h := admin.NewHandler(spaces, fs, nil, usage, func(*http.Request) (store.User, error) {
		return store.User{ID: "u1", JobNumber: "E001"}, nil
	}, log)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/settings 返回 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []admin.SecretItem
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	inUse := map[string]bool{}
	for _, it := range out.Items {
		inUse[it.Scope+"/"+it.Name] = it.InUse
	}
	if len(out.Items) != 2 {
		t.Fatalf("期望列出 2 条凭证（含未被 refs 引用的），实际 %d 条: %+v", len(out.Items), out.Items)
	}
	if !inUse["search/bocha"] {
		t.Error("search/bocha 应在使用中")
	}
	if _, ok := inUse["plugin/erp_token"]; !ok {
		t.Error("plugin scope 的凭证也要列出来")
	}
}

// fakeMCPApps 是应用密钥视图的桩：admin 只做鉴权与响应，取数在插件层，
// 所以这里验证的是「谁能看、响应长什么样」，不是密钥逻辑本身。
type fakeMCPApps struct {
	items []plugin.MCPAppInfo
	key   string
}

func (f *fakeMCPApps) ListMCPApps(context.Context) ([]plugin.MCPAppInfo, error) {
	return f.items, nil
}

func (f *fakeMCPApps) RevealMCPAppKey(_ context.Context, _, app string) (plugin.MCPAppInfo, string, error) {
	for _, it := range f.items {
		if it.App == app && it.HasKey {
			return it, f.key, nil
		}
	}
	return plugin.MCPAppInfo{}, "", plugin.ErrMCPAppKeyMissing
}

func TestMCPAppsRequireAdminAndRevealOnDemand(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs, err := secret.NewFileStore(filepath.Join(t.TempDir(), "secrets"), "0123456789abcdef")
	if err != nil {
		t.Fatalf("构造凭证存储失败: %v", err)
	}
	// 密钥本身存在凭证存储里；列表接口不该把它带出来
	if err := fs.Put(context.Background(), "mcp_jwt", "crm_mcp", "k1"); err != nil {
		t.Fatalf("写入凭证失败: %v", err)
	}

	spaces := space.NewService(nil, nil, log, []string{"E001"})
	view := &fakeMCPApps{
		items: []plugin.MCPAppInfo{{App: "crm_mcp", OwnerName: "张三", HasKey: true, Fingerprint: "aaaa1111"}},
		key:   "k1",
	}

	newHandler := func(user store.User) *http.ServeMux {
		h := admin.NewHandler(spaces, fs, nil, nil, func(*http.Request) (store.User, error) {
			return user, nil
		}, log)
		h.SetMCPApps(view)
		mux := http.NewServeMux()
		h.Register(mux)
		return mux
	}

	adminMux := newHandler(store.User{ID: "u1", JobNumber: "E001"})
	rec := httptest.NewRecorder()
	adminMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/mcp-apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/mcp-apps 返回 %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		SecretsEnabled bool                `json:"secrets_enabled"`
		Items          []plugin.MCPAppInfo `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !list.SecretsEnabled || len(list.Items) != 1 || list.Items[0].App != "crm_mcp" {
		t.Fatalf("列表响应不对: %+v", list)
	}
	// 列表只给指纹：明文出现一次就会进浏览器缓存与 HAR
	if strings.Contains(rec.Body.String(), "k1") {
		t.Fatalf("列表响应不应包含明文密钥: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	adminMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/mcp-apps/crm_mcp/key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("查看密钥返回 %d: %s", rec.Code, rec.Body.String())
	}
	var reveal struct {
		App string `json:"app"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reveal); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if reveal.Key != "k1" || reveal.App != "crm_mcp" {
		t.Fatalf("查看密钥响应不对: %+v", reveal)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("密钥响应必须 no-store")
	}

	rec = httptest.NewRecorder()
	adminMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/mcp-apps/ghost/key", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺密钥应当 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 非系统管理员（= 非系统空间成员）一律 403
	otherMux := newHandler(store.User{ID: "u2", JobNumber: "E999"})
	for _, path := range []string{"/admin/mcp-apps", "/admin/mcp-apps/crm_mcp/key"} {
		rec = httptest.NewRecorder()
		otherMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s 非管理员应当 403，实际 %d", path, rec.Code)
		}
	}
}
