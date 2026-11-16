package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/policy"
	"github.com/workagent/auto-agent/internal/sandbox"
)

const (
	// Invoker 对单次工具调用的墙钟是 30s，这里必须明显短于它，
	// 否则命令还没到自己的超时就被外层掐断，报错会指向错误的层。
	defaultShellTimeout = 15 * time.Second
	maxShellTimeout     = 25 * time.Second
	// 命令输出比文件内容更碎，给 16KB；再多对模型也没有价值。
	shellMaxResultBytes = 16 * 1024
)

// localPathStore 是能把 key 解析成本机路径的存储实现（目前是 localfs）。
// 用可选接口而不是扩大 storage.Storage：将来换成 S3 时，只有这条能力消失，
// 其它工具完全不受影响。
type localPathStore interface {
	LocalPath(key string) (string, error)
}

// shellExec 在当前空间的文件目录里执行命令。
//
// 边界不靠提示词，靠 sandbox：全盘只读、只有空间 files/ 可写、默认无网络、
// 有超时和输出上限。命令失败（非零退出、超时）作为结果回给模型，
// 只有「沙箱不可用」「存储不可用」这类平台问题才返回 error。
func shellExec() Tool {
	return Tool{
		Name:       "shell_exec",
		Capability: "shell.exec",
		Description: "在当前空间的文件目录里执行 shell 命令并返回输出。命令运行在沙箱中：" +
			"只有当前空间目录可写，系统其它位置只读，默认没有网络访问，有超时与输出上限。" +
			"适合做文件统计、批量改名、跑本地脚本；要查外网信息请用 web_search。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "要执行的命令，由 sh -c 执行"},
    "timeout_seconds": {"type": "integer", "description": "超时秒数，默认 15，最大 25"}
  },
  "required": ["command"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 默认只有命中名单的命令才要确认，其余命令直接跑（沙箱已限定范围）。
		ShellApproval: DefaultApprovalCommands,
		SideEffects:   policy.SideEffectsWorkspace,
		Runtime:       "builtin",
		// 命令输出（尤其是测试日志）比文件内容更长，单独放宽回填额度
		MaxResultBytes: shellMaxResultBytes,
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			var in struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			command := strings.TrimSpace(in.Command)
			if command == "" {
				return "", fmt.Errorf("command 不能为空")
			}
			if env.Shell == nil {
				return "", fmt.Errorf("命令执行不可用：平台没有可用的沙箱后端")
			}
			if env.Storage == nil || env.SpaceID == "" {
				return "", fmt.Errorf("空间存储不可用")
			}

			local, ok := env.Storage.(localPathStore)
			if !ok {
				return "", fmt.Errorf("当前存储不支持命令执行（需要可直接访问的文件系统）")
			}
			spaceRoot, err := local.LocalPath(env.Storage.SpaceKey(env.SpaceID))
			if err != nil {
				return "", fmt.Errorf("解析空间目录失败: %w", err)
			}
			workDir := filepath.Join(spaceRoot, workspaceFilesDir)
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				return "", fmt.Errorf("创建空间文件目录失败: %w", err)
			}

			timeout := defaultShellTimeout
			if in.TimeoutSeconds > 0 {
				timeout = time.Duration(in.TimeoutSeconds) * time.Second
			}
			if timeout > maxShellTimeout {
				timeout = maxShellTimeout
			}

			res, runErr := env.Shell.Run(ctx, sandbox.Spec{
				Cmd:     []string{"sh", "-c", command},
				Dir:     workDir,
				Mode:    sandbox.ModeWorkspaceWrite,
				Timeout: timeout,
			})

			// 超时与输出超限是「命令没跑完」，属于结果的一部分，回给模型让它调整；
			// ctx 取消、启动失败这类才是平台错误。
			if runErr != nil &&
				!errors.Is(runErr, sandbox.ErrTimeout) &&
				!errors.Is(runErr, sandbox.ErrOutputTooLarge) {
				return "", runErr
			}
			return formatShellResult(res, runErr, timeout), nil
		},
	}
}

// formatShellResult 把执行结果整理成模型能直接读的文本。
// 先给结论（退出码 / 耗时 / 终止原因），再给输出，避免模型从一大段日志里找结论。
func formatShellResult(res sandbox.Result, runErr error, timeout time.Duration) string {
	var b strings.Builder
	switch {
	case errors.Is(runErr, sandbox.ErrTimeout):
		fmt.Fprintf(&b, "命令超时（%s），已终止整棵进程树\n", timeout)
	case errors.Is(runErr, sandbox.ErrOutputTooLarge):
		b.WriteString("命令输出超过上限，已终止\n")
	case runErr != nil:
		fmt.Fprintf(&b, "执行失败：%v\n", runErr)
	default:
		fmt.Fprintf(&b, "退出码 %d，耗时 %s\n", res.ExitCode, res.Duration.Round(time.Millisecond))
	}

	write := func(label, content string) {
		content = strings.TrimRight(content, "\n")
		if content == "" {
			return
		}
		fmt.Fprintf(&b, "\n[%s]\n%s\n", label, content)
	}
	write("stdout", res.Stdout)
	write("stderr", res.Stderr)
	if b.Len() > 0 && !strings.Contains(b.String(), "\n[") && runErr == nil {
		b.WriteString("（无输出）\n")
	}
	return b.String()
}
