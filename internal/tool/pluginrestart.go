package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/policy"
)

// pluginRestart 让「改完草稿」有一个显式的生效动作。
//
// 常驻进程里加载的是启动那一刻的模块：file_write 只改文件、只刷索引，进程里
// 还是旧代码。没有这个工具，作者只能去插件面板把开关关了再开一次；那个动作会
// 顺带把插件从工具列表与检索池里摘掉一轮，语义上也说不清「我只是想换个进程」。
func pluginRestart() Tool {
	return Tool{
		Name:       "plugin_restart",
		Capability: "plugin.restart",
		Description: "重启当前空间某个插件的常驻进程，让磁盘上的最新代码生效。" +
			"改完 plugins/<slug>/ 下的脚本、清单或配置后调用它；未声明 runtime.resident 的插件" +
			"没有常驻进程，调用会直接报错。重启会断开该插件的长连接（IM 机器人、订阅流）几秒，" +
			"正在进行的调用可能失败，不要在对客高峰反复调用。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug": {"type": "string", "description": "当前空间 files/plugins/ 下的插件目录名"}
  },
  "required": ["slug"],
  "additionalProperties": false
}`),
		// 重启不写文件、不改数据，但会打断进程与连接：按写操作计提，
		// 免得模型在一轮里反复重启同一个插件。
		Risk:           policy.RiskWrite,
		SideEffects:    policy.SideEffectsWorkspace,
		Runtime:        "builtin",
		MaxResultBytes: 2 << 10,
		Invoke:         invokePluginRestart,
	}
}

func invokePluginRestart(ctx context.Context, args json.RawMessage, env Env) (string, error) {
	if env.PluginRestart == nil {
		return "", fmt.Errorf("插件重启不可用：当前会话没有接上插件服务")
	}
	var in struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return "", fmt.Errorf("slug 不能为空")
	}
	if err := env.PluginRestart.Restart(ctx, env.UserID, env.SpaceID, slug); err != nil {
		return "", err
	}
	return fmt.Sprintf("已重启 %s 的常驻进程，新进程已按磁盘上的最新代码启动。", slug), nil
}
