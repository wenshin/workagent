package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mcpProtocolVersion 与平台 MCP 客户端保持一致（internal/mcp/client.go）。
const mcpProtocolVersion = "2024-11-05"

// rpcRequest 是 JSON-RPC 2.0 请求；ID 为空表示通知。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcp 实现 streamable HTTP 传输的 MCP 服务端（无 SSE，直接回 JSON）。
func (s *server) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "请求体不是合法 JSON："+err.Error())
		return
	}

	// 通知（notifications/initialized 等）不需要响应体，回 202 即可。
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", fmt.Sprintf("mock-%d", time.Now().UnixNano()))
		version := mcpProtocolVersion
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		if params.ProtocolVersion != "" {
			version = params.ProtocolVersion
		}
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mock-datacenter", "version": "0.1.0"},
		})
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": mcpToolDefs()})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPCError(w, req.ID, -32602, "tools/call 参数不合法："+err.Error())
			return
		}
		text, isErr := s.callTool(r, params.Name, params.Arguments)
		writeRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		})
	default:
		writeRPCError(w, req.ID, -32601, "未实现的方法："+req.Method)
	}
}

// mcpToolDefs 只暴露四个业务工具，与 REST 接口一一对应。
func mcpToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "search_tables",
			"description": "按关键字搜索大数据表，返回表元信息（库名、表名、字段、分区、行数、更新时间）与该表负责人（姓名、工号、邮箱）。关键字可以命中表名、字段名、描述、标签或负责人。",
			"inputSchema": rawSchema(`{
				"type": "object",
				"properties": {
					"keyword":   {"type": "string", "description": "搜索关键字，例如 订单、gmv、sales、严小序；为空时列出全部表"},
					"database":  {"type": "string", "description": "限定库名，例如 dw / dm / ods"},
					"page":      {"type": "integer", "description": "页码，从 1 开始，默认 1"},
					"page_size": {"type": "integer", "description": "每页条数，默认 20，最大 100"}
				}
			}`),
		},
		{
			"name":        "submit_query",
			"description": "提交一条 Spark SQL 查询任务，异步执行并立即返回 task_id。只支持 SELECT（分析账号只读）。之后用 get_query_status 轮询状态，成功后用 get_query_result 取数据或 download_query_result 下载文件。",
			"inputSchema": rawSchema(`{
				"type": "object",
				"properties": {
					"sql":          {"type": "string", "description": "要执行的 Spark SQL，必须是 SELECT 语句"},
					"engine":       {"type": "string", "description": "执行引擎，默认 spark"},
					"wait_seconds": {"type": "integer", "description": "提交后阻塞等待的秒数，0 表示立即返回，最大 25"}
				},
				"required": ["sql"]
			}`),
		},
		{
			"name":        "get_query_status",
			"description": "查询任务状态（pending / running / succeeded / failed）、进度、耗时与错误信息。wait_seconds 大于 0 时会阻塞等待任务结束再返回，最长 25 秒。",
			"inputSchema": rawSchema(`{
				"type": "object",
				"properties": {
					"task_id":      {"type": "string", "description": "submit_query 返回的任务 ID"},
					"wait_seconds": {"type": "integer", "description": "阻塞等待的秒数，0 表示立即返回，最大 25"}
				},
				"required": ["task_id"]
			}`),
		},
		{
			"name":        "get_query_result",
			"description": "获取任务结果数据（JSON，可按 offset / limit 分页）。任务未结束或失败时返回对应状态，不报错。",
			"inputSchema": rawSchema(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "任务 ID"},
					"offset":  {"type": "integer", "description": "起始行号，默认 0"},
					"limit":   {"type": "integer", "description": "返回行数，默认 50，最大 200"}
				},
				"required": ["task_id"]
			}`),
		},
		{
			"name":        "download_query_result",
			"description": "获取任务结果数据的下载地址与文件摘要（CSV / JSON）。任务必须已成功结束。返回内容包括下载链接、文件名、总行数和前几行预览。",
			"inputSchema": rawSchema(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "任务 ID"},
					"format":  {"type": "string", "description": "下载格式，csv 或 json，默认 csv"}
				},
				"required": ["task_id"]
			}`),
		},
	}
}

// callTool 分发工具调用，返回给模型看的文本；第二个返回值表示是否业务失败。
func (s *server) callTool(r *http.Request, name string, args json.RawMessage) (string, bool) {
	var in map[string]any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return "工具参数不是合法的 JSON 对象：" + err.Error(), true
		}
	}
	if in == nil {
		in = map[string]any{}
	}

	switch name {
	case "search_tables":
		keyword := argString(in, "keyword")
		database := argString(in, "database")
		page := argInt(in, "page", 1)
		pageSize := argInt(in, "page_size", 20)
		total, items := searchTablesData(keyword, database, page, pageSize)
		return toText(map[string]any{
			"total":     total,
			"page":      page,
			"page_size": pageSize,
			"items":     items,
			"caller":    claimsFrom(r).Name,
		}), false

	case "submit_query":
		sql := argString(in, "sql")
		if strings.TrimSpace(sql) == "" {
			return "sql 不能为空", true
		}
		task := s.tasks.submit(sql, argString(in, "engine"))
		if wait := argInt(in, "wait_seconds", 0); wait > 0 {
			task, _ = s.tasks.wait(r.Context(), task.ID, time.Duration(clampSeconds(wait))*time.Second)
		}
		return toText(s.viewOf(task, r)), task.Status == statusFailed

	case "get_query_status":
		id := argString(in, "task_id")
		if id == "" {
			return "task_id 不能为空", true
		}
		wait := time.Duration(clampSeconds(argInt(in, "wait_seconds", 0))) * time.Second
		task, ok := s.tasks.wait(r.Context(), id, wait)
		if !ok {
			return "任务不存在：" + id, true
		}
		return toText(s.viewOf(task, r)), false

	case "get_query_result":
		id := argString(in, "task_id")
		if id == "" {
			return "task_id 不能为空", true
		}
		task, ok := s.tasks.get(id)
		if !ok {
			return "任务不存在：" + id, true
		}
		if !task.done() {
			return toText(map[string]any{
				"task_id":  task.ID,
				"status":   task.Status,
				"progress": task.Progress,
				"message":  "任务尚未结束，请稍后再取结果",
			}), false
		}
		if task.Status == statusFailed {
			return toText(map[string]any{
				"task_id": task.ID,
				"status":  task.Status,
				"message": task.Message,
				"columns": []string{},
				"rows":    []map[string]any{},
			}), false
		}
		offset := argInt(in, "offset", 0)
		limit := argInt(in, "limit", 50)
		if limit > maxResultRows {
			limit = maxResultRows
		}
		total := len(task.Rows)
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		return toText(map[string]any{
			"task_id":   task.ID,
			"status":    task.Status,
			"columns":   task.Columns,
			"rows":      task.Rows[offset:end],
			"row_count": total,
			"offset":    offset,
			"has_more":  end < total,
		}), false

	case "download_query_result":
		id := argString(in, "task_id")
		if id == "" {
			return "task_id 不能为空", true
		}
		task, ok := s.tasks.get(id)
		if !ok {
			return "任务不存在：" + id, true
		}
		if !task.done() {
			return toText(map[string]any{
				"task_id": task.ID,
				"status":  task.Status,
				"message": "任务尚未结束，当前状态：" + task.Status,
			}), true
		}
		if task.Status == statusFailed {
			return toText(map[string]any{
				"task_id": task.ID,
				"status":  task.Status,
				"message": "任务执行失败，无结果可下载：" + task.Message,
			}), true
		}
		format := strings.ToLower(argString(in, "format"))
		if format != "json" {
			format = "csv"
		}
		base := s.baseURL(r) + "/api/v1/queries/" + task.ID + "/download?format=" + format
		return toText(map[string]any{
			"task_id":      task.ID,
			"format":       format,
			"file_name":    task.ID + "." + format,
			"row_count":    len(task.Rows),
			"columns":      task.Columns,
			"download_url": base,
			"note":         "下载链接需要携带同一个 JWT 才能访问（Authorization: Bearer <token>）",
			"preview":      previewCSV(task, 5),
		}), false

	default:
		return "未知工具：" + name, true
	}
}

// previewCSV 生成前 n 行的纯文本预览，模型不用下载就能看两眼数据。
func previewCSV(t Task, n int) string {
	if len(t.Rows) < n {
		n = len(t.Rows)
	}
	var b strings.Builder
	b.WriteString(strings.Join(t.Columns, ","))
	b.WriteString("\n")
	for i := 0; i < n; i++ {
		cells := make([]string, 0, len(t.Columns))
		for _, col := range t.Columns {
			cells = append(cells, formatCell(t.Rows[i][col]))
		}
		b.WriteString(strings.Join(cells, ","))
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// JSON-RPC 编码
// ---------------------------------------------------------------------------

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   rpcError{Code: code, Message: msg},
	})
}

func toText(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func rawSchema(s string) json.RawMessage {
	var buf json.RawMessage
	if err := json.Unmarshal([]byte(s), &buf); err != nil {
		panic("内置工具 schema 不合法: " + err.Error())
	}
	return buf
}

func argString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func argInt(m map[string]any, key string, fallback int) int {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		var n int
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
