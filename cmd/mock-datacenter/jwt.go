package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// jwtAlg 固定 HS256：mock 服务不引第三方依赖，只用标准库 crypto/hmac。
// 真实平台按技术方案（§4 身份）用 RS256 / Ed25519 签发，公开公钥供服务端校验；
// mock 侧只关心「能不能验签 + 身份字段对不对」，算法差异不影响联调结论。
const jwtAlg = "HS256"

// Claims 是 mock 数仓接受的身份声明。
//
// 字段对齐技术方案里 access_token 的口径（sub / tenant / roles / scope），
// 额外补了 mock 场景需要的 name / job_number / space_id，便于断言「平台注入的
// token 到底带了谁、从哪个空间发起」。
type Claims struct {
	Iss       string `json:"iss"`                  // 签发者
	Aud       string `json:"aud"`                  // 受众（应用名，对齐平台 auth.app）
	Sub       string `json:"sub"`                  // 用户唯一标识
	UserID    string `json:"user_id,omitempty"`    // 平台用户 ID（与 sub 一致）
	Name      string `json:"name,omitempty"`       // 用户姓名
	Email     string `json:"email,omitempty"`      // 邮箱
	JobNumber string `json:"job_number,omitempty"` // 工号
	SpaceID   string `json:"space_id,omitempty"`   // 平台空间 ID
	Scope     string `json:"scope,omitempty"`      // 授权范围，空格分隔
	Act       string `json:"act,omitempty"`        // 委派场景下的 agent 标识（RFC 8693）
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
}

// jwtIssuer 负责签发与校验 HS256 token。
type jwtIssuer struct {
	secret  string
	iss     string
	aud     string
	skipAud bool
}

func newJWTIssuer(secret, iss, aud string, skipAud bool) *jwtIssuer {
	return &jwtIssuer{secret: secret, iss: iss, aud: aud, skipAud: skipAud}
}

// sign 签发 token；Exp 为 0 时默认 24h（mock 环境不想频繁换 token）。
// 返回填充后的声明，便于调用方回显实际生效的 iss/aud/exp。
func (i *jwtIssuer) sign(c Claims) (string, Claims, error) {
	now := time.Now()
	c.Iss = i.iss
	if c.Aud == "" {
		c.Aud = i.aud
	}
	c.Iat = now.Unix()
	if c.Exp == 0 {
		c.Exp = now.Add(24 * time.Hour).Unix()
	}
	header, err := json.Marshal(map[string]string{"alg": jwtAlg, "typ": "JWT"})
	if err != nil {
		return "", c, err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", c, err
	}
	signing := encodeSegment(header) + "." + encodeSegment(payload)
	return signing + "." + encodeSegment(i.mac(signing)), c, nil
}

// verify 校验签名、有效期与签发者/受众，返回解析后的声明。
func (i *jwtIssuer) verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("token 格式不合法，应为 header.payload.signature")
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("token header 不是合法的 base64url")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, errors.New("token header 不是合法的 JSON")
	}
	// 显式拒绝 alg=none / 其他算法：不校验算法就等于没有签名。
	if header.Alg != jwtAlg {
		return nil, fmt.Errorf("不支持的签名算法 %q，mock 只接受 %s", header.Alg, jwtAlg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("token 签名不是合法的 base64url")
	}
	if !hmacEqual(sig, i.mac(parts[0]+"."+parts[1])) {
		return nil, errors.New("token 签名校验失败")
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("token payload 不是合法的 base64url")
	}
	var claims Claims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return nil, errors.New("token payload 不是合法的 JSON")
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, errors.New("token 已过期")
	}
	if i.iss != "" && claims.Iss != i.iss {
		return nil, fmt.Errorf("token 签发者不匹配：期望 %s，实际 %s", i.iss, claims.Iss)
	}
	if !i.skipAud && i.aud != "" && claims.Aud != i.aud {
		return nil, fmt.Errorf("token 受众不匹配：期望 %s，实际 %s", i.aud, claims.Aud)
	}
	if claims.Sub == "" {
		return nil, errors.New("token 缺少 sub，无法确认调用者身份")
	}
	return &claims, nil
}

func encodeSegment(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// mac 按 HS256 计算签名。
func (i *jwtIssuer) mac(signing string) []byte {
	m := hmac.New(sha256.New, []byte(i.secret))
	m.Write([]byte(signing))
	return m.Sum(nil)
}

// hmacEqual 用常数时间比较签名，避免时序侧信道。
func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }
