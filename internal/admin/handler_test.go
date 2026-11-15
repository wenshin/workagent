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
