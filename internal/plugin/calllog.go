package plugin

import (
	"context"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/spacelog"
	"github.com/workagent/auto-agent/internal/tool"
)

// callLogPerSpace 是每个空间保留的调用记录条数。
//
// 按空间分组而不是全局共享：一个空间刷满不该把另一个空间的排障依据挤掉，
// 「最近那次为什么失败」永远是按空间问的。
const callLogPerSpace = 200

// callLogMaxErr 是错误摘要上限：长栈对排障没有帮助，只会把工具回执撑爆。
const callLogMaxErr = 400

// callEntry 是一条调用记录。用独立类型而不是直接存 tool.PluginCall，
// 是为了在写入时就截断错误，避免把一整段堆栈留在内存里。
type callEntry struct {
	at       time.Time
	spaceID  string
	slug     string
	toolName string
	ok       bool
	dur      time.Duration
	err      string
}

// callLog 是进程内的插件调用记录，环形覆盖。
//
// 不落库：它解决的是「刚才那次为什么失败」，而不是合规审计（审计另有
// tool_call / audit_log，按 run 可查）。与常驻进程池、审批一样只活在当前进程，
// 重启即清空——这正好符合「最近发生了什么」的用途，也不需要为它扛持久化成本。
type callLog struct {
	mu      sync.RWMutex
	size    int
	bySpace map[string][]callEntry
}

func newCallLog(size int) *callLog {
	if size <= 0 {
		size = callLogPerSpace
	}
	return &callLog{size: size, bySpace: map[string][]callEntry{}}
}

func (l *callLog) add(e callEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.err != "" {
		e.err = clipText(e.err, callLogMaxErr)
	}
	buf := append(l.bySpace[e.spaceID], e)
	if len(buf) > l.size {
		// 复制一份再切：直接 buf[1:] 会让底层数组一直留着旧记录。
		buf = append([]callEntry(nil), buf[len(buf)-l.size:]...)
	}
	l.bySpace[e.spaceID] = buf
}

// recent 返回按时间倒序的调用记录。过滤条件在读取时做，写入侧不做判断：
// 查询通常只看一个插件，没必要为每条记录维护一份索引。
func (l *callLog) recent(spaceID, slug string, limit int) []tool.PluginCall {
	if limit <= 0 {
		limit = 20
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	buf := l.bySpace[spaceID]
	out := make([]tool.PluginCall, 0, limit)
	for i := len(buf) - 1; i >= 0 && len(out) < limit; i-- {
		e := buf[i]
		if slug != "" && e.slug != slug {
			continue
		}
		out = append(out, tool.PluginCall{
			At:       e.at,
			Tool:     e.toolName,
			OK:       e.ok,
			Duration: e.dur,
			Err:      e.err,
		})
	}
	return out
}

// recordCall 记一次调用结果：内存里给 plugin_logs 立即可查，磁盘上给事后复核。
//
// 两份都要写，缺一不可：内存那份会随重启消失、且只留摘要；磁盘那份（files/.logs）
// 按天归档，用于回答「昨天下午那次为什么失败」，也免得排障只能靠翻服务日志。
// 空间取调用方会话的空间，而不是插件所属空间：借用来的插件执行落在使用方，
// 「刚才那次为什么失败」也应该在使用方查得到。
func (m *Manager) recordCall(ctx context.Context, env tool.Env, slug, toolName string, start time.Time, err error) {
	spaceID := env.SpaceID
	if m.calls != nil {
		e := callEntry{
			at:       time.Now(),
			spaceID:  spaceID,
			slug:     slug,
			toolName: toolName,
			ok:       err == nil,
			dur:      time.Since(start),
		}
		if err != nil {
			e.err = err.Error()
		}
		m.calls.add(e)
	}

	if m.o.SpaceLog == nil {
		return
	}
	status, msg := "ok", ""
	if err != nil {
		status, msg = "error", err.Error()
	}
	m.o.SpaceLog.Write(ctx, spacelog.Entry{
		SpaceID:   spaceID,
		Kind:      "plugin",
		Tool:      slug,
		Status:    status,
		Latency:   time.Since(start),
		SessionID: env.SessionID,
		RunID:     env.RunID,
		UserID:    env.UserID,
		Output:    msg,
	})
}

// RecentCalls 实现 tool.PluginLogs。
func (m *Manager) RecentCalls(_ context.Context, spaceID, slug string, limit int) []tool.PluginCall {
	if m.calls == nil {
		return nil
	}
	return m.calls.recent(spaceID, slug, limit)
}
