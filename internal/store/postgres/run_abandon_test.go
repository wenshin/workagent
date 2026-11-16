package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/store"
)

// TestAbandonRunSettlesStreamingMessages 校验「没人跑的 run」收尾口径：
// run 置 failed，空占位删掉、有正文的标中断，已有终态的不被改写。
//
// 这条路径取代了「等心跳过期再清扫」的兜底：进程被 kill 后库里会留下一条
// 挂着 running 的执行，active-run 必须能当场把它收掉，否则前端每次刷新
// 都会空挂一条不会再来的流（先显示「思考中」再消失）。
func TestAbandonRunSettlesStreamingMessages(t *testing.T) {
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

	// 直接建最小依赖链（user → space → session），避免把上层服务拉进单测
	var userID, spaceID, sessionID string
	if err := db.pool.QueryRow(ctx,
		`insert into app_user (name) values ('收尾测试') returning id::text`).Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `delete from app_user where id = $1::uuid`, userID)
	})
	if err := db.pool.QueryRow(ctx,
		`insert into space (owner_id, name, type, path) values ($1::uuid, '收尾测试', 'personal', $2)
		 returning id::text`, userID, "itest-abandon-"+userID).Scan(&spaceID); err != nil {
		t.Fatalf("建空间失败: %v", err)
	}
	if err := db.pool.QueryRow(ctx,
		`insert into session (space_id, created_by) values ($1::uuid, $2::uuid) returning id::text`,
		spaceID, userID).Scan(&sessionID); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	// 一条空占位（正文还没开始）与一条已有正文的占位：收尾口径不同
	var emptyID, partialID string
	if err := db.pool.QueryRow(ctx,
		`insert into message (session_id, role, content, streaming) values ($1::uuid, 'assistant', '', true)
		 returning id::text`, sessionID).Scan(&emptyID); err != nil {
		t.Fatalf("写空占位失败: %v", err)
	}
	if err := db.pool.QueryRow(ctx,
		`insert into message (session_id, role, content, streaming) values ($1::uuid, 'assistant', '写了一半', true)
		 returning id::text`, sessionID).Scan(&partialID); err != nil {
		t.Fatalf("写正文占位失败: %v", err)
	}

	// 心跳 age 决定「有没有别处还在跑」，测试里直接把它写到想要的时刻
	runID := func(status, heartbeatAge string) string {
		var id string
		if err := db.pool.QueryRow(ctx,
			`insert into run (session_id, status, started_at, heartbeat_at)
			 values ($1::uuid, $2, now() - interval '1 hour', now() - $3::interval)
			 returning id::text`, sessionID, status, heartbeatAge).Scan(&id); err != nil {
			t.Fatalf("建 run(%s) 失败: %v", status, err)
		}
		return id
	}
	ghost := runID(store.RunRunning, "5 minutes")
	done := runID(store.RunSucceeded, "5 minutes")
	// 心跳还新鲜（另一个副本可能在跑它）：本进程无权替它收尾
	alive := runID(store.RunRunning, "0 seconds")

	if err := db.AbandonRun(ctx, ghost, sessionID, "测试收尾", 30*time.Second); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if err := db.AbandonRun(ctx, alive, sessionID, "测试收尾", 30*time.Second); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	stillAlive, err := db.GetRun(ctx, alive)
	if err != nil {
		t.Fatalf("读 run 失败: %v", err)
	}
	if stillAlive.Status != store.RunRunning {
		t.Fatalf("心跳新鲜的 run 不该被收尾，实际: %+v", stillAlive)
	}

	got, err := db.GetRun(ctx, ghost)
	if err != nil {
		t.Fatalf("读 run 失败: %v", err)
	}
	if got.Status != store.RunFailed || got.Error != "测试收尾" || got.EndedAt == nil {
		t.Fatalf("幽灵 run 应当置 failed 并落 ended_at，实际: %+v", got)
	}

	// 已经有终态的不该被改写：收尾只处理 running / waiting_approval
	kept, err := db.GetRun(ctx, done)
	if err != nil {
		t.Fatalf("读 run 失败: %v", err)
	}
	if kept.Status != store.RunSucceeded || kept.EndedAt != nil {
		t.Fatalf("已结束的 run 被改写了: %+v", kept)
	}

	msgs, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != partialID {
		t.Fatalf("空占位应当被删掉、只留下有正文的那条，实际 %d 条", len(msgs))
	}
	if msgs[0].Streaming || !msgs[0].Interrupted {
		t.Fatalf("有正文的占位应当标成中断，实际: streaming=%v interrupted=%v",
			msgs[0].Streaming, msgs[0].Interrupted)
	}
	if msgs[0].ID == emptyID {
		t.Fatal("空占位没有被删掉")
	}

	// 收尾没改到行（这一轮已经有人收过）时不能再碰消息：会话里新起的那一轮
	// 占位消息必须原样留着，否则会把正在进行的回答标成「中断」
	var freshID string
	if err := db.pool.QueryRow(ctx,
		`insert into message (session_id, role, content, streaming) values ($1::uuid, 'assistant', '', true)
		 returning id::text`, sessionID).Scan(&freshID); err != nil {
		t.Fatalf("写新一轮占位失败: %v", err)
	}
	if err := db.AbandonRun(ctx, done, sessionID, "重复收尾", 30*time.Second); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	after, err := db.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	seen := false
	for _, m := range after {
		if m.ID == freshID {
			seen = true
			if !m.Streaming || m.Interrupted {
				t.Fatalf("新一轮占位被误改: streaming=%v interrupted=%v", m.Streaming, m.Interrupted)
			}
		}
	}
	if !seen {
		t.Fatal("新一轮的占位消息被误删了")
	}
}
