package app

import (
	"context"
	"testing"

	"github.com/workagent/auto-agent/internal/admin"
	"github.com/workagent/auto-agent/internal/secret"
)

func TestParseSecretRef(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantExp  bool
	}{
		{"default", "default", false},
		{"bocha", "bocha", false},
		{"${SEARCH_API_KEY}", "SEARCH_API_KEY", true},
		{" ${SEARCH_API_KEY} ", "SEARCH_API_KEY", true},
		{"${}", "${}", false},
		{"${", "${", false},
		{"$SEARCH_API_KEY", "$SEARCH_API_KEY", false},
		{"env:SEARCH_API_KEY", "env:SEARCH_API_KEY", false},
	}
	for _, c := range cases {
		gotName, gotExp := parseSecretRef(c.in)
		if gotName != c.wantName || gotExp != c.wantExp {
			t.Errorf("parseSecretRef(%q) = (%q, %v)，期望 (%q, %v)",
				c.in, gotName, gotExp, c.wantName, c.wantExp)
		}
	}
}

func TestSearchRefLabel(t *testing.T) {
	if got := searchRefLabel("bocha", "default", false); got != "web_search（bocha）" {
		t.Errorf("普通引用文案不对: %q", got)
	}
	if got := searchRefLabel("bocha", "SEARCH_API_KEY", true); got != "web_search（${SEARCH_API_KEY}）" {
		t.Errorf("${} 引用文案不对: %q", got)
	}
}

// ${NAME} 既能命中界面保存的凭证，也能退回环境变量——来源由是否有同名条目决定。
func TestSearchRefSource(t *testing.T) {
	store, err := secret.NewFileStore(t.TempDir(), "test-kek-0123456789")
	if err != nil {
		t.Fatalf("构造凭证存储失败: %v", err)
	}
	if err := store.Put(context.Background(), "search", "SEARCH_API_KEY", "v"); err != nil {
		t.Fatalf("写入凭证失败: %v", err)
	}
	if got := searchRefSource(store, "search", "SEARCH_API_KEY", true); got != admin.SourceStore {
		t.Errorf("已保存的凭证应判定为 store，实际 %q", got)
	}
	if got := searchRefSource(store, "search", "NOT_SAVED", true); got != admin.SourceEnv {
		t.Errorf("没有同名凭证时应退回 env，实际 %q", got)
	}
	if got := searchRefSource(store, "search", "default", false); got != admin.SourceStore {
		t.Errorf("裸名字始终属于 store，实际 %q", got)
	}
}
