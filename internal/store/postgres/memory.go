package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/workagent/auto-agent/internal/store"
)

// GetSessionSummary 读取会话的滚动摘要。
func (d *DB) GetSessionSummary(ctx context.Context, sessionID string) (store.SessionSummary, bool, error) {
	const q = `
select session_id::text, coalesce(upto_message_id::text, ''), content, message_count, model, updated_at
from session_summary
where session_id = $1::uuid`

	var s store.SessionSummary
	err := d.pool.QueryRow(ctx, q, sessionID).Scan(
		&s.SessionID, &s.UptoMessageID, &s.Content, &s.MessageCount, &s.Model, &s.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.SessionSummary{}, false, nil
		}
		return store.SessionSummary{}, false, err
	}
	return s, true, nil
}

// SaveSessionSummary 写入或更新滚动摘要。
//
// 水位只能前进：折叠可能并发（多副本、重试），后到的旧结果若把 upto_message_id
// 往回拨，摘要会原地打转——下一轮又从更早的位置重折一遍。
// 比较依据是消息时间而不是写入顺序，所以乱序提交也不会回退。
func (d *DB) SaveSessionSummary(ctx context.Context, s store.SessionSummary) error {
	const q = `
insert into session_summary (session_id, upto_message_id, content, message_count, model, updated_at)
values ($1::uuid, nullif($2, '')::uuid, $3, $4, $5, now())
on conflict (session_id) do update set
    upto_message_id = excluded.upto_message_id,
    content         = excluded.content,
    message_count   = excluded.message_count,
    model           = excluded.model,
    updated_at      = now()
where coalesce((select m.created_at from message m where m.id = session_summary.upto_message_id),
               'epoch'::timestamptz)
    < coalesce((select m.created_at from message m where m.id = excluded.upto_message_id),
               'epoch'::timestamptz)`

	_, err := d.pool.Exec(ctx, q, s.SessionID, s.UptoMessageID, s.Content, s.MessageCount, s.Model)
	return err
}

// SaveMessageEmbedding 保存消息向量；已存在则覆盖。
func (d *DB) SaveMessageEmbedding(ctx context.Context, r store.EmbeddingRecord) error {
	const q = `
insert into message_embedding (message_id, session_id, model, embedding)
values ($1::uuid, $2::uuid, $3, $4)
on conflict (message_id) do update set
    model     = excluded.model,
    embedding = excluded.embedding`

	_, err := d.pool.Exec(ctx, q, r.MessageID, r.SessionID, r.Model, r.Vector)
	return err
}

// ListMessageEmbeddings 返回会话内的消息向量，按时间倒序取最近 N 条。
func (d *DB) ListMessageEmbeddings(ctx context.Context, sessionID string, limit int) ([]store.EmbeddingRecord, error) {
	if limit <= 0 {
		limit = 2000
	}
	const q = `
select e.message_id::text, e.session_id::text, e.model, e.embedding, m.role, m.content
from message_embedding e
join message m on m.id = e.message_id
where e.session_id = $1::uuid
order by m.created_at desc
limit $2`

	rows, err := d.pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.EmbeddingRecord, 0, 64)
	for rows.Next() {
		var r store.EmbeddingRecord
		if err := rows.Scan(&r.MessageID, &r.SessionID, &r.Model, &r.Vector, &r.Role, &r.Content); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
