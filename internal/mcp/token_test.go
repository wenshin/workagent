package mcp

import (
	"strings"
	"testing"
)

func TestSignAndVerifyUserToken(t *testing.T) {
	key, err := NewSigningKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	claims := UserClaims{
		Subject:   "u_1001",
		UserID:    "u_1001",
		Email:     "yanxiaoxu99999@autohome.com.cn",
		JobNumber: "99999",
	}
	token, err := SignUserToken(key, "crm_mcp", claims, UserTokenTTL)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	got, err := VerifyUserToken(key, token)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if got.Iss != JWTIssuer || got.Aud != "crm_mcp" {
		t.Fatalf("iss/aud 不对: %+v", got)
	}
	if got.UserID != claims.UserID || got.Email != claims.Email || got.JobNumber != claims.JobNumber {
		t.Fatalf("用户声明丢失: %+v", got)
	}
	if got.Exp <= got.Iat {
		t.Fatalf("过期时间应晚于签发时间: %+v", got)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	key, _ := NewSigningKey()
	token, _ := SignUserToken(key, "crm_mcp", UserClaims{Subject: "u_1", UserID: "u_1"}, UserTokenTTL)

	parts := strings.Split(token, ".")
	// 换掉 payload 但保留原签名：验签必须失败，不能只看格式。
	if _, err := VerifyUserToken(key, parts[0]+"."+parts[1]+"x."+parts[2]); err == nil {
		t.Fatal("篡改 payload 后仍然验签通过")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	key, _ := NewSigningKey()
	other, _ := NewSigningKey()
	token, _ := SignUserToken(key, "crm_mcp", UserClaims{Subject: "u_1", UserID: "u_1"}, UserTokenTTL)

	if _, err := VerifyUserToken(other, token); err == nil {
		t.Fatal("用别的应用密钥也验过了：应用隔离失效")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	key, _ := NewSigningKey()
	// 手工构造一个签名合法但 exp 已过去的令牌：签发入口不接受非正有效期。
	header := encodeSegment([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := encodeSegment([]byte(`{"sub":"u_1","user_id":"u_1","iss":"workagent","aud":"crm_mcp","iat":1,"exp":2}`))
	token := header + "." + payload + "." + encodeSegment(mac(key, header+"."+payload))

	if _, err := VerifyUserToken(key, token); err == nil {
		t.Fatal("过期令牌应被拒绝")
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	// 攻击面最小验证：alg=none 不能被当成合法令牌。
	token := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJ1XzEifQ."
	if _, err := VerifyUserToken("any-key", token); err == nil {
		t.Fatal("alg=none 应被拒绝")
	}
}
