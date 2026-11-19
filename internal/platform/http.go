package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/workagent/auto-agent/internal/cache"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// RequestID 为每个请求生成 trace_id，回写响应头并注入 context，便于排查问题。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newID(8)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestIDFrom 从 context 取出 trace_id。
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// Recover 捕获 panic，返回 500 并记录堆栈，避免单个请求击垮进程。
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						"request_id", RequestIDFrom(r.Context()),
						"path", r.URL.Path,
						"panic", rec,
					)
					WriteError(w, r, http.StatusInternalServerError, "internal_error", "服务内部错误，请稍后重试")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog 输出访问日志。
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", RequestIDFrom(r.Context()),
				"cost_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// APIError 是统一的错误响应体。
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON 输出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError 输出统一错误响应。
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	WriteJSON(w, status, APIError{
		Code:      code,
		Message:   msg,
		RequestID: RequestIDFrom(r.Context()),
	})
}

// WriteAuthError 把「还原身份失败」翻译成响应。
//
// 没登录是 401，缓存不可用是 503——两者混成一个，Redis 抖一下就会把所有人踢到
// 登录页，而登录页也因为同一个原因进不去，表现为「突然全站掉线」。
func WriteAuthError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, cache.ErrUnavailable) {
		WriteError(w, r, http.StatusServiceUnavailable, "session_store_unavailable",
			"登录服务暂不可用，请稍后重试")
		return
	}
	WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
}

// NewID 生成 n 字节的随机十六进制串。
func NewID(n int) string { return newID(n) }

func newID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}

// DecodeJSON 解析请求体，限制 1MB。
func DecodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
