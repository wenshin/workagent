package plugin

import (
	"testing"

	"github.com/workagent/auto-agent/internal/search"
)

// 凭证声明有三个入口（搜索源 / secret.use / MCP 预置凭证），少算一处，
// 插件卡片就会在「其实用不了」的时候什么都不说。
func TestManifestSecretRefs(t *testing.T) {
	m := Manifest{
		Source: &SourceSpec{Secret: "bocha"},
		Auth:   &Auth{Mode: AuthModeStatic, Secret: "crm_token"},
		Capabilities: []Capability{
			{Method: MethodSecret, Name: "erp_key"},
			{Method: MethodSecret, Name: "erp_key"}, // 重复声明只算一次
			{Method: MethodHTTP, Hosts: []string{"api.example.com"}},
		},
	}
	got := m.SecretRefs()
	want := []SecretRef{
		{Scope: SecretScope, Name: "crm_token"},
		{Scope: SecretScope, Name: "erp_key"},
		{Scope: search.SecretScope, Name: "bocha"},
	}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 条凭证引用，实际 %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条不对：期望 %+v，实际 %+v", i, want[i], got[i])
		}
	}
}

// auth.mode=jwt 的密钥由平台现签（mcp_jwt scope），不是用户要录入的凭证。
func TestManifestSecretRefsSkipsJWT(t *testing.T) {
	m := Manifest{Auth: &Auth{Mode: AuthModeJWT, App: "crm"}}
	if refs := m.SecretRefs(); len(refs) != 0 {
		t.Fatalf("jwt 模式不应产生凭证引用，实际 %+v", refs)
	}
}
