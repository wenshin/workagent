package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
)

// pluginLogs 查插件最近的调用记录。
//
// 用户说「钉钉机器人怎么没反应」时，模型需要的是「上一次调成没成、错在哪」，
// 而不是再猜一遍插件是否启用——启停与缺凭证在调用时都会以错误形式直接出现，
// 真正看不到的是「那次调用到底发生了什么」。
func pluginLogs() Tool {
	return Tool{
		Name:       "plugin_logs",
		Capability: "plugin.logs",
		Description: "查看某个插件在当前空间最近的调用记录：时间、成功或失败、耗时、错误摘要。" +
			"用户问「插件没反应 / 报错了 / 刚才那条没发出去」时用它，不要靠猜。" +
			"只覆盖本次服务启动以来的调用（每个空间保留最近 200 次），重启后清空；不含插件自己打印的日志。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug": {"type": "string", "description": "插件目录名，取自 plugin_search 的 name 或清单里的 slug"},
    "limit": {"type": "integer", "description": "最多返回几条，默认 20，最大 100"}
  },
  "required": ["slug"],
  "additionalProperties": false
}`),
		// 读的是平台自己的记账，不出网、不碰插件进程。
		Risk:           policy.RiskReadOnly,
		SideEffects:    policy.SideEffectsReadOnly,
		Runtime:        "builtin",
		MaxResultBytes: 8 << 10,
		Invoke:         invokePluginLogs,
	}
}

func invokePluginLogs(ctx context.Context, args json.RawMessage, env Env) (string, error) {
	if env.PluginLogs == nil {
		return "", fmt.Errorf("插件调用记录不可用：当前会话没有接上插件服务")
	}
	var in struct {
		Slug  string `json:"slug"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return "", fmt.Errorf("slug 不能为空：先用 plugin_search 找到插件目录名")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	calls := env.PluginLogs.RecentCalls(ctx, env.SpaceID, slug, limit)
	if len(calls) == 0 {
		return fmt.Sprintf("没有「%s」的调用记录：可能还没被调用过，或者后端重启后记录已清空（每个空间只保留最近 200 次）。", slug), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "「%s」最近 %d 次调用（按时间倒序）：\n", slug, len(calls))
	for _, c := range calls {
		status := "成功"
		if !c.OK {
			status = "失败"
		}
		name := c.Tool
		if name == "" || name == slug {
			name = "插件调用"
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s\n",
			c.At.Format("01-02 15:04:05"), status, c.Duration.Round(time.Millisecond), name)
		if strings.TrimSpace(c.Err) == "" {
			continue
		}
		// 缩进两格让错误挂在上一条记录下面，而不是变成另一条记录。
		for _, line := range strings.Split(strings.TrimSpace(c.Err), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String(), nil
}
