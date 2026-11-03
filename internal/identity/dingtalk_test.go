package identity

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/config"
)

func TestAuthorizeURL(t *testing.T) {
	p := &DingTalk{
		ClientID:     "ding_test_id",
		ClientSecret: "secret",
		RedirectURI:  "http://127.0.0.1:8000/auth/callback",
		AuthBase:     dingtalkAuthBase,
	}

	raw, err := p.AuthorizeURL("state-123")
	if err != nil {
		t.Fatalf("生成授权地址失败: %v", err)
	}
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
		"redirect_uri":  "http://127.0.0.1:8000/auth/callback",
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s = %q, 期望 %q", k, got, want)
		}
	}
}

func TestAuthorizeURLNotConfigured(t *testing.T) {
	p := &DingTalk{AuthBase: dingtalkAuthBase}
	if _, err := p.AuthorizeURL("state"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("缺少凭证应返回 ErrNotConfigured，实际 %v", err)
	}
}

// newFakeDingTalk 启动一个假钉钉服务，覆盖个人资料、企业令牌、通讯录与企业名称接口。
// contactEmail 为空表示模拟敏感字段未授权：v2/user/get 返回中不含 email 字段。
func newFakeDingTalk(t *testing.T, contactStatus, orgNameStatus int, contactEmail string) (*DingTalk, *bool) {
	t.Helper()

	corpTokenCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.0/contact/users/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]string{
			"nick": "张三", "avatarUrl": "https://a", "mobile": "13800000000",
			"openId": "open-1", "unionId": "union-1", "email": "",
		})
	})
	mux.HandleFunc("GET /v1.0/contact/organizations/authInfos", func(w http.ResponseWriter, r *http.Request) {
		if orgNameStatus != 0 {
			w.WriteHeader(orgNameStatus)
			writeJSON(t, w, map[string]string{"code": "Forbidden.AccessDenied"})
			return
		}
		writeJSON(t, w, map[string]string{"orgName": "测试公司", "licenseOrgName": "测试公司"})
	})
	mux.HandleFunc("POST /v1.0/oauth2/accessToken", func(w http.ResponseWriter, r *http.Request) {
		corpTokenCalled = true
		writeJSON(t, w, map[string]any{"accessToken": "corp-token", "expireIn": 7200})
	})
	mux.HandleFunc("POST /topapi/user/getbyunionid", func(w http.ResponseWriter, r *http.Request) {
		if contactStatus != 0 {
			// 与钉钉真实返回一致：errcode=88 + sub_code=60011（缺 qyapi_get_member）
			writeJSON(t, w, map[string]any{
				"errcode": 88, "errmsg": "ding talk error",
				"sub_code": "60011", "sub_msg": "应用尚未开通所需的权限：[qyapi_get_member]",
			})
			return
		}
		writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "result": map[string]string{"userid": "u-1"}})
	})
	mux.HandleFunc("POST /topapi/v2/user/get", func(w http.ResponseWriter, r *http.Request) {
		result := map[string]any{
			"userid": "u-1", "name": "张三（通讯录）",
			"job_number": "A001", "title": "工程师", "dept_id_list": []int{1, 2},
		}
		if contactEmail != "" {
			result["email"] = contactEmail
		}
		writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "result": result})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &DingTalk{
		ClientID:     "ding_test",
		ClientSecret: "secret",
		CorpID:       "ding-corp-test",
		RedirectURI:  "http://127.0.0.1:8000/auth/callback",
		APIBase:      srv.URL,
		OAPIBase:     srv.URL,
		AuthBase:     dingtalkAuthBase,
		HTTP:         srv.Client(),
		Log:          slog.Default(),
	}, &corpTokenCalled
}

func TestProfileEnrichesJobNumberAndEmail(t *testing.T) {
	p, corpTokenCalled := newFakeDingTalk(t, 0, 0, "zhangsan@corp.com")

	got, err := p.Profile(context.Background(), Credential{AccessToken: "user-token"})
	if err != nil {
		t.Fatalf("拉取资料失败: %v", err)
	}
	if !*corpTokenCalled {
		t.Fatal("应当请求企业 accessToken 以补全通讯录字段")
	}
	if got.ProviderUID != "union-1" || got.EmployeeID != "u-1" {
		t.Fatalf("身份标识不符: %+v", got)
	}
	if got.Email != "zhangsan@corp.com" {
		t.Errorf("邮箱应以通讯录为准，实际 %q", got.Email)
	}
	if got.JobNumber != "A001" {
		t.Errorf("工号未取到，实际 %q", got.JobNumber)
	}
	if got.Title != "工程师" || len(got.DeptIDs) != 2 {
		t.Errorf("职务/部门未取到: %+v", got)
	}
	if got.Name != "张三（通讯录）" {
		t.Errorf("姓名字段应被通讯录覆盖，实际 %q", got.Name)
	}
	if got.CorpName != "测试公司" {
		t.Errorf("企业名称未取到，实际 %q", got.CorpName)
	}
	if got.ContactError != "" {
		t.Errorf("同步成功时不应有告警，实际 %q", got.ContactError)
	}
	if len(got.Raw) == 0 {
		t.Error("应保留通讯录原始返回，便于排查")
	}
}

func TestProfileDegradesWithoutContactPermission(t *testing.T) {
	p, _ := newFakeDingTalk(t, 60011, 0, "")

	got, err := p.Profile(context.Background(), Credential{AccessToken: "user-token"})
	if err != nil {
		t.Fatalf("通讯录失败不应阻断登录: %v", err)
	}
	if got.JobNumber != "" {
		t.Errorf("无通讯录权限时工号应为空，实际 %q", got.JobNumber)
	}
	if got.Name != "张三" || got.Mobile != "13800000000" {
		t.Errorf("应保留个人资料，实际 %+v", got)
	}
	if !strings.Contains(got.ContactError, "qyapi_get_member") {
		t.Errorf("降级原因应指出缺失的权限，实际 %q", got.ContactError)
	}
	if strings.Contains(got.ContactError, "access_token") {
		t.Errorf("降级原因不应包含敏感信息: %q", got.ContactError)
	}
	if got.CorpName != "测试公司" {
		t.Errorf("通讯录失败时企业名称仍应可用，实际 %q", got.CorpName)
	}
}

// 敏感字段（邮箱/手机号）未授权时，钉钉不返回 email 字段：工号正常，但要提示原因。
func TestProfileEmailMissingHint(t *testing.T) {
	p, _ := newFakeDingTalk(t, 0, 0, "")

	got, err := p.Profile(context.Background(), Credential{AccessToken: "user-token"})
	if err != nil {
		t.Fatalf("缺邮箱不应阻断登录: %v", err)
	}
	if got.JobNumber != "A001" {
		t.Errorf("工号应正常返回，实际 %q", got.JobNumber)
	}
	if got.Email != "" {
		t.Errorf("邮箱应为空，实际 %q", got.Email)
	}
	if !strings.Contains(got.ContactError, "邮箱") {
		t.Errorf("应提示邮箱缺失原因，实际 %q", got.ContactError)
	}
}

func TestProfileCorpNameFallback(t *testing.T) {
	p, _ := newFakeDingTalk(t, 60011, http.StatusForbidden, "")
	p.CorpNameFallback = "兜底公司"

	got, err := p.Profile(context.Background(), Credential{AccessToken: "user-token"})
	if err != nil {
		t.Fatalf("企业名称失败不应阻断登录: %v", err)
	}
	if got.CorpName != "兜底公司" {
		t.Errorf("未开通 Contact.Org.Read 时应使用兜底企业名，实际 %q", got.CorpName)
	}
}

func TestContactErrorMessage(t *testing.T) {
	msg := contactErrorMessage(&APIError{
		Code: 88, SubCode: "60011", SubMsg: "应用尚未开通所需的权限：[qyapi_get_member]",
	})
	if !strings.Contains(msg, "qyapi_get_member") {
		t.Errorf("缺少权限的提示应包含权限点，实际 %q", msg)
	}
	if strings.Contains(msg, "access_token") {
		t.Errorf("提示不应包含敏感信息: %q", msg)
	}
}

func TestUnimplementedProviders(t *testing.T) {
	for _, name := range []string{"wecom", "feishu", "unknown-idp"} {
		cfg := &config.Config{IdentityProvider: name}
		p := New(cfg, slog.Default())
		if p.Name() != name {
			t.Errorf("provider 名称应为 %s，实际 %s", name, p.Name())
		}
		if p.Ready() {
			t.Errorf("%s 尚未实现，不应处于就绪状态", name)
		}
		if _, err := p.AuthorizeURL("state"); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s 应返回 ErrNotImplemented，实际 %v", name, err)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("写入响应失败: %v", err)
	}
}
