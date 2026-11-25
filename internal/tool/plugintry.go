package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
)

// pluginTry 是「对话开发插件」的闭环工具：写完草稿立刻试跑一次，
// 把诊断（状态码、响应顶层键、命中条数、样例）带回给模型。
//
// 没有它，作者只能「发布 → 安装 → 发现取不到字段」，一轮反馈要跨好几个接口。
func pluginTry() Tool {
	return Tool{
		Name:       "plugin_try",
		Capability: "plugin.invoke",
		Description: "对当前空间的本地插件草稿做一次试跑，返回真实执行结果与诊断。" +
			"开发技能/工具/MCP/组件/智能体/搜索源时用它验证，不要靠猜。" +
			"搜索源（kind=source）必须传 query，用真实检索词；返回里的 top_keys 决定了 items 路径怎么写。" +
			"试跑不会发布、不会改动任何文件，但会真实出网或执行沙箱，可能有少量耗时。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug": {"type": "string", "description": "当前空间 files/plugins/ 下的插件目录名"},
    "query": {"type": "string", "description": "搜索源试跑用的检索词；其它类型可省略"},
    "input": {"type": "string", "description": "其它类型的试跑参数（JSON 字符串）；通常是 {\"args\": {...}} 或组件 props"}
  },
  "required": ["slug"],
  "additionalProperties": false
}`),
		// 试跑不写数据、不改文件，但会真实出网/执行沙箱：
		// 按「读外部数据」计提风险，不是 L0 的纯内存操作。
		Risk:           policy.RiskReadCost,
		SideEffects:    policy.SideEffectsReadOnly,
		Runtime:        "builtin",
		MaxResultBytes: 16 << 10,
		// 试跑的输入只有 slug 与 args，但结果取决于磁盘上的草稿：改了入口再用
		// 同样的参数重跑，回放会原样奉还上一次的回执（连耗时都一样），
		// 作者会以为「改了半天没生效」。开发循环必须真的执行。
		NoReplay: true,
		Invoke:   invokePluginTry,
	}
}

func invokePluginTry(ctx context.Context, args json.RawMessage, env Env) (string, error) {
	if env.PluginTry == nil {
		return "", fmt.Errorf("插件试跑不可用：当前会话没有接上插件服务")
	}
	var in struct {
		Slug  string `json:"slug"`
		Query string `json:"query"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return "", fmt.Errorf("slug 不能为空")
	}

	// 试跑参数统一收成一个 JSON：搜索源用 query，其它类型用 input 原样透传。
	payload := json.RawMessage(`{}`)
	if q := strings.TrimSpace(in.Query); q != "" {
		payload, _ = json.Marshal(map[string]string{"query": q})
	} else if s := strings.TrimSpace(in.Input); s != "" {
		if !json.Valid([]byte(s)) {
			return "", fmt.Errorf("input 必须是合法 JSON 字符串")
		}
		payload = json.RawMessage(s)
	}

	out, err := env.PluginTry.DryRunJSON(ctx, env.SpaceID, slug, env.UserID, payload)
	if err != nil {
		// 试跑失败时集群仍然有诊断价值：把回执一起带回去，让模型能看到 top_keys。
		if strings.TrimSpace(out) != "" {
			return fmt.Sprintf("试跑失败：%v\n\n%s", err, out), nil
		}
		return "", err
	}
	return out, nil
}
