package app

import (
	"testing"

	"github.com/workagent/auto-agent/internal/admin"
)

func TestEnvRefName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"default", ""},
		{"bocha", ""},
		{"${SEARCH_API_KEY}", "SEARCH_API_KEY"},
		{" ${SEARCH_API_KEY} ", "SEARCH_API_KEY"},
		{"${}", ""},
		{"${", ""},
		{"$SEARCH_API_KEY", ""},
		{"env:SEARCH_API_KEY", ""},
	}
	for _, c := range cases {
		if got := envRefName(c.in); got != c.want {
			t.Errorf("envRefName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestSearchRefSource(t *testing.T) {
	if got := searchRefSource(""); got != admin.SourceStore {
		t.Errorf("普通凭证名应指向 store，实际 %q", got)
	}
	if got := searchRefSource("SEARCH_API_KEY"); got != admin.SourceEnv {
		t.Errorf("环境变量引用应指向 env，实际 %q", got)
	}
	if got := searchRefLabel("bocha", ""); got != "web_search（bocha）" {
		t.Errorf("文案不对: %q", got)
	}
	if got := searchRefLabel("bocha", "SEARCH_API_KEY"); got != "web_search（环境变量 SEARCH_API_KEY）" {
		t.Errorf("环境变量文案不对: %q", got)
	}
}
