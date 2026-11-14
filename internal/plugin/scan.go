package plugin

import (
	"context"
	"encoding/json"

	"github.com/workagent/auto-agent/internal/securityscan"
	"github.com/workagent/auto-agent/internal/store"
)

// ScanBlockedError 表示发布被安全扫描拒绝。
//
// 它 wrap ErrConflict，HTTP 层因此返回 409；同时把完整报告带出去，
// 发布者能看到「哪条规则、哪个文件、第几行」，而不是一句笼统的失败。
type ScanBlockedError struct {
	Report securityscan.Report
}

func (e *ScanBlockedError) Error() string {
	if e.Report.BlockedReason != "" {
		return e.Report.BlockedReason
	}
	return "发布被安全扫描阻断"
}

func (e *ScanBlockedError) Unwrap() error { return ErrConflict }

// scanDraft 在发布前扫描草稿包。
//
// 未配置扫描器时直接放行：扫描是闸门，不是发布的依赖——没配就不拦，
// 但不能假装扫过（报告状态会是空，不会写成 passed）。
func (m *Manager) scanDraft(ctx context.Context, b *Bundle, version string) (securityscan.Report, bool) {
	if m.o.Scanner == nil {
		return securityscan.Report{}, false
	}
	// 每次发布前检查一次规则文件：改了规则，下一次发布立即生效，
	// 不需要重启服务，也不需要额外的后台 goroutine。
	m.o.Scanner.Reload()
	rep := m.o.Scanner.Scan(ctx, securityscan.Input{
		Slug:    b.Meta.Slug,
		Version: version,
		Files:   b.Files,
	})
	return rep, rep.Blocked()
}

// saveScan 把扫描结论落库（元数据，失败只告警，不影响已完成的发布）。
func (m *Manager) saveScan(ctx context.Context, pluginID, sha string, rep securityscan.Report, userID string) {
	if m.o.Scanner == nil || pluginID == "" {
		return
	}
	findings, err := json.Marshal(rep.Findings)
	if err != nil {
		m.o.Logger.Warn("序列化扫描结论失败", "plugin", pluginID, "err", err)
		return
	}
	if findings == nil {
		findings = []byte("[]")
	}
	if _, err := m.o.Store.SavePluginScan(ctx, store.PluginScan{
		PluginID:      pluginID,
		Version:       rep.Version,
		SHA256:        sha,
		RulesVersion:  rep.RulesVersion,
		Scanner:       "builtin",
		Status:        rep.Status,
		MaxSeverity:   string(rep.MaxSeverity),
		Deps:          rep.Deps,
		Lockfile:      rep.Lockfile,
		Findings:      findings,
		BlockedReason: rep.BlockedReason,
		DepError:      rep.DepError,
		ScannedBy:     userID,
	}); err != nil {
		m.o.Logger.Warn("保存扫描结论失败", "plugin", pluginID, "err", err)
	}
}
