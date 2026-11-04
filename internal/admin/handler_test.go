package admin_test

import (
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
		[]admin.SecretRef{{Scope: "search", Name: "bocha", Label: "web_search"}},
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
