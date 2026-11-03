package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/config"
)

const (
	dingtalkAuthBase = "https://login.dingtalk.com"
	dingtalkAPIBase  = "https://api.dingtalk.com"
	dingtalkOAPIBase = "https://oapi.dingtalk.com"

	// corpTokenSkew 提前刷新企业令牌，避免边界过期
	corpTokenSkew = 60 * time.Second
)

// APIError 保留钉钉的 errcode / sub_code，便于把「缺哪个权限」翻译成用户可读提示。
type APIError struct {
	Path    string
	Code    int
	SubCode string
	Message string
	SubMsg  string
}

func (e *APIError) Error() string {
	if e.SubCode != "" {
		return fmt.Sprintf("钉钉接口 %s 返回 %d/%s: %s", e.Path, e.Code, e.SubCode, e.SubMsg)
	}
	return fmt.Sprintf("钉钉接口 %s 返回 %d: %s", e.Path, e.Code, e.Message)
}

// emailMissingHint 用于「通讯录同步成功但没拿到邮箱」的场景：
// 钉钉把邮箱/手机号列为敏感字段，未单独授权时整段字段不会返回。
const emailMissingHint = "通讯录已同步，但钉钉未返回邮箱：应用可能缺少『邮箱等个人信息』权限，" +
	"或该成员在钉钉通讯录中未填写企业邮箱。管理员可在钉钉开放平台-权限管理-通讯录管理勾选" +
	"『企业员工手机号信息』『邮箱等个人信息』后重新登录。"

// contactErrorMessage 把钉钉错误翻译成可落库、可展示给用户的提示（不含 token 等敏感信息）。
func contactErrorMessage(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.SubCode == "60011" || strings.Contains(apiErr.SubMsg, "qyapi_get_member"):
			return "企业资料未同步：应用缺少通讯录成员读权限（qyapi_get_member），工号与邮箱为空。" +
				"请管理员在钉钉开放平台申请该权限后重新登录。"
		case apiErr.Code == 88 || apiErr.Code == 403:
			return "企业资料未同步：通讯录权限不足（" + firstNonEmpty(apiErr.SubCode, apiErr.SubMsg) + "），" +
				"工号与邮箱为空。请管理员在钉钉开放平台申请权限后重新登录。"
		default:
			return fmt.Sprintf("企业资料未同步：钉钉通讯录接口返回 %d/%s，工号与邮箱为空。", apiErr.Code, apiErr.SubCode)
		}
	}
	if errors.Is(err, ErrNotConfigured) {
		return "企业资料未同步：钉钉应用凭证未配置，工号与邮箱为空。"
	}
	return "企业资料未同步：无法访问钉钉通讯录，工号与邮箱为空。"
}

// DingTalk 是钉钉身份接入实现（扫码登录 + 企业通讯录补全工号/邮箱）。
type DingTalk struct {
	ClientID     string
	ClientSecret string
	CorpID       string
	// CorpNameFallback 是未开通 Contact.Org.Read 时的企业名称兜底（DINGTALK_CORP_NAME）
	CorpNameFallback string
	RedirectURI      string
	AuthBase         string
	APIBase          string
	OAPIBase         string
	HTTP             *http.Client
	Log              *slog.Logger

	mu            sync.Mutex
	corpToken     string
	corpExpire    time.Time
	corpNameCache string
	corpNameAt    time.Time
}

func newDingTalk(cfg *config.Config, log *slog.Logger) *DingTalk {
	return &DingTalk{
		ClientID:         cfg.DingTalkClientID,
		ClientSecret:     cfg.DingTalkClientSecret,
		CorpID:           cfg.DingTalkCorpID,
		CorpNameFallback: cfg.DingTalkCorpName,
		RedirectURI:      cfg.DingTalkRedirectURI,
		AuthBase:         dingtalkAuthBase,
		APIBase:          dingtalkAPIBase,
		OAPIBase:         dingtalkOAPIBase,
		HTTP:             &http.Client{Timeout: 10 * time.Second},
		Log:              log,
	}
}

func (p *DingTalk) Name() string        { return "dingtalk" }
func (p *DingTalk) DisplayName() string { return "钉钉" }

// Ready 判断扫码登录所需凭证是否齐全（通讯录额外需要 CorpID，缺失只降级）。
func (p *DingTalk) Ready() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

func (p *DingTalk) Missing() []string {
	missing := make([]string, 0, 2)
	if p.ClientID == "" {
		missing = append(missing, "DINGTALK_CLIENT_ID")
	}
	if p.ClientSecret == "" {
		missing = append(missing, "DINGTALK_CLIENT_SECRET")
	}
	return missing
}

// AuthorizeURL 生成扫码登录地址。
func (p *DingTalk) AuthorizeURL(state string) (string, error) {
	if !p.Ready() {
		return "", ErrNotConfigured
	}
	q := url.Values{}
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("scope", "openid")
	q.Set("state", state)
	q.Set("prompt", "consent")
	return p.AuthBase + "/oauth2/auth?" + q.Encode(), nil
}

type userAccessToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpireIn     int    `json:"expireIn"`
	CorpID       string `json:"corpId"`
}

// Exchange 用授权码换取用户 accessToken。
func (p *DingTalk) Exchange(ctx context.Context, code string) (Credential, error) {
	if !p.Ready() {
		return Credential{}, ErrNotConfigured
	}
	if code == "" {
		return Credential{}, ErrInvalidRequest
	}

	payload := map[string]string{
		"clientId":     p.ClientID,
		"clientSecret": p.ClientSecret,
		"code":         code,
		"grantType":    "authorization_code",
	}

	var out userAccessToken
	if err := p.postJSON(ctx, p.APIBase+"/v1.0/oauth2/userAccessToken", payload, &out); err != nil {
		return Credential{}, err
	}
	if out.AccessToken == "" {
		return Credential{}, fmt.Errorf("钉钉未返回 accessToken")
	}
	return Credential{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresIn:    out.ExpireIn,
		CorpID:       out.CorpID,
	}, nil
}

// meProfile 是 GET /v1.0/contact/users/me 的返回（个人资料，不含工号）。
type meProfile struct {
	Nick      string `json:"nick"`
	AvatarURL string `json:"avatarUrl"`
	Mobile    string `json:"mobile"`
	OpenID    string `json:"openId"`
	UnionID   string `json:"unionId"`
	Email     string `json:"email"`
}

// contactUser 是 /topapi/v2/user/get 的 result（企业通讯录，含工号）。
type contactUser struct {
	UserID    string  `json:"userid"`
	UnionID   string  `json:"unionid"`
	Name      string  `json:"name"`
	Avatar    string  `json:"avatar"`
	Mobile    string  `json:"mobile"`
	Email     string  `json:"email"`
	JobNumber string  `json:"job_number"`
	Title     string  `json:"title"`
	DeptIDs   []int64 `json:"dept_id_list"`
}

// Profile 拉取个人资料并用企业通讯录补全工号/邮箱。
//
// 通讯录链路失败（未配 CorpID、无权限、接口异常）只告警降级：返回个人资料，
// 工号与邮箱留空，不阻断登录（§5.1.1）。
func (p *DingTalk) Profile(ctx context.Context, cred Credential) (Profile, error) {
	if cred.AccessToken == "" {
		return Profile{}, ErrInvalidRequest
	}

	var me meProfile
	if err := p.getJSON(ctx, p.APIBase+"/v1.0/contact/users/me", cred.AccessToken, &me); err != nil {
		return Profile{}, err
	}

	out := Profile{
		Provider:    p.Name(),
		ProviderUID: firstNonEmpty(me.UnionID, me.OpenID),
		UnionID:     me.UnionID,
		OpenID:      me.OpenID,
		Name:        me.Nick,
		Avatar:      me.AvatarURL,
		Mobile:      me.Mobile,
		Email:       me.Email,
		CorpName:    p.corpName(ctx),
	}
	if out.ProviderUID == "" {
		return Profile{}, ErrNoIdentity
	}
	// 没有 unionId 就换不到 userid，直接返回个人资料
	if me.UnionID == "" {
		return out, nil
	}

	detail, raw, err := p.contactUser(ctx, me.UnionID)
	if err != nil {
		// 降级原因必须让用户看见：写入 contact_error，随 /auth/me 的 profile_warning 返回
		out.ContactError = contactErrorMessage(err)
		if p.Log != nil {
			p.Log.Warn("钉钉通讯录补全失败，降级为个人资料",
				"err", err, "union_id", me.UnionID, "corp_id_configured", p.CorpID != "")
		}
		return out, nil
	}

	out.EmployeeID = detail.UserID
	out.Name = firstNonEmpty(detail.Name, out.Name)
	out.Avatar = firstNonEmpty(detail.Avatar, out.Avatar)
	out.Mobile = firstNonEmpty(detail.Mobile, out.Mobile)
	out.Email = firstNonEmpty(detail.Email, out.Email)
	out.JobNumber = detail.JobNumber
	out.Title = detail.Title
	out.DeptIDs = make([]string, 0, len(detail.DeptIDs))
	for _, id := range detail.DeptIDs {
		out.DeptIDs = append(out.DeptIDs, fmt.Sprintf("%d", id))
	}
	// 邮箱/手机号是敏感字段：未开通「企业员工手机号信息/邮箱等个人信息」时，
	// 钉钉的 v2/user/get 返回里直接不含 email 字段（不是空字符串），需要提示管理员。
	if out.Email == "" {
		out.ContactError = emailMissingHint
	}
	out.Raw = raw
	return out, nil
}

// contactUser 走「企业令牌 → unionid 换 userid → 用户详情」三步。
func (p *DingTalk) contactUser(ctx context.Context, unionID string) (contactUser, []byte, error) {
	token, err := p.corpAccessToken(ctx)
	if err != nil {
		return contactUser{}, nil, err
	}

	var byUnion struct {
		Result struct {
			UserID string `json:"userid"`
		} `json:"result"`
	}
	if err := p.topAPI(ctx, token, "/topapi/user/getbyunionid", map[string]string{"unionid": unionID}, &byUnion); err != nil {
		return contactUser{}, nil, err
	}
	if byUnion.Result.UserID == "" {
		return contactUser{}, nil, fmt.Errorf("钉钉未返回该 unionId 对应的 userid")
	}

	var detail struct {
		Result contactUser `json:"result"`
	}
	raw, err := p.topAPIRaw(ctx, token, "/topapi/v2/user/get", map[string]string{
		"userid":   byUnion.Result.UserID,
		"language": "zh_CN",
	}, &detail)
	if err != nil {
		return contactUser{}, nil, err
	}
	if detail.Result.UserID == "" {
		detail.Result.UserID = byUnion.Result.UserID
	}
	return detail.Result, raw, nil
}

// corpName 取企业名称（orgName），成功缓存 1 小时；失败用 DINGTALK_CORP_NAME 兜底。
//
// 注意：不能持有 p.mu 调用 corpAccessToken（它自己加锁），否则死锁。
func (p *DingTalk) corpName(ctx context.Context) string {
	p.mu.Lock()
	cached, at := p.corpNameCache, p.corpNameAt
	p.mu.Unlock()
	if cached != "" && time.Since(at) < time.Hour {
		return cached
	}

	name := ""
	if p.CorpID != "" {
		token, err := p.corpAccessToken(ctx)
		if err != nil {
			if p.Log != nil {
				p.Log.Warn("获取钉钉企业令牌失败，跳过企业名称", "err", err)
			}
		} else if got, err := p.fetchOrgName(ctx, token); err != nil {
			if p.Log != nil {
				p.Log.Warn("获取钉钉企业名称失败，使用兜底值",
					"err", err, "fallback_configured", p.CorpNameFallback != "")
			}
		} else {
			name = got
		}
	}
	if name == "" {
		name = p.CorpNameFallback
	}
	if name != "" {
		p.mu.Lock()
		p.corpNameCache, p.corpNameAt = name, time.Now()
		p.mu.Unlock()
	}
	return name
}

// fetchOrgName 调用获取企业认证信息接口，返回企业通讯录名称。
// 需要 Contact.Org.Read 权限，未开通时返回 403。
func (p *DingTalk) fetchOrgName(ctx context.Context, token string) (string, error) {
	endpoint := p.APIBase + "/v1.0/contact/organizations/authInfos?targetCorpId=" + url.QueryEscape(p.CorpID)
	var out struct {
		OrgName string `json:"orgName"`
	}
	if err := p.getJSON(ctx, endpoint, token, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.OrgName), nil
}

// corpAccessToken 取企业内部应用 accessToken，进程内缓存到过期前 60s。
func (p *DingTalk) corpAccessToken(ctx context.Context) (string, error) {
	if p.ClientID == "" || p.ClientSecret == "" {
		return "", ErrNotConfigured
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.corpToken != "" && time.Now().Before(p.corpExpire) {
		return p.corpToken, nil
	}

	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err := p.postJSON(ctx, p.APIBase+"/v1.0/oauth2/accessToken", map[string]string{
		"appKey":    p.ClientID,
		"appSecret": p.ClientSecret,
	}, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("钉钉未返回企业 accessToken")
	}

	ttl := time.Duration(out.ExpireIn) * time.Second
	if ttl <= corpTokenSkew {
		ttl = 2 * time.Hour
	}
	p.corpToken = out.AccessToken
	p.corpExpire = time.Now().Add(ttl - corpTokenSkew)
	return p.corpToken, nil
}

func (p *DingTalk) topAPI(ctx context.Context, token, path string, in, out any) error {
	_, err := p.topAPIRaw(ctx, token, path, in, out)
	return err
}

// topAPIRaw 调用旧版 topapi（oapi.dingtalk.com），返回原始响应体。
func (p *DingTalk) topAPIRaw(ctx context.Context, token, path string, in, out any) ([]byte, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	endpoint := p.OAPIBase + path + "?access_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求钉钉接口失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("钉钉接口 %s 返回 %d: %s", path, resp.StatusCode, truncate(body, 300))
	}

	var meta struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("解析钉钉接口 %s 返回失败: %w", path, err)
	}
	if meta.ErrCode != 0 {
		return nil, &APIError{
			Path: path, Code: meta.ErrCode, SubCode: meta.SubCode,
			Message: meta.ErrMsg, SubMsg: meta.SubMsg,
		}
	}
	if out == nil {
		return body, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("解析钉钉接口 %s 返回失败: %w", path, err)
	}
	return body, nil
}

func (p *DingTalk) getJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉用户信息失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉用户信息接口返回 %d: %s", resp.StatusCode, truncate(body, 300))
	}
	return json.Unmarshal(body, out)
}

func (p *DingTalk) postJSON(ctx context.Context, endpoint string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTP.Do(req)
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
		return fmt.Errorf("钉钉接口返回 %d: %s", resp.StatusCode, truncate(body, 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
