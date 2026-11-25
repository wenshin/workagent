package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

// 摘要水位只能前进：乱序提交的旧结果不能把覆盖范围往回拨，
// 否则下一轮会从更早的位置重折一遍，摘要原地打转。
func TestSaveSessionSummaryKeepsWatermarkMonotonic(t *testing.T) {
	db, sessionID := newStreamingFixture(t, "summary")
	ctx := context.Background()

	ids := make([]string, 0, 3)
	for i := 1; i <= 3; i++ {
		var id string
		if err := db.pool.QueryRow(ctx,
			`insert into message (session_id, role, content, created_at)
			 values ($1::uuid, 'user', $2, $3::timestamptz) returning id::text`,
			sessionID, fmt.Sprintf("第 %d 条", i),
			fmt.Sprintf("2026-01-01 00:00:0%d+00", i)).Scan(&id); err != nil {
			t.Fatalf("建消息失败: %v", err)
		}
		ids = append(ids, id)
	}

	save := func(upto, content string, count int) {
		t.Helper()
		if err := db.SaveSessionSummary(ctx, store.SessionSummary{
			SessionID:     sessionID,
			UptoMessageID: upto,
			Content:       content,
			MessageCount:  count,
			Model:         "test",
		}); err != nil {
			t.Fatalf("写摘要失败: %v", err)
		}
	}

	save(ids[1], "覆盖到第 2 条", 2)

	// 回拨：更早的水位不该覆盖已有摘要
	save(ids[0], "覆盖到第 1 条", 1)
	got, ok, err := db.GetSessionSummary(ctx, sessionID)
	if err != nil || !ok {
		t.Fatalf("读摘要失败: ok=%v err=%v", ok, err)
	}
	if got.UptoMessageID != ids[1] || got.Content != "覆盖到第 2 条" {
		t.Fatalf("水位被回拨了: %+v", got)
	}

	// 前进：应当生效
	save(ids[2], "覆盖到第 3 条", 3)
	got, _, err = db.GetSessionSummary(ctx, sessionID)
	if err != nil {
		t.Fatalf("读摘要失败: %v", err)
	}
	if got.UptoMessageID != ids[2] || got.Content != "覆盖到第 3 条" || got.MessageCount != 3 {
		t.Fatalf("水位没有推进: %+v", got)
	}
}
