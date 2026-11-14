package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
)

// maxWaitSeconds 是 wait_seconds 的上限。平台工具调用墙钟是 30s，
// 留出余量，避免长轮询把调用方拖到超时。
const maxWaitSeconds = 25

type ctxKey string

const claimsKey ctxKey = "claims"

// server 聚合 mock 服务的全部依赖。
type server struct {
	cfg   config
	log   *slog.Logger
	jwt   *jwtIssuer
	tasks *taskStore
}

// claimsFrom 取当前请求的身份；鉴权被关闭时返回一个匿名声明。
func claimsFrom(r *http.Request) *Claims {
	if c, ok := r.Context().Value(claimsKey).(*Claims); ok && c != nil {
		return c
	}
	return &Claims{Sub: "anonymous", Name: "匿名调试"}
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

// requireAuth 校验平台注入的 JWT。
//
// 兼容三种携带方式（平台侧目前是静态串注入，见 internal/mcp/client.go）：
//  1. Authorization: Bearer <jwt>   —— 标准方式，推荐
//  2. Authorization: <jwt>          —— 不带前缀
//  3. X-MCP-Delegation: <jwt>       —— 技术方案里的委派头
func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.disableAuth {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated",
				"缺少身份凭证，请通过 Authorization: Bearer <jwt> 传入")
			return
		}
		claims, err := s.jwt.verify(token)
		if err != nil {
			s.log.Warn("token 校验失败",
				"path", r.URL.Path,
				"request_id", platform.RequestIDFrom(r.Context()),
				"err", err.Error(),
			)
			platform.WriteError(w, r, http.StatusUnauthorized, "invalid_token", err.Error())
			return
		}
		s.log.Info("身份校验通过",
			"path", r.URL.Path,
			"sub", claims.Sub,
			"user_id", claims.UserID,
			"name", claims.Name,
			"email", claims.Email,
			"job_number", claims.JobNumber,
			"space_id", claims.SpaceID,
			"act", claims.Act,
		)
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken 从请求头里取出 token。
func bearerToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-MCP-Delegation"))
}

// ---------------------------------------------------------------------------
// 调试端点
// ---------------------------------------------------------------------------

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "mock-datacenter",
		"tables":  len(mockTables()),
		"auth":    !s.cfg.disableAuth,
	})
}

// issueToken 是调试用签发端点：拿到 token 后直接填进平台的密钥配置。
func (s *server) issueToken(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.allowTokenIssue {
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "token 签发端点已关闭")
		return
	}
	var req struct {
		Sub        string `json:"sub"`
		Name       string `json:"name"`
		JobNumber  string `json:"job_number"`
		SpaceID    string `json:"space_id"`
		Scope      string `json:"scope"`
		Act        string `json:"act"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	}
	if req.Sub == "" {
		req.Sub = "u_99999"
	}
	if req.Name == "" {
		req.Name = ownerYan.Name
	}
	if req.JobNumber == "" {
		req.JobNumber = ownerYan.JobNumber
	}
	claims := Claims{
		Sub:       req.Sub,
		Name:      req.Name,
		JobNumber: req.JobNumber,
		SpaceID:   req.SpaceID,
		Scope:     req.Scope,
		Act:       req.Act,
	}
	if req.TTLSeconds > 0 {
		claims.Exp = time.Now().Add(time.Duration(req.TTLSeconds) * time.Second).Unix()
	}
	token, signed, err := s.jwt.sign(claims)
	if err != nil {
		platform.WriteError(w, r, http.StatusInternalServerError, "sign_failed", err.Error())
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_in": signed.Exp - signed.Iat,
		"claims":     signed,
	})
}

// me 用来手工验证「平台注进来的 token 到底是谁」。
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	platform.WriteJSON(w, http.StatusOK, claimsFrom(r))
}

// ---------------------------------------------------------------------------
// 表搜索
// ---------------------------------------------------------------------------

// searchTables 返回表元信息与负责人。
//
// 参数：keyword / database / page / page_size。
func (s *server) searchTables(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := firstNonEmpty(q.Get("keyword"), q.Get("kw"), q.Get("q"), q.Get("table"))
	database := strings.TrimSpace(q.Get("database"))
	page := positiveInt(q.Get("page"), 1)
	pageSize := positiveInt(q.Get("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}

	total, items := searchTablesData(keyword, database, page, pageSize)

	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"keyword":   keyword,
		"items":     items,
		"caller":    claimsFrom(r).Name,
	})
}

// searchTablesData 是表搜索的公共实现，REST 与 MCP 共用。
func searchTablesData(keyword, database string, page, pageSize int) (int, []*Table) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	matched := make([]*Table, 0, len(mockTables()))
	for _, t := range mockTables() {
		if database != "" && !strings.EqualFold(t.Database, database) {
			continue
		}
		if !matchTable(t, keyword) {
			continue
		}
		matched = append(matched, t)
	}

	total := len(matched)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return total, matched[start:end]
}

// ---------------------------------------------------------------------------
// 查询任务
// ---------------------------------------------------------------------------

// queryView 是任务对外的 JSON 形态。
type queryView struct {
	TaskID      string   `json:"task_id"`
	Engine      string   `json:"engine"`
	SQL         string   `json:"sql"`
	Status      string   `json:"status"`
	Progress    int      `json:"progress"`
	Message     string   `json:"message,omitempty"`
	Columns     []string `json:"columns,omitempty"`
	RowCount    int      `json:"row_count"`
	DurationMS  int64    `json:"duration_ms"`
	CreatedAt   string   `json:"created_at"`
	StartedAt   string   `json:"started_at,omitempty"`
	FinishedAt  string   `json:"finished_at,omitempty"`
	ResultURL   string   `json:"result_url,omitempty"`
	DownloadCSV string   `json:"download_csv_url,omitempty"`
	DownloadJSN string   `json:"download_json_url,omitempty"`
}

func (s *server) viewOf(t Task, r *http.Request) queryView {
	base := s.baseURL(r) + "/api/v1/queries/" + t.ID
	v := queryView{
		TaskID:      t.ID,
		Engine:      t.Engine,
		SQL:         t.SQL,
		Status:      t.Status,
		Progress:    t.Progress,
		Message:     t.Message,
		Columns:     t.Columns,
		RowCount:    t.RowCount,
		CreatedAt:   t.CreatedAt.Format(time.RFC3339),
		ResultURL:   base + "/result",
		DownloadCSV: base + "/download?format=csv",
		DownloadJSN: base + "/download?format=json",
	}
	if !t.StartedAt.IsZero() {
		v.StartedAt = t.StartedAt.Format(time.RFC3339)
	}
	if !t.FinishedAt.IsZero() {
		v.FinishedAt = t.FinishedAt.Format(time.RFC3339)
		v.DurationMS = t.FinishedAt.Sub(t.StartedAt).Milliseconds()
	}
	return v
}

// submitQuery 提交 Spark SQL，默认立即返回任务 ID。
// 传 wait_seconds 时提交后直接等到结束（最多 25s），省一次轮询。
func (s *server) submitQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL         string `json:"sql"`
		Engine      string `json:"engine"`
		WaitSeconds int    `json:"wait_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_argument", "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_argument", "sql 不能为空")
		return
	}

	task := s.tasks.submit(req.SQL, req.Engine)
	s.log.Info("提交查询任务",
		"task_id", task.ID,
		"engine", task.Engine,
		"submitter", claimsFrom(r).Name,
		"space_id", claimsFrom(r).SpaceID,
	)

	if req.WaitSeconds > 0 {
		wait := time.Duration(clampSeconds(req.WaitSeconds)) * time.Second
		task, _ = s.tasks.wait(r.Context(), task.ID, wait)
	}
	platform.WriteJSON(w, http.StatusOK, s.viewOf(task, r))
}

// getQuery 查任务状态；wait_seconds > 0 时阻塞等待任务结束。
func (s *server) getQuery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait := time.Duration(clampSeconds(positiveInt(r.URL.Query().Get("wait_seconds"), 0))) * time.Second

	task, ok := s.tasks.wait(r.Context(), id, wait)
	if !ok {
		platform.WriteError(w, r, http.StatusNotFound, "task_not_found", "任务不存在："+id)
		return
	}
	platform.WriteJSON(w, http.StatusOK, s.viewOf(task, r))
}

// queryResult 返回结果预览（JSON），支持 offset/limit 分页。
func (s *server) queryResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := s.tasks.get(id)
	if !ok {
		platform.WriteError(w, r, http.StatusNotFound, "task_not_found", "任务不存在："+id)
		return
	}
	if !task.done() {
		platform.WriteJSON(w, http.StatusAccepted, map[string]any{
			"task_id":  task.ID,
			"status":   task.Status,
			"progress": task.Progress,
			"message":  "任务尚未结束，请稍后重试",
		})
		return
	}
	if task.Status == statusFailed {
		platform.WriteJSON(w, http.StatusOK, map[string]any{
			"task_id":   task.ID,
			"status":    task.Status,
			"message":   task.Message,
			"columns":   []string{},
			"rows":      []map[string]any{},
			"row_count": 0,
		})
		return
	}

	offset := nonNegativeInt(r.URL.Query().Get("offset"), 0)
	limit := positiveInt(r.URL.Query().Get("limit"), 50)
	if limit > maxResultRows {
		limit = maxResultRows
	}
	total := len(task.Rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}

	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"task_id":   task.ID,
		"status":    task.Status,
		"columns":   task.Columns,
		"rows":      task.Rows[offset:end],
		"row_count": total,
		"offset":    offset,
		"limit":     limit,
		"has_more":  end < total,
	})
}

// downloadQuery 下载结果数据（csv / json）。
//
// 真实数仓一般是「拿一个带签名的临时下载地址」，mock 直接流式返回文件，
// 语义一致：调用方拿到的是完整结果集，不是分页预览。
func (s *server) downloadQuery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := s.tasks.get(id)
	if !ok {
		platform.WriteError(w, r, http.StatusNotFound, "task_not_found", "任务不存在："+id)
		return
	}
	if !task.done() {
		platform.WriteError(w, r, http.StatusConflict, "task_not_finished", "任务尚未结束，当前状态："+task.Status)
		return
	}
	if task.Status == statusFailed {
		platform.WriteError(w, r, http.StatusUnprocessableEntity, "task_failed", "任务执行失败，无结果可下载："+task.Message)
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}

	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, task.ID))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": task.ID,
			"columns": task.Columns,
			"rows":    task.Rows,
		})
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, task.ID))
		cw := csv.NewWriter(w)
		_ = cw.Write(task.Columns)
		for _, row := range task.Rows {
			record := make([]string, 0, len(task.Columns))
			for _, col := range task.Columns {
				record = append(record, formatCell(row[col]))
			}
			_ = cw.Write(record)
		}
		cw.Flush()
	default:
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_argument", "format 只支持 csv 或 json")
	}
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func (s *server) baseURL(r *http.Request) string {
	if s.cfg.publicURL != "" {
		return strings.TrimRight(s.cfg.publicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = s.cfg.addr
	}
	return scheme + "://" + host
}

func formatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

func clampSeconds(n int) int {
	if n <= 0 {
		return 0
	}
	if n > maxWaitSeconds {
		return maxWaitSeconds
	}
	return n
}

func positiveInt(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func nonNegativeInt(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
