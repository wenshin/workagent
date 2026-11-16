package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/store"
)

// newStreamingFixture 建一条最小依赖链（user → space → session），返回库、会话 id 与清理函数。
func newStreamingFixture(t *testing.T, tag string) (*DB, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 DATABASE_URL，跳过集成测试")
	}
	ctx := context.Background()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(db.Close)

	var userID, spaceID, sessionID string
	if err := db.pool.QueryRow(ctx,
		`insert into app_user (dingtalk_union_id, name) values ($1, $2) returning id::text`,
		"itest-"+tag+"-"+time.Now().Format("150405.000000"), "占位结算"+tag).Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `delete from app_user where id = $1::uuid`, userID)
	})
	if err := db.pool.QueryRow(ctx,
		`insert into space (owner_id, name, type, path) values ($1::uuid, $2, 'personal', $3)
		 returning id::text`, userID, "占位结算"+tag, "itest-stream-"+tag+"-"+userID).Scan(&spaceID); err != nil {
		t.Fatalf("建空间失败: %v", err)
	}
	if err := db.pool.QueryRow(ctx,
		`insert into session (space_id, created_by) values ($1::uuid, $2::uuid) returning id::text`,
		spaceID, userID).Scan(&sessionID); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return db, sessionID
}

// stepPayload 是测试里造 run_step 用的形状，与 agent.ToolState.Payload 一致。
func stepPayload(callID, tool, args, summary, status string, durationMS int64) []byte {
	p := map[string]any{
		"call_id": callID, "tool": tool, "args": args, "status": status, "risk": 1,
	}
	if summary != "" {
		p["summary"] = summary
	}
	if durationMS > 0 {
		p["duration_ms"] = durationMS
	}
	data, _ := json.Marshal(p)
	return data
}

func appendStep(t *testing.T, db *DB, runID string, seq int, typ string, payload []byte) {
	t.Helper()
	if _, err := db.AppendRunStep(context.Background(), store.RunStep{
		RunID: runID, Seq: seq, Type: typ, Payload: payload,
	}); err != nil {
		t.Fatalf("写 run_step 失败: %v", err)
	}
}

type partView struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Result     string `json:"result"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

// TestInterruptStreamingMessageKeepsToolTrace 锁定「续跑接手一条中断的 run」时的口径：
// 占位消息不能只被删掉或一直挂着 streaming，而要把 run_step 里已发生的工具调用
// 还原进 parts 并标 interrupted——用户点「继续」之后仍能看到中断前做过什么。
func TestInterruptStreamingMessageKeepsToolTrace(t *testing.T) {
	db, sessionID := newStreamingFixture(t, "resume")
	ctx := context.Background()

	var runID string
	if err := db.pool.QueryRow(ctx,
		`insert into run (session_id, status, ended_at) values ($1::uuid, 'failed', now())
		 returning id::text`, sessionID).Scan(&runID); err != nil {
		t.Fatalf("建 run 失败: %v", err)
	}
	var placeID string
	if err := db.pool.QueryRow(ctx,
		`insert into message (session_id, role, content, streaming) values ($1::uuid, 'assistant', '', true)
		 returning id::text`, sessionID).Scan(&placeID); err != nil {
		t.Fatalf("写占位失败: %v", err)
	}

	// 一次有回执的调用 + 一次还没拿到回执就中断的调用
	appendStep(t, db, runID, 1, "tool.call", stepPayload("c1", "wiki_search", `{"query":"GMV"}`, "", "running", 0))
	appendStep(t, db, runID, 2, "tool.result", stepPayload("c1", "wiki_search", `{"query":"GMV"}`, "命中已确认模型", "ok", 12))
	appendStep(t, db, runID, 3, "tool.call", stepPayload("c2", "mcp_call", `{}`, "", "running", 0))

	if err := db.InterruptStreamingMessage(ctx, runID, sessionID); err != nil {
		t.Fatalf("结算占位失败: %v", err)
	}

	msgs, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != placeID {
		t.Fatalf("有工具轨迹的占位应当保留，实际 %d 条", len(msgs))
	}
	if msgs[0].Streaming || !msgs[0].Interrupted {
		t.Fatalf("应当标成中断，实际 streaming=%v interrupted=%v", msgs[0].Streaming, msgs[0].Interrupted)
	}

	var parts []partView
	if err := json.Unmarshal(msgs[0].Parts, &parts); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("应当还原 2 次工具调用，实际 %d", len(parts))
	}
	if parts[0].ID != "c1" || parts[0].Status != "ok" || parts[0].Result != "命中已确认模型" || parts[0].DurationMS != 12 {
		t.Fatalf("有回执的调用还原不对: %+v", parts[0])
	}
	if parts[1].ID != "c2" || parts[1].Status != "error" || parts[1].Result == "" {
		t.Fatalf("没有回执的调用应当标成中断，实际: %+v", parts[1])
	}

	// 重复结算幂等：已经不再是 streaming，不会再被改
	if err := db.InterruptStreamingMessage(ctx, runID, sessionID); err != nil {
		t.Fatalf("重复结算失败: %v", err)
	}
	again, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if len(again) != 1 || !again[0].Interrupted {
		t.Fatalf("重复结算改动了已结算的消息: %+v", again)
	}
}

// TestSweepStaleRunsHealsOrphanPlaceholder 锁定启动自愈：上一轮已经终态（被续跑接替、
// 或被更早的清扫收掉）却还挂着 streaming 的占位，没有任何路径会再碰到它；
// 启动清扫要按「占位之前启动的那条 run」把工具轨迹补回去。
func TestSweepStaleRunsHealsOrphanPlaceholder(t *testing.T) {
	db, sessionID := newStreamingFixture(t, "orphan")
	ctx := context.Background()

	// 中断那一轮：已经被收尾（failed），比占位建得早
	var interruptedID string
	if err := db.pool.QueryRow(ctx,
		`insert into run (session_id, status, started_at, ended_at)
		 values ($1::uuid, 'failed', now() - interval '10 minutes', now() - interval '9 minutes')
		 returning id::text`, sessionID).Scan(&interruptedID); err != nil {
		t.Fatalf("建中断 run 失败: %v", err)
	}
	var placeID string
	if err := db.pool.QueryRow(ctx,
		`insert into message (session_id, role, content, streaming, created_at)
		 values ($1::uuid, 'assistant', '', true, now() - interval '5 minutes')
		 returning id::text`, sessionID).Scan(&placeID); err != nil {
		t.Fatalf("写占位失败: %v", err)
	}
	appendStep(t, db, interruptedID, 1, "tool.call", stepPayload("c1", "file_read", `{"path":"a.md"}`, "", "running", 0))
	appendStep(t, db, interruptedID, 2, "tool.result", stepPayload("c1", "file_read", `{"path":"a.md"}`, "已读取", "ok", 5))

	// 之后用户点了「继续」：新的 run 起来、正常结束，但没有自己的工具步骤
	var resumedID string
	if err := db.pool.QueryRow(ctx,
		`insert into run (session_id, status, started_at, ended_at)
		 values ($1::uuid, 'succeeded', now() - interval '1 minute', now() - interval '30 seconds')
		 returning id::text`, sessionID).Scan(&resumedID); err != nil {
		t.Fatalf("建续跑 run 失败: %v", err)
	}

	// 续跑那一轮没有工具步骤，还原时必须退回中断那一轮
	if steps, err := db.ListRunSteps(ctx, resumedID); err != nil || len(steps) != 0 {
		t.Fatalf("续跑 run 不该有步骤: n=%d err=%v", len(steps), err)
	}

	// staleAfter 取很长：这条用例只关心孤儿占位的自愈，不去收尾库里别的执行。
	// 注意 SweepStaleRuns 是全局自愈，会顺手结算库里其它会话的孤儿占位（幂等，
	// 只碰「没有任何活 run」的会话）；跑在开发库上等于把历史遗留的脏占位一起清了。
	if _, err := db.SweepStaleRuns(ctx, 24*time.Hour, "测试清扫"); err != nil {
		t.Fatalf("清扫失败: %v", err)
	}

	msgs, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != placeID {
		t.Fatalf("孤儿占位应当补上轨迹而不是删掉，实际 %d 条", len(msgs))
	}
	if msgs[0].Streaming {
		t.Fatal("孤儿占位还在 streaming")
	}
	var parts []partView
	if err := json.Unmarshal(msgs[0].Parts, &parts); err != nil {
		t.Fatalf("解析 parts 失败: %v", err)
	}
	if len(parts) != 1 || parts[0].ID != "c1" || parts[0].Result != "已读取" {
		t.Fatalf("应当用中断那一轮的轨迹还原，实际: %+v", parts)
	}
}
