package postgres

import (
	"context"

	"github.com/workagent/auto-agent/internal/store"
)

// UpsertCapability 注册能力接口；同名则更新 Schema 与风险等级。
func (d *DB) UpsertCapability(ctx context.Context, c store.Capability) error {
	const q = `
insert into capability (name, description, input_schema, risk_level, idempotent, side_effects)
values ($1, $2, $3::jsonb, $4, $5, $6)
on conflict (name) do update set
    description  = excluded.description,
    input_schema = excluded.input_schema,
    risk_level   = excluded.risk_level,
    idempotent   = excluded.idempotent,
    side_effects = excluded.side_effects`

	schema := c.InputSchema
	if len(schema) == 0 {
		schema = []byte(`{"type":"object","properties":{}}`)
	}
	_, err := d.pool.Exec(ctx, q, c.Name, c.Description, schema, c.RiskLevel, c.Idempotent, c.SideEffects)
	return err
}

// UpsertTool 注册能力的实现；同名则更新风险等级与运行时。
func (d *DB) UpsertTool(ctx context.Context, t store.ToolRecord) error {
	const q = `
insert into tool (capability_id, name, runtime, risk_level, mcp_server_id)
select c.id, $2, $3, $4, nullif($5, '')::uuid
from capability c
where c.name = $1
on conflict (name) do update set
    runtime     = excluded.runtime,
    risk_level  = excluded.risk_level,
    updated_at  = now()`

	_, err := d.pool.Exec(ctx, q, t.Capability, t.Name, t.Runtime, t.RiskLevel, t.MCPServerID)
	return err
}

// ListMCPServers 返回全部注册的 MCP Server。
func (d *DB) ListMCPServers(ctx context.Context) ([]store.MCPServer, error) {
	const q = `
select id::text, name, transport, endpoint, auth_header, secret_ref,
       visibility, health, coalesce(tools_cache, '[]'::jsonb), last_sync_at
from mcp_server
order by name`

	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.MCPServer, 0, 4)
	for rows.Next() {
		var s store.MCPServer
		if err := rows.Scan(&s.ID, &s.Name, &s.Transport, &s.Endpoint, &s.AuthHeader,
			&s.SecretRef, &s.Visibility, &s.Health, &s.ToolsCache, &s.LastSyncAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpsertMCPServer 按名称注册或更新 MCP Server。
func (d *DB) UpsertMCPServer(ctx context.Context, s store.MCPServer) (store.MCPServer, error) {
	const q = `
insert into mcp_server (name, transport, endpoint, auth_header, secret_ref, visibility)
values ($1, $2, $3, $4, $5, $6)
on conflict (name) do update set
    transport   = excluded.transport,
    endpoint    = excluded.endpoint,
    auth_header = excluded.auth_header,
    secret_ref  = excluded.secret_ref,
    visibility  = excluded.visibility,
    updated_at  = now()
returning id::text, name, transport, endpoint, auth_header, secret_ref, visibility, health`

	var out store.MCPServer
	if err := d.pool.QueryRow(ctx, q, s.Name, s.Transport, s.Endpoint, s.AuthHeader, s.SecretRef, s.Visibility).Scan(
		&out.ID, &out.Name, &out.Transport, &out.Endpoint, &out.AuthHeader, &out.SecretRef, &out.Visibility, &out.Health,
	); err != nil {
		return store.MCPServer{}, err
	}
	return out, nil
}

// UpdateMCPToolsCache 保存工具清单缓存与健康状态。
func (d *DB) UpdateMCPToolsCache(ctx context.Context, id string, cache []byte) error {
	const q = `
update mcp_server
set tools_cache = $2::jsonb, last_sync_at = now(), health = 'ok', updated_at = now()
where id = $1::uuid`
	_, err := d.pool.Exec(ctx, q, id, cache)
	return err
}

// AppendAuditLog 写入审计记录；失败不影响主流程。
func (d *DB) AppendAuditLog(ctx context.Context, e store.AuditEntry) error {
	const q = `
insert into audit_log (actor, action, object, result, detail, trace_id)
values (nullif($1, '')::uuid, $2, $3, $4, $5::jsonb, nullif($6, ''))`

	detail := e.Detail
	if len(detail) == 0 {
		detail = []byte(`{}`)
	}
	_, err := d.pool.Exec(ctx, q, e.Actor, e.Action, e.Object, e.Result, detail, e.TraceID)
	return err
}

// RecordToolCall 记录一次工具调用。
func (d *DB) RecordToolCall(ctx context.Context, r store.ToolCallRecord) error {
	const q = `
insert into tool_call (session_id, run_id, tool_name, args, status, latency_ms, risk_level, result)
values (nullif($1, '')::uuid, nullif($2, '')::uuid, $3, $4::jsonb, $5, $6, $7, nullif($8, ''))`

	args := r.Args
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	_, err := d.pool.Exec(ctx, q, r.SessionID, r.RunID, r.ToolName, args, r.Status, r.LatencyMS, r.RiskLevel, r.Result)
	return err
}

// ListToolResults 返回一次执行里已经产生结果的工具调用。
func (d *DB) ListToolResults(ctx context.Context, runID string) ([]store.ToolCallRecord, error) {
	const q = `
select coalesce(session_id::text, ''), coalesce(run_id::text, ''), tool_name, args, status, latency_ms, risk_level, result
from tool_call
where run_id = $1::uuid and result is not null
order by created_at`

	rows, err := d.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.ToolCallRecord, 0, 8)
	for rows.Next() {
		var r store.ToolCallRecord
		if err := rows.Scan(&r.SessionID, &r.RunID, &r.ToolName, &r.Args,
			&r.Status, &r.LatencyMS, &r.RiskLevel, &r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeletePluginToolRecords 卸载插件时清理它注册的能力与工具。
//
// 工具名约定：工具插件就是 slug，MCP 是 slug__<远端工具>，所以按前缀匹配。
// audit_log / run_step 是历史，不在这里删。
func (d *DB) DeletePluginToolRecords(ctx context.Context, slug string) error {
	pattern := slug + `\_\_%`
	if _, err := d.pool.Exec(ctx,
		`delete from tool where name = $1 or name like $2`, slug, pattern); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx,
		`delete from capability where name = $1 or name like $2`, slug, pattern)
	return err
}
