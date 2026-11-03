-- 上下文管理与记忆（技术方案 §5.7.2 摘要压缩、§5.7.4 长期记忆）
--
-- pgvector 说明：当前部署的 postgres 镜像不含 vector 扩展，向量先以 float4[] 存储，
-- 由应用侧计算余弦相似度；单会话十万级以内足够。后续换用 pgvector 镜像时，
-- 只需新增 vector 列与 ivfflat 索引，检索逻辑不变。

create table if not exists session_summary (
    session_id      uuid primary key references session (id) on delete cascade,
    upto_message_id uuid,
    content         text not null default '',
    message_count   int  not null default 0,
    model           text not null default '',
    updated_at      timestamptz not null default now()
);

create table if not exists message_embedding (
    message_id uuid primary key references message (id) on delete cascade,
    session_id uuid not null references session (id) on delete cascade,
    model      text not null default '',
    embedding  real[] not null,
    created_at timestamptz not null default now()
);

create index if not exists message_embedding_session_idx on message_embedding (session_id);
