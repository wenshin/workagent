package auth

import (
	"net/url"
	"testing"
	"time"
)

func TestAuthorizeURL(t *testing.T) {
	c := NewDingTalkClient("ding_test_id", "secret", "http://127.0.0.1:8000/auth/dingtalk/callback")

	raw := c.AuthorizeURL("state-123")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("授权地址无法解析: %v", err)
	}
	if u.Scheme != "https" || u.Host != "login.dingtalk.com" || u.Path != "/oauth2/auth" {
		t.Fatalf("授权地址前缀不符: %s", raw)
	}

	q := u.Query()
	cases := map[string]string{
		"client_id":     "ding_test_id",
		"response_type": "code",
		"scope":         "openid",
		"state":         "state-123",
		"redirect_uri":  "http://127.0.0.1:8000/auth/dingtalk/callback",
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s = %q, 期望 %q", k, got, want)
		}
	}
}

func TestStateStoreConsumeOnce(t *testing.T) {
	s := newStateStore(time.Minute)

	state, err := s.Issue()
	if err != nil {
		t.Fatalf("生成 state 失败: %v", err)
	}
	if !s.Consume(state) {
		t.Fatal("首次校验应当通过")
	}
	if s.Consume(state) {
		t.Fatal("同一个 state 不允许重复使用")
	}
	if s.Consume("not-exist") {
		t.Fatal("不存在的 state 不应通过")
	}
}

func TestStateStoreExpiry(t *testing.T) {
	s := newStateStore(-time.Second)

	state, err := s.Issue()
	if err != nil {
		t.Fatalf("生成 state 失败: %v", err)
	}
	if s.Consume(state) {
		t.Fatal("过期 state 不应通过")
	}
}

func TestHashTokenStableAndNotPlaintext(t *testing.T) {
	token := "plain-token"
	h1 := hashToken(token)
	h2 := hashToken(token)

	if h1 != h2 {
		t.Fatal("同一 token 的哈希应稳定")
	}
	if h1 == token {
		t.Fatal("不能明文存储 token")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 十六进制长度应为 64，实际 %d", len(h1))
	}
}
