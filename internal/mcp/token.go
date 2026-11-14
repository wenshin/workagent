package mcp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MCP 应用令牌的签发约定。
//
// 每个 MCP 应用（插件清单里的 auth.app）在平台侧有独立的 HMAC 密钥，
// 平台用它给「用户身份」签名，MCP Server 用同一把密钥验签后取用户信息。
// 密钥只在两个地方出现：凭证存储（scope=MCPJWTScope）与装配层内存，
// 不落日志、不进模型上下文、不写进插件产物。
const (
	// JWTIssuer 是平台签发的 MCP 令牌的 iss。
	JWTIssuer = "workagent"
	// MCPJWTScope 是应用签名密钥在凭证存储里的分组。
	MCPJWTScope = "mcp_jwt"
	// UserTokenTTL 是用户令牌寿命：每次工具调用现签，不跨调用复用。
	UserTokenTTL = 5 * time.Minute
	// ServiceTokenTTL 是服务令牌寿命：仅用于握手与 tools/list。
	ServiceTokenTTL = time.Hour

	jwtAlg = "HS256"
)

// UserClaims 是平台注入给 MCP Server 的身份声明。
//
// 用户令牌带 sub/user_id/email/job_number；服务令牌（握手用）只有
// sub=app:<app> 与 act=platform，用户字段留空。
type UserClaims struct {
	Subject   string `json:"sub"`
	UserID    string `json:"user_id,omitempty"`
	Email     string `json:"email,omitempty"`
	JobNumber string `json:"job_number,omitempty"`
	// Act 非空表示这是一次平台发起的调用（act=platform），不是某个用户。
	Act string `json:"act,omitempty"`
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// NewSigningKey 生成一个应用级 HMAC 密钥：32 字节随机，base64url 无填充。
func NewSigningKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成密钥失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SignUserToken 用应用密钥签发令牌，ttl 为有效期。
func SignUserToken(secret, app string, c UserClaims, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("签发密钥为空")
	}
	if app == "" {
		return "", errors.New("应用名为空")
	}
	if ttl <= 0 {
		return "", errors.New("令牌有效期必须为正数")
	}
	now := time.Now()
	c.Iss = JWTIssuer
	c.Aud = app
	c.Iat = now.Unix()
	c.Exp = now.Add(ttl).Unix()
	if c.Subject == "" {
		c.Subject = c.UserID
	}

	header, err := json.Marshal(map[string]string{"alg": jwtAlg, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := encodeSegment(header) + "." + encodeSegment(payload)
	return signing + "." + encodeSegment(mac(secret, signing)), nil
}

// VerifyUserToken 校验签名与过期时间，返回声明。
//
// 平台自己不用它（签发端无需验签），但 mock 服务与第三方 MCP Server
// 的直接测试用例需要同一套实现，放在这里避免两处各写一遍。
func VerifyUserToken(secret, token string) (UserClaims, error) {
	var out UserClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out, errors.New("token 格式不是 header.payload.signature")
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return out, errors.New("token header 不是 base64url")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return out, errors.New("token header 不是 JSON")
	}
	// 只接受自己签发时用的算法：alg=none 与降级到弱算法都在这里被挡掉。
	if header.Alg != jwtAlg {
		return out, fmt.Errorf("不支持的签名算法 %q", header.Alg)
	}

	expected := mac(secret, parts[0]+"."+parts[1])
	actual, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return out, errors.New("token 签名不是 base64url")
	}
	if !hmac.Equal(expected, actual) {
		return out, errors.New("token 签名校验失败")
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, errors.New("token payload 不是 base64url")
	}
	if err := json.Unmarshal(payloadRaw, &out); err != nil {
		return out, errors.New("token payload 不是 JSON")
	}
	if out.Exp > 0 && time.Now().After(time.Unix(out.Exp, 0)) {
		return out, errors.New("token 已过期")
	}
	return out, nil
}

func encodeSegment(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mac(secret, signing string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(signing))
	return h.Sum(nil)
}
