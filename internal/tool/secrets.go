package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
)

// secretNameRe 与 internal/secret 的校验保持一致：凭证名只允许字母数字与下划线短横线。
var secretNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// secretRequestSpec 是给前端渲染录入卡片的描述，不含任何凭证值。
type secretRequestSpec struct {
	Scope       string `json:"scope"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Hint        string `json:"hint,omitempty"`
	Configured  bool   `json:"configured"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// State 给模型一个不必再猜的结论（configured / missing / disabled）。
	// 布尔字段容易被长上下文里前面几轮的错误叙事带偏，这里用词直接说清。
	State string `json:"state"`
	// CanWrite 决定卡片给「输入框」还是「请转给管理员」
	CanWrite bool `json:"can_write"`
	// Available 为 false 表示凭证存储没启用（没配 SECRET_KEK）
	Available bool `json:"available"`
}

// secretRequestResult 的字段顺序是刻意的：结论与下一步写在最前面。
// 模型对 JSON 的注意力按出现顺序衰减，把 state/next 放在卡片描述之后时，
// 它更容易被前面几轮「缺少 key」的叙事带走，说完「请去录入」就收尾。
type secretRequestResult struct {
	State   string            `json:"state"`
	Next    string            `json:"next"`
	Request secretRequestSpec `json:"secret_request"`
}

// secretRequest 让用户在会话界面上录入一条凭证，工具本身不接收凭证值。
//
// 这是「密钥永不进上下文」在会话里的落点：模型只说「我要一个叫 bocha 的凭证」，
// 明文由用户在浏览器里填、前端直接写凭证存储，工具参数、模型上下文、run_step
// 审计里都不会出现明文。模型只拿到「配过没有 + 指纹」。
func secretRequest() Tool {
	return Tool{
		Name:       "secret_request",
		Capability: "system.secret.request",
		Description: "查询一条凭证是否已配置，未配置时请用户在会话界面上录入（API Key / Token / 密码）。" +
			"先调用它查状态：回执 state=configured 说明凭证已存在，直接往下做，不要提示用户录入；" +
			"state=missing 时卡片会自己出现在消息里，用户填完再继续；state=disabled 说明部署没配 SECRET_KEK。" +
			"你只提供凭证名与用途，明文不会进入对话、不会进入你的上下文，你只会拿到「是否已配置 + 指纹」。" +
			"不要向用户索要明文，也不要把明文写进文件或清单。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "scope": {"type": "string", "description": "凭证分组；搜索源固定用 search，MCP 用 mcp，其他按插件能力自取"},
    "name": {"type": "string", "description": "凭证名，只允许字母数字下划线短横线，如 bocha；清单里引用这个名字"},
    "label": {"type": "string", "description": "给用户看的标题，如「博查搜索 API Key」"},
    "hint": {"type": "string", "description": "一句话说明用途与去哪里申请，会显示在录入卡片上"}
  },
  "required": ["scope", "name"],
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			var in struct {
				Scope string `json:"scope"`
				Name  string `json:"name"`
				Label string `json:"label"`
				Hint  string `json:"hint"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			scope := strings.TrimSpace(in.Scope)
			name := strings.TrimSpace(in.Name)
			if !secretNameRe.MatchString(scope) || !secretNameRe.MatchString(name) {
				return "", fmt.Errorf("凭证名只允许字母、数字、下划线与短横线（1-64 位）：scope=%q name=%q", scope, name)
			}
			label := strings.TrimSpace(in.Label)
			if label == "" {
				label = name
			}

			out := secretRequestResult{Request: secretRequestSpec{
				Scope: scope, Name: name, Label: label,
				Hint: strings.TrimSpace(in.Hint), CanWrite: env.CanWriteSecrets,
			}}
			if env.Secrets == nil || !env.Secrets.Enabled() {
				// 存储没开就不要引导用户去填：填了也存不住，等于变相索要明文
				out.State, out.Request.State = "disabled", "disabled"
				out.Next = "state=disabled：凭证存储未启用（部署时没配置 SECRET_KEK）。先如实告诉用户这件事，" +
					"不要改用明文，也不要把密钥写进文件。"
				return mustJSON(out), nil
			}
			out.Request.Available = true

			items, err := env.Secrets.Status(ctx, scope)
			if err != nil {
				return "", fmt.Errorf("读取凭证状态失败：%w", err)
			}
			for _, it := range items {
				if it.Name == name {
					out.Request.Configured = true
					out.Request.Fingerprint = it.Fingerprint
					break
				}
			}

			switch {
			case out.Request.Configured:
				out.State, out.Request.State = "configured", "configured"
				out.Next = fmt.Sprintf("凭证 %s 已存在（指纹 %s）：用户不需要做任何操作，"+
					"清单里直接写这个名字就能跑。不要提示用户录入，也不要把上游 401/403 归因为缺凭证"+
					"（那种情况先检查 endpoint、请求参数与凭证是否对这个上游有效）。", name, out.Request.Fingerprint)
			case env.CanWriteSecrets:
				out.State, out.Request.State = "missing", "missing"
				out.Next = "录入卡片已展示给用户。等用户填写并确认后再继续；" +
					"不要询问、不要复述任何明文，也不要把它写进文件或清单。"
			default:
				out.State, out.Request.State = "missing", "missing"
				out.Next = fmt.Sprintf("当前用户不是系统管理员，无法自己录入。把这条信息转给管理员即可："+
					"scope=%s、凭证名=%s，管理员在「密钥配置」录入后你再继续；期间不要索要明文。", scope, name)
			}
			return mustJSON(out), nil
		},
	}
}

// mustJSON 序列化固定结构，失败只可能是类型写错，按空对象降级。
func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{}`
	}
	return string(raw)
}
