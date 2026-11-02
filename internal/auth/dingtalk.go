package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	dingtalkAuthBase = "https://login.dingtalk.com"
	dingtalkAPIBase  = "https://api.dingtalk.com"
)

// DingTalkClient 封装钉钉 OAuth2（新版 api.dingtalk.com 接口）。
type DingTalkClient struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	AuthBase     string
	APIBase      string
	HTTP         *http.Client
}

// NewDingTalkClient 创建客户端。
func NewDingTalkClient(clientID, clientSecret, redirectURI string) *DingTalkClient {
	return &DingTalkClient{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		AuthBase:     dingtalkAuthBase,
		APIBase:      dingtalkAPIBase,
		HTTP:         &http.Client{Timeout: 10 * time.Second},
	}
}

// AuthorizeURL 生成扫码登录地址。
func (c *DingTalkClient) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("scope", "openid")
	q.Set("state", state)
	q.Set("prompt", "consent")
	return c.AuthBase + "/oauth2/auth?" + q.Encode()
}

type accessToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpireIn     int    `json:"expireIn"`
	CorpID       string `json:"corpId"`
}

// ExchangeCode 用授权码换取用户 accessToken。
func (c *DingTalkClient) ExchangeCode(ctx context.Context, code string) (accessToken, error) {
	payload := map[string]string{
		"clientId":     c.ClientID,
		"clientSecret": c.ClientSecret,
		"code":         code,
		"grantType":    "authorization_code",
	}

	var out accessToken
	if err := c.postJSON(ctx, "/v1.0/oauth2/userAccessToken", payload, &out); err != nil {
		return accessToken{}, err
	}
	if out.AccessToken == "" {
		return accessToken{}, fmt.Errorf("钉钉未返回 accessToken")
	}
	return out, nil
}

// Profile 是钉钉返回的用户资料。
type Profile struct {
	Nick      string `json:"nick"`
	AvatarURL string `json:"avatarUrl"`
	Mobile    string `json:"mobile"`
	OpenID    string `json:"openId"`
	UnionID   string `json:"unionId"`
	Email     string `json:"email"`
}

// UserProfile 获取当前授权用户的资料。
func (c *DingTalkClient) UserProfile(ctx context.Context, token string) (Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/v1.0/contact/users/me", nil)
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("请求钉钉用户信息失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Profile{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Profile{}, fmt.Errorf("钉钉用户信息接口返回 %d: %s", resp.StatusCode, truncate(body, 300))
	}

	var p Profile
	if err := json.Unmarshal(body, &p); err != nil {
		return Profile{}, fmt.Errorf("解析钉钉用户信息失败: %w", err)
	}
	return p, nil
}

func (c *DingTalkClient) postJSON(ctx context.Context, path string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉接口失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// 不打印 clientSecret，仅回传钉钉的错误描述
		return fmt.Errorf("钉钉接口 %s 返回 %d: %s", path, resp.StatusCode, truncate(body, 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
