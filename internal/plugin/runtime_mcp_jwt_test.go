package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// jwtSecrets 是支持写入的凭证存储替身：应用密钥需要能生成并保存。
type jwtSecrets struct {
	mu   sync.Mutex
	data map[string]string
}

func (f *jwtSecrets) Enabled() bool { return true }

func (f *jwtSecrets) Get(_ context.Context, scope, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.data[scope+"/"+name]; ok {
		return v, nil
	}
	return "", errors.New("不存在")
}

func (f *jwtSecrets) Put(_ context.Context, scope, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[scope+"/"+name] = value
	return nil
}

// jwtUsers 只认一个用户，用来验证签发时确实按调用者取信息。
type jwtUsers struct{ user store.User }

func (u jwtUsers) GetUser(_ context.Context, id string) (store.User, error) {
	if id != u.user.ID {
		return store.User{}, errors.New("用户不存在")
	}
	return u.user, nil
}

// jwtMCPServer 是一个只实现握手、清单与 echo 调用的 MCP Server，
// 记录每个请求带的 Authorization，供断言「用户身份有没有正确注入」。
func jwtMCPServer(t *testing.T, seen map[string][]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		seen[req.Method] = append(seen[req.Method], r.Header.Get("Authorization"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
					"isError": false,
				},
			})
		default:
			// notifications/initialized 没有 id，按 MCP 约定回 202。
			w.WriteHeader(http.StatusAccepted)
		}
	}))
}

func jwtMeta(url string) Manifest {
	return Manifest{
		Protocol: Protocol, Kind: KindMCP, Slug: "crm_mcp", Name: "CRM", Description: "客户查询",
		Transport: &Transport{Type: "http", URL: url},
		Auth:      &Auth{Mode: AuthModeJWT, App: "crm_app"},
		Tools:     &AllowedTools{Allow: []string{"echo"}},
	}
}

// 每次 tools/call 都要带当前用户的令牌，且 claims 里的信息来自用户目录。
func TestMCPJWTSignsUserTokenPerCall(t *testing.T) {
	seen := map[string][]string{}
	srv := jwtMCPServer(t, seen)
	defer srv.Close()

	secrets := &jwtSecrets{data: map[string]string{}}
	users := jwtUsers{user: store.User{
		ID: "u_1001", Name: "严小序", Email: "yanxiaoxu99999@autohome.com.cn", JobNumber: "99999",
	}}
	meta := jwtMeta(srv.URL + "/mcp")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	rt, err := newMCPRuntime(context.Background(), meta, secrets, users, log, "")
	if err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 握手阶段没有用户上下文：用服务令牌（act=platform），不带用户字段。
	handshake := strings.TrimPrefix(seen["initialize"][0], "Bearer ")
	key, err := secrets.Get(context.Background(), mcp.MCPJWTScope, "crm_app")
	if err != nil {
		t.Fatalf("应用密钥没生成: %v", err)
	}
	svc, err := mcp.VerifyUserToken(key, handshake)
	if err != nil {
		t.Fatalf("服务令牌验签失败: %v", err)
	}
	if svc.Act != "platform" || svc.UserID != "" || svc.Aud != "crm_app" {
		t.Fatalf("服务令牌声明不对: %+v", svc)
	}

	if out, err := rt.call(context.Background(), "echo", json.RawMessage(`{}`), tool.Env{UserID: "u_1001"}); err != nil {
		t.Fatalf("调用失败: %v", err)
	} else if out != "ok" {
		t.Fatalf("结果不对: %q", out)
	}

	calls := seen["tools/call"]
	if len(calls) != 1 {
		t.Fatalf("tools/call 次数不对: %d", len(calls))
	}
	token := strings.TrimPrefix(calls[0], "Bearer ")
	claims, err := mcp.VerifyUserToken(key, token)
	if err != nil {
		t.Fatalf("用户令牌验签失败: %v", err)
	}
	if claims.Subject != "u_1001" || claims.UserID != "u_1001" {
		t.Fatalf("sub/user_id 不对: %+v", claims)
	}
	if claims.Email != "yanxiaoxu99999@autohome.com.cn" || claims.JobNumber != "99999" {
		t.Fatalf("邮箱/工号没进令牌: %+v", claims)
	}
	if claims.Iss != mcp.JWTIssuer || claims.Aud != "crm_app" || claims.Act != "" {
		t.Fatalf("iss/aud/act 不对: %+v", claims)
	}
}

// 没有调用者身份时不能退化成匿名调用：直接失败，让上层看到原因。
func TestMCPJWTRequiresUserIdentity(t *testing.T) {
	seen := map[string][]string{}
	srv := jwtMCPServer(t, seen)
	defer srv.Close()

	secrets := &jwtSecrets{data: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt, err := newMCPRuntime(context.Background(), jwtMeta(srv.URL+"/mcp"), secrets,
		jwtUsers{user: store.User{ID: "u_1001"}}, log, "")
	if err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	if _, err := rt.call(context.Background(), "echo", json.RawMessage(`{}`), tool.Env{}); err == nil {
		t.Fatal("缺少用户身份时应报错，而不是以服务身份调用")
	}
	if len(seen["tools/call"]) != 0 {
		t.Fatal("没有身份的调用不应该发到远端")
	}
}

// 凭证存储没有写能力（未配置 SECRET_KEK）时，jwt 插件必须明确不可用。
func TestMCPJWTWithoutSecretStoreFailsLoudly(t *testing.T) {
	seen := map[string][]string{}
	srv := jwtMCPServer(t, seen)
	defer srv.Close()

	// fakeSecrets 只有 Get：模拟凭证存储未启用。
	meta := jwtMeta(srv.URL + "/mcp")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := newMCPRuntime(context.Background(), meta, fakeSecrets{values: map[string]string{}}, nil, log, "")
	if err == nil {
		t.Fatal("凭证存储不可写时应拒绝装配")
	}
	if !errors.Is(err, errSecretsDisabled) {
		t.Fatalf("错误应指向凭证存储未启用: %v", err)
	}
}
