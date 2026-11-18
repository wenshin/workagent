package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
		// 卡片徽标按命令判：`ls` 不应该和 `rm -rf` 显示成同一档。
		RiskFor: shellRisk,
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

// shellReadOnlyCommands 是「静态可判定为只读」的命令名单，只用于展示。
//
// 口径故意窄：白名单外的命令（解释器 python/node/sh、mv/cp/tee/sed/awk、
// mysql/psql 这类客户端）一律按写入展示——把 `mv` 显示成「只读」比把
// `echo hi > f` 显示成「写入」糟糕得多。
var shellReadOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "fd": true,
	"find": true, "du": true, "df": true, "stat": true, "file": true,
	"pwd": true, "echo": true, "printf": true, "date": true, "uname": true,
	"whoami": true, "id": true, "which": true, "type": true, "tree": true,
	"diff": true, "cmp": true, "cut": true, "tr": true, "nl": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true, "column": true,
	"od": true, "hexdump": true, "jq": true, "sort": true,
}

// shellGitReadOnly 是 git 的只读子命令；`git commit` / `push` / `checkout`
// 这些会改动仓库或远端，按写入展示。
var shellGitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true,
	"rev-parse": true, "ls-files": true, "ls-tree": true, "describe": true,
	"shortlog": true, "cat-file": true, "grep": true,
}

var (
	// shellDevNull 是「写 /dev/null」的重定向：不落地成文件，不算写操作。
	shellDevNull = regexp.MustCompile(`(?:[0-9]|&)?>>?\s*/dev/null\b`)
	// 这两个参数会把白名单里的读命令变成写：`find -delete/-exec`、`sort -o`。
	shellFindWrites = regexp.MustCompile(`\s-(delete|exec|execdir|ok|okdir)\b`)
	shellSortWrites = regexp.MustCompile(`\s(--output|-o)\b`)
)

// shellRisk 按命令给出这一次调用的展示等级。
//
// 判据与审批同源（复用 MatchApprovalCommand 的名单与动态形态），但**只影响
// 徽标**：审批该弹还是弹，空间写权限也仍按声明的 L3 判。返回 0 表示判不出来，
// 由 Tool.EffectiveRisk 退回声明等级。
func shellRisk(t Tool, args json.RawMessage) int {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return 0
	}
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return 0
	}
	// 重定向写文件、命令替换、xargs、嵌套 shell 一律按写入：这些形态静态判不准。
	if shellWritesByRedirect(command) {
		return policy.RiskWrite
	}
	if hit := MatchApprovalCommand(command, t.ShellApproval); hit != "" {
		return policy.RiskWrite
	}
	segments := shellSegments(command)
	if len(segments) == 0 {
		return 0
	}
	for _, seg := range segments {
		if !shellSegmentReadOnly(seg) {
			return policy.RiskWrite
		}
	}
	return policy.RiskReadOnly
}

// shellWritesByRedirect 判断命令里是否有落地成文件的重定向（`>` / `>>`）。
//
// `>/dev/null` 与 `>&1` / `>&2` 不算：前者没有数据落地，后者写的是已打开的句柄。
func shellWritesByRedirect(command string) bool {
	rest := shellDevNull.ReplaceAllString(command, " ")
	for i := 0; i < len(rest); i++ {
		if rest[i] != '>' {
			continue
		}
		j := i + 1
		if j < len(rest) && rest[j] == '>' {
			j++
		}
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		if j < len(rest) && rest[j] == '&' {
			i = j
			continue
		}
		return true
	}
	return false
}

// shellSegments 按管道 / 顺序分隔符切段（与审批判定同一口径）。
func shellSegments(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, seg := range fields {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// shellSegmentReadOnly 判断单段命令是不是纯读：命令名在只读名单里，
// 且没有把读变成写的参数（`find -exec`、`sort -o`）。
func shellSegmentReadOnly(seg string) bool {
	fields := strings.Fields(seg)
	for i := 0; i < len(fields); i++ {
		token := strings.Trim(fields[i], `"'()`)
		if token == "" {
			continue
		}
		// VAR=value 前缀与 wrapper 不是命令名，继续往后找（与审批判定同一口径）
		if idx := strings.IndexByte(token, '='); idx > 0 && !strings.ContainsAny(token[:idx], "/.-") {
			continue
		}
		if shellWrappers[token] {
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		name := baseName(token)
		if name == "git" {
			sub := shellFirstArg(fields, i+1)
			// 只有选项、没有子命令（`git --version`）不会改任何东西
			return sub == "" || shellGitReadOnly[sub]
		}
		if !shellReadOnlyCommands[name] {
			return false
		}
		switch name {
		case "find":
			return !shellFindWrites.MatchString(seg)
		case "sort":
			return !shellSortWrites.MatchString(seg)
		}
		return true
	}
	return false
}

// shellFirstArg 取命令名之后的第一个非选项参数（`git status` 的 `status`）。
// 取不到就是判不出来，按非只读处理。
func shellFirstArg(fields []string, from int) string {
	for i := from; i < len(fields); i++ {
		token := strings.Trim(fields[i], `"'()`)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		return token
	}
	return ""
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
