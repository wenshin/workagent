package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/workagent/auto-agent/internal/app"
	"github.com/workagent/auto-agent/internal/config"
	"github.com/workagent/auto-agent/internal/storage/localfs"
	"github.com/workagent/auto-agent/internal/store/postgres"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("初始化数据库失败", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	if err := db.Migrate(migrateCtx); err != nil {
		cancelMigrate()
		logger.Error("执行数据库迁移失败", "err", err)
		os.Exit(1)
	}
	cancelMigrate()
	logger.Info("数据库迁移完成")

	fs, err := localfs.New(cfg.StorageRoot)
	if err != nil {
		logger.Error("初始化存储失败", "err", err)
		os.Exit(1)
	}
	logger.Info("存储已就绪", "root", fs.Root())

	application, err := app.New(cfg, db, fs, logger)
	if err != nil {
		logger.Error("初始化应用失败", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 不设 WriteTimeout：Go 的这个值是「整条响应」的绝对写截止时间，且无法按路由豁免，
		// 会把 SSE 长连接按秒掐断（表现为「回答到一半停住、无任何报错」）。
		// 流式写入的时限由 platform.Stream 每次写前刷新 + 15s 心跳维持（技术方案 §7.3）。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("服务已启动",
			"addr", "http://"+cfg.HTTPAddr,
			"web", cfg.WebBaseURL,
			"redirect_uri", cfg.DingTalkRedirectURI,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("服务异常退出", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
	}
	// 插件常驻进程要显式关：它们在独立进程组里，服务退出不会自动带走。
	application.Close()
	logger.Info("服务已停止")
}
