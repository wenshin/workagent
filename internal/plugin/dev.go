package plugin

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/tool"
)

const (
	devExecDefaultTimeout = 60 * time.Second
	devExecMaxTimeout     = 5 * time.Minute
)

// DevExec 在插件草稿目录里执行一条命令。
//
// 这是「插件开发执行体」的落地形态：与空间 shell 共用同一套沙箱规则，
// 只是根目录换成草稿目录——
//   - 工作区 = 草稿目录（持久，改一次跑一次）
//   - 依赖   = 草稿目录/.pkg（npm 全局、venv，重建后还在）
//   - 系统   = 草稿目录/.root（overlay 可写层，只有这个空间看得见）
//
// 与空间 shell 的唯一差别是**不做审批**：命令是开发者在界面上敲的，
// 不是模型自动决策。联网范围仍由 NetworkPolicy 兜底，不受这里影响。
func (m *Manager) DevExec(ctx context.Context, spaceID, slug, userID, command string, timeout time.Duration) (sandbox.Result, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return sandbox.Result{}, fmt.Errorf("命令为空")
	}
	if m.o.Runner == nil {
		return sandbox.Result{}, fmt.Errorf("当前环境没有可用的沙箱执行器")
	}
	lp, ok := m.o.Storage.(interface{ LocalPath(string) (string, error) })
	if !ok {
		return sandbox.Result{}, fmt.Errorf("当前存储不支持在草稿目录执行（需要本地文件系统）")
	}
	dir, err := lp.LocalPath(DraftKey(m.o.Storage, spaceID, slug))
	if err != nil {
		return sandbox.Result{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return sandbox.Result{}, fmt.Errorf("草稿 %s 不存在，先创建草稿再执行命令", slug)
	}
	if err := tool.CheckSpaceQuota(dir); err != nil {
		return sandbox.Result{}, err
	}
	if timeout <= 0 {
		timeout = devExecDefaultTimeout
	}
	if timeout > devExecMaxTimeout {
		timeout = devExecMaxTimeout
	}

	// 系统安装与空间 shell 用同一个判据：只有 apt/dpkg 需要放开系统目录。
	mode := sandbox.ModeWorkspaceWrite
	if tool.NeedsSystemWrite(command) {
		mode = sandbox.ModeSystemWrite
	}
	// 开发会话总是给网：装依赖、deno cache、拉示例代码都要。
	// 超时/输出超限本身是结果的一部分，把 res 一起返回让调用方展示。
	// 草稿目录挂到 /workspace：开发会话里的 pwd、报错与装包日志都不该出现
	// 控制面的存储路径（spaces/<id>/drafts/<slug>）。
	return m.o.Runner.Run(ctx, sandbox.Spec{
		// 开发会话跑在租户的插件容器里：装进去的依赖不影响任何空间容器。
		Scope: sandbox.Scope{Space: spaceID, Kind: sandbox.KindPlugin},
		Cmd:   []string{"sh", "-c", command},
		Dir:   sandbox.WorkspaceDir,
		Mode:  mode,
		Mounts: []sandbox.Mount{
			{Source: dir, Key: DraftKey(m.o.Storage, spaceID, slug), Target: sandbox.WorkspaceDir, Write: true},
			{Source: tool.CacheRoot(), Root: sandbox.RootCache, Target: sandbox.CacheDir, Write: true},
		},
		Timeout: timeout,
		Network: true,
		Env:     tool.InstallEnv(),
	})
}
