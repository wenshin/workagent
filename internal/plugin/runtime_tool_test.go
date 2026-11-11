package plugin

import "testing"

// TestReplaceURLSecrets 锁定 URL 引用的替换规则：
// 只有「query 参数值整体是 secret:name」才替换，别处出现的 secret: 一律不动。
func TestReplaceURLSecrets(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "dingtalk_token":
			return "abc123+/=", true
		case "cn_token":
			return "密钥", true
		}
		return "", false
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "query 引用被替换并按 URL 规则编码",
			in:   "https://oapi.dingtalk.com/robot/send?access_token=secret:dingtalk_token",
			want: "https://oapi.dingtalk.com/robot/send?access_token=abc123%2B%2F%3D",
		},
		{
			name: "中文密钥同样编码",
			in:   "https://x.example.com/a?token=secret:cn_token",
			want: "https://x.example.com/a?token=%E5%AF%86%E9%92%A5",
		},
		{
			name: "没有引用时原样返回",
			in:   "https://x.example.com/a?q=1",
			want: "https://x.example.com/a?q=1",
		},
		{
			name: "路径里的 secret: 不当引用",
			in:   "https://x.example.com/secret:dingtalk_token/a",
			want: "https://x.example.com/secret:dingtalk_token/a",
		},
		{
			name: "参数值中间出现 secret: 不替换",
			in:   "https://x.example.com/a?q=pre-secret:dingtalk_token",
			want: "https://x.example.com/a?q=pre-secret:dingtalk_token",
		},
		{
			name: "未知密钥替换为空，让请求在目标侧明确失败",
			in:   "https://x.example.com/a?token=secret:missing",
			want: "https://x.example.com/a?token=",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaceURLSecrets(tc.in, lookup); got != tc.want {
				t.Fatalf("replaceURLSecrets(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}
