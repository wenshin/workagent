package platform

import (
	"net/http"
	"strings"
)

// CORS 允许白名单内的前端源跨域携带 Cookie 访问 API。
// 前后端分离部署时由 WEB_BASE_URL / CORS_ALLOWED_ORIGINS 决定白名单。
func CORS(allowed []string) func(http.Handler) http.Handler {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, origin := range allowed {
		allowSet[strings.TrimRight(origin, "/")] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowSet[origin]; ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Set("Access-Control-Allow-Credentials", "true")
					h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id")
					h.Set("Access-Control-Max-Age", "600")
					h.Add("Vary", "Origin")

					if r.Method == http.MethodOptions {
						w.WriteHeader(http.StatusNoContent)
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
