package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/store"
)

// mcpAppSpec 是给前端渲染卡片用的描述，不含任何密钥值。
//
// 卡片自己调用 POST /spaces/{id}/mcp-apps/{app}/key 拿明文：那条响应只到浏览器，
// 工具回执只有状态。这样密钥既没进模型上下文，也没进消息表与 run_step。
type mcpAppSpec struct {
	App string `json:"app"`
	// State 与回执 state 同义，卡片按它决定「显示输入框 / 直接取回 / 提示找创建人」
	State     string `json:"state"`
	OwnerName string `json:"owner_name,omitempty"`
	Available bool   `json:"available"`
	// Hint 是调用方给的一句话用途，显示在卡片上
	Hint string `json:"hint,omitempty"`
}

// mcpAppResult 的字段顺序同样是刻意的：结论与下一步在最前面。
type mcpAppResult struct {
	State  string     `json:"state"`
	Next   string     `json:"next"`
	MCPApp mcpAppSpec `json:"mcp_app"`
}

// mcpAppKey 让用户在会话界面上确定 MCP 应用名并拿到它的 JWT 签名密钥。
//
// 应用名是全局唯一的，一把密钥对应一个应用名；谁先创建谁就是创建者，
// 只有创建者能在卡片里取回明文。所以工具只负责「把卡片摆出来」和
// 「告诉模型现在是什么状态」，密钥本身永远不经过模型。
func mcpAppKey() Tool {
	return Tool{
		Name:       "mcp_app_key",
		Capability: "system.mcp.app.key",
		Description: "确定 MCP 插件的应用名（auth.app），并让创建者在界面上拿到对应的 JWT 签名密钥。" +
			"创建 MCP 插件（auth.mode=jwt）之前先调用它，不要在没确认应用名时直接写清单；" +
			"用户想把某个应用的密钥找回来时也调用它，把应用名放进 app 参数。" +
			"回执只有状态（new / owned / taken / unowned / disabled）和创建者姓名，密钥不会进入你的上下文：" +
			"卡片会展示给用户。state=taken 说明应用名已被别人创建，让用户联系创建者，不要换名字重试、也不要自己生成密钥。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "app": {"type": "string", "description": "应用名（1-64 位字母数字下划线短横线）。用户还没定就留空，卡片会让用户填"},
    "purpose": {"type": "string", "description": "一句话用途，显示在卡片上，如「CRM 工具联调用」"}
  },
  "additionalProperties": false
}`),
		Risk:        policy.RiskReadOnly,
		SideEffects: policy.SideEffectsReadOnly,
		Runtime:     RuntimeBuiltin,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			var in struct {
				App     string `json:"app"`
				Purpose string `json:"purpose"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}
			app := strings.TrimSpace(in.App)
			out := mcpAppResult{MCPApp: mcpAppSpec{
				App:  app,
				Hint: strings.TrimSpace(in.Purpose),
			}}

			if env.Secrets == nil || !env.Secrets.Enabled() {
				out.State, out.MCPApp.State = "disabled", "disabled"
				out.Next = "凭证存储未启用（部署时没配置 SECRET_KEK），生成不了签名密钥。" +
					"如实告诉用户这件事，不要改用明文密钥，也不要把密钥写进清单或文件。"
				return mustJSON(out), nil
			}
			out.MCPApp.Available = true

			if app == "" {
				out.State, out.MCPApp.State = "needs_app", "needs_app"
				out.Next = "卡片已展示给用户。等用户填好应用名并确认后再继续；" +
					"不要自己编一个应用名，也不要在名字确定前把 auth.app 写进清单。"
				return mustJSON(out), nil
			}
			if !secretNameRe.MatchString(app) {
				return "", fmt.Errorf("应用名只能是字母、数字、下划线与短横线（1-64 位）：%q", app)
			}

			// 归属判定与卡片接口共用同一张表，两边结论必须一致；
			// 这里只读，真正生成密钥的动作在卡片里（那一步才需要用户确认）。
			rec, err := env.Store.GetMCPApp(ctx, app)
			switch {
			case err == nil && rec.OwnerUserID == env.UserID:
				out.State, out.MCPApp.State = "owned", "owned"
				out.Next = "应用 " + app + " 已存在且由当前用户创建：卡片会取回密钥并展示给用户复制。" +
					"密钥不进你的上下文，后续清单里只写 auth.app=" + app + "，不要复述密钥。"
			case err == nil:
				owner := displayName(ctx, env, rec.OwnerUserID)
				out.MCPApp.OwnerName = owner
				out.State, out.MCPApp.State = "taken", "taken"
				out.Next = fmt.Sprintf("应用 %s 已被 %s 创建，只有创建者能拿到密钥：让用户联系他取，"+
					"或者换一个明确属于自己的应用名（由用户在卡片里重新输入）。不要尝试生成同名密钥。",
					app, orUnknown(owner))
			case errors.Is(err, store.ErrNotFound):
				// 有密钥但没归属：这条流程之前生成的（历史发布件），不能认领
				if v, kerr := env.Secrets.Get(ctx, "mcp_jwt", app); kerr == nil && v != "" {
					out.State, out.MCPApp.State = "unowned", "unowned"
					out.Next = "应用 " + app + " 已在使用，但没有创建人记录（历史数据）：" +
						"让用户找系统管理员，不要自己认领，也不要新生成一把同名密钥。"
					break
				}
				out.State, out.MCPApp.State = "new", "new"
				out.Next = "应用名 " + app + " 可用：用户在卡片上确认后即生成密钥并展示给他复制，" +
					"之后清单里写 auth.app=" + app + "，同一把密钥一直有效（发布前后不变）。"
			default:
				return "", fmt.Errorf("查询应用归属失败：%w", err)
			}
			return mustJSON(out), nil
		},
	}
}

func displayName(ctx context.Context, env Env, userID string) string {
	u, err := env.Store.GetUser(ctx, userID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Name)
}

func orUnknown(name string) string {
	if name == "" {
		return "其他成员"
	}
	return name
}
