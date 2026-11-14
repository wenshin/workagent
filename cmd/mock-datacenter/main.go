// Command mock-datacenter 是本地联调用的「大数据平台」mock 服务。
//
// 它只解决一件事：让数据分析类智能体在没有真实数仓的环境里也能跑通
// 「搜表 → 提交 Spark SQL → 异步等结果 → 预览 / 下载」这条完整链路。
//
// 接口：
//
//	GET  /healthz                          健康检查（免鉴权）
//	POST /auth/token                       调试签发 JWT（免鉴权，可关闭）
//	GET  /auth/me                          回显当前 token 的身份（需要鉴权）
//	GET  /api/v1/tables                    表搜索，返回表元信息与负责人
//	POST /api/v1/queries                   提交 Spark SQL，异步执行
//	GET  /api/v1/queries/{id}              查询任务状态，支持 wait_seconds 长轮询
//	GET  /api/v1/queries/{id}/result       结果预览（JSON，offset/limit）
//	GET  /api/v1/queries/{id}/download     结果下载（format=csv|json）
//	POST /mcp                              MCP streamable HTTP（initialize/tools/list/tools/call）
//
// 身份：所有业务接口都要带平台注入的 JWT（HS256），取 Authorization: Bearer <token>，
// 也兼容不带 Bearer 前缀与 X-MCP-Delegation 头。校验 iss / aud / exp / 签名。
//
// 环境变量：
//
//	MOCK_DC_ADDR               监听地址，默认 127.0.0.1:8322
//	MOCK_DC_JWT_SECRET         HS256 密钥，默认 mock-datacenter-dev-secret
//	MOCK_DC_JWT_ISS            签发者，默认 workagent-mock-dc
//	MOCK_DC_JWT_AUD            受众，默认 mock-dc
//	MOCK_DC_JWT_SKIP_AUD       true 时不校验 aud
//	MOCK_DC_JWT_DISABLE_AUTH   true 时跳过鉴权（仅本地快速调试）
//	MOCK_DC_ALLOW_TOKEN_ISSUE  false 时关闭 /auth/token
//	MOCK_DC_QUERY_DELAY_MS     单任务模拟耗时，默认 1500
//	MOCK_DC_PUBLIC_URL         下载链接前缀，默认按请求 Host 推导
//	MOCK_DC_TLS_CERT           证书路径；与 KEY 同时配置时以 https 监听
//	MOCK_DC_TLS_KEY            私钥路径
//
// 本地跑起来：
//
//	go run ./cmd/mock-datacenter
//	curl -s -XPOST localhost:8322/auth/token -d '{}' | jq -r .token
//
// 以插件形式接入时（plugin.json 的 transport.url 强制 https）：
//
//	mkcert -install && mkcert 127.0.0.1 localhost   # 生成 PEM 证书
//	MOCK_DC_TLS_CERT=./127.0.0.1+localhost.pem \
//	MOCK_DC_TLS_KEY=./127.0.0.1+localhost-key.pem go run ./cmd/mock-datacenter
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
)

// config 是 mock 服务的运行参数，全部来自环境变量。
type config struct {
	addr            string
	jwtSecret       string
	jwtIss          string
	jwtAud          string
	skipAud         bool
	disableAuth     bool
	allowTokenIssue bool
	queryDelay      time.Duration
	publicURL       string
	tlsCert         string
	tlsKey          string
}

func loadConfig() config {
	return config{
		addr:            env("MOCK_DC_ADDR", "127.0.0.1:8322"),
		jwtSecret:       env("MOCK_DC_JWT_SECRET", "mock-datacenter-dev-secret"),
		jwtIss:          env("MOCK_DC_JWT_ISS", "workagent-mock-dc"),
		jwtAud:          env("MOCK_DC_JWT_AUD", "mock-dc"),
		skipAud:         env("MOCK_DC_JWT_SKIP_AUD", "") == "true",
		disableAuth:     env("MOCK_DC_JWT_DISABLE_AUTH", "") == "true",
		allowTokenIssue: env("MOCK_DC_ALLOW_TOKEN_ISSUE", "true") != "false",
		queryDelay:      time.Duration(envInt("MOCK_DC_QUERY_DELAY_MS", 1500)) * time.Millisecond,
		publicURL:       env("MOCK_DC_PUBLIC_URL", ""),
		tlsCert:         env("MOCK_DC_TLS_CERT", ""),
		tlsKey:          env("MOCK_DC_TLS_KEY", ""),
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := loadConfig()
	srv := &server{
		cfg:   cfg,
		log:   log,
		jwt:   newJWTIssuer(cfg.jwtSecret, cfg.jwtIss, cfg.jwtAud, cfg.skipAud),
		tasks: newTaskStore(cfg.queryDelay),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.healthz)
	mux.HandleFunc("POST /auth/token", srv.issueToken)
	mux.Handle("GET /auth/me", srv.requireAuth(http.HandlerFunc(srv.me)))
	mux.Handle("GET /api/v1/tables", srv.requireAuth(http.HandlerFunc(srv.searchTables)))
	mux.Handle("POST /api/v1/queries", srv.requireAuth(http.HandlerFunc(srv.submitQuery)))
	mux.Handle("GET /api/v1/queries/{id}", srv.requireAuth(http.HandlerFunc(srv.getQuery)))
	mux.Handle("GET /api/v1/queries/{id}/result", srv.requireAuth(http.HandlerFunc(srv.queryResult)))
	mux.Handle("GET /api/v1/queries/{id}/download", srv.requireAuth(http.HandlerFunc(srv.downloadQuery)))
	mux.Handle("POST /mcp", srv.requireAuth(http.HandlerFunc(srv.mcp)))

	var handler http.Handler = mux
	handler = platform.AccessLog(log)(handler)
	handler = platform.Recover(log)(handler)
	handler = platform.RequestID(handler)
	handler = cors(handler)

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scheme := "http"
	useTLS := cfg.tlsCert != "" && cfg.tlsKey != ""
	if useTLS {
		scheme = "https"
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("mock 数仓已启动",
			"addr", scheme+"://"+cfg.addr,
			"mcp", scheme+"://"+cfg.addr+"/mcp",
			"auth", !cfg.disableAuth,
			"tls", useTLS,
			"tables", len(mockTables()),
		)
		var err error
		if useTLS {
			err = httpSrv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Error("服务异常退出", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("收到退出信号，开始关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("优雅关闭失败", "err", err)
	}
	log.Info("mock 数仓已停止")
}

// cors 放开本地调试的跨域请求（浏览器里直接点下载链接时会用到）。
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-MCP-Delegation, Mcp-Session-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
