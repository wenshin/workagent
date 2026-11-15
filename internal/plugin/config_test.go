package plugin

import (
	"encoding/json"
	"testing"
)

func TestNormalizeConfig(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		// 清单是否声明 allow_http：安装级配置不自己放宽这个决定
		allowHTTP bool
		wantErr   bool
		check     func(t *testing.T, got map[string]string)
	}{
		{
			name:   "空值表示沿用清单",
			values: map[string]string{ConfigURL: "  ", ConfigSecret: ""},
			check: func(t *testing.T, got map[string]string) {
				if len(got) != 0 {
					t.Fatalf("空值应被裁剪，得到 %v", got)
				}
			},
		},
		{
			name:   "https 通过",
			values: map[string]string{ConfigURL: "https://mcp.example.com/mcp", ConfigSecret: "mcp-key"},
			check: func(t *testing.T, got map[string]string) {
				if got[ConfigURL] != "https://mcp.example.com/mcp" || got[ConfigSecret] != "mcp-key" {
					t.Fatalf("配置未保留：%v", got)
				}
			},
		},
		{
			name:   "本机 http 放行",
			values: map[string]string{ConfigURL: "http://127.0.0.1:3000/mcp"},
		},
		{
			name:    "非本机 http 拒绝",
			values:  map[string]string{ConfigURL: "http://mcp.example.com/mcp"},
			wantErr: true,
		},
		{
			name:      "清单允许明文后内网 http 通过",
			values:    map[string]string{ConfigURL: "http://crm.internal/mcp"},
			allowHTTP: true,
			check: func(t *testing.T, got map[string]string) {
				if got[ConfigURL] != "http://crm.internal/mcp" {
					t.Fatalf("配置未保留：%v", got)
				}
			},
		},
		{
			name:    "带用户名密码拒绝",
			values:  map[string]string{ConfigURL: "https://user:pass@mcp.example.com/mcp"},
			wantErr: true,
		},
		{
			name:    "未知键拒绝",
			values:  map[string]string{"dsn": "postgres://x"},
			wantErr: true,
		},
		{
			name:    "凭证名非法字符拒绝",
			values:  map[string]string{ConfigSecret: "a b"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeConfig(tc.values, tc.allowHTTP)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// 安装级覆盖只作用于 MCP，并且不修改发布快照本身。
func TestApplyInstallConfigShape(t *testing.T) {
	meta := Manifest{
		Kind:      KindMCP,
		Transport: &Transport{Type: "http", URL: "https://published.example.com/mcp"},
		Auth:      &Auth{In: "header", Name: "Authorization", Secret: "published-key"},
	}
	raw, _ := json.Marshal(map[string]string{ConfigURL: "https://space.example.com/mcp"})

	// 直接验证合并规则，避免依赖 DB：applyInstallConfig 的取值语义与这里一致。
	values := configValues(meta, raw)
	if values[ConfigURL] != "https://space.example.com/mcp" {
		t.Fatalf("覆盖值未生效：%v", values)
	}
	if values[ConfigSecret] != "published-key" {
		t.Fatalf("未覆盖的字段应沿用清单：%v", values)
	}
	if meta.Transport.URL != "https://published.example.com/mcp" {
		t.Fatalf("原始清单不应被修改")
	}
}
