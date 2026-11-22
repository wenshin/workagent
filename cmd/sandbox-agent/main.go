// Command sandbox-agent 是执行容器里的执行体：接收控制面的执行请求，
// 在容器内用 bwrap 起一次受限进程。
//
// 它自己不做任何策略判断（白名单、审批、审计都在控制面），只负责：
//  1. 启动时探测真实后端，起不来就如实报告 local（绝不假装隔离）；
//  2. 把 Run 请求翻译成 sandbox.Spec 交给 bwrap；
//  3. 用令牌挡住「集群内任何人都能执行命令」这条最危险的路径。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/workagent/auto-agent/internal/egress"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/sandbox/remote"
)

func main() {
	addr := flag.String("addr", env("SANDBOX_AGENT_ADDR", ":8323"), "监听地址")
	root := flag.String("root", env("SANDBOX_AGENT_ROOT", "/var/lib/workagent"), "允许执行的存储根目录（空表示不限制）")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	sandbox.SetExtraRead(splitList(env("SANDBOX_READ_EXTRA", "")))

	// 执行体是服务端唯一的执行面，只接受 bwrap：
	// 容器里不是 Linux 猜错平台，就是镜像没装 bubblewrap——两种情况都该启动失败，
	// 而不是退回「容器即边界」。服务端不存在无隔离的合法档位（WA_ROLE=server 同理）。
	runner, err := sandbox.NewBwrap()
	if err != nil {
		log.Error("执行体必须用 bwrap：容器内的空间级隔离没有替代品", "err", err)
		os.Exit(1)
	}
	log.Info("执行体已就绪", "backend", runner.Name(), "root", *root)

	token := env("SANDBOX_RUNNER_TOKEN", "")
	if token == "" {
		log.Error("缺少 SANDBOX_RUNNER_TOKEN：拒绝在没有鉴权的情况下暴露执行接口")
		os.Exit(1)
	}

	// 环境自述由镜像构建时写入（见 deploy/runner/Dockerfile）。
	// 读不到只告警不退出：老镜像没有这个文件是正常的，控制面会整段省略，
	// 表现为「提示词里没有执行环境一节」，而不是执行不可用。
	facts, err := sandbox.LoadEnvFacts(env("SANDBOX_ENV_FACTS", sandbox.DefaultEnvFactsPath))
	if err != nil {
		log.Warn("读取执行环境自述失败，提示词将不包含执行环境", "err", err)
	}
	if !facts.Empty() {
		log.Info("执行环境已载入", "image", facts.Image, "tools", len(facts.Tools), "env_hash", facts.Hash())
	}

	// 出网代理由执行体自己建：控制面的 socket 在另一个容器的 /tmp 里，
	// 路径传过来没有意义。控制面只把白名单（Spec.Egress）过线。
	eg := egress.NewRegistry(log)
	defer eg.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           remote.Handler(runner, token, *root, facts, eg, log),
		ReadHeaderTimeout: 5 * time.Second,
		// 不设 WriteTimeout：单次执行的墙钟由 Spec.Timeout 决定（最长 30s 量级），
		// 这里再压一个上限只会把合法的长命令截断，而且报错位于 HTTP 层、难排查。
		// 常驻进程更依赖这一条：它的连接会一直开着。
		IdleTimeout: 60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("执行体开始监听", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("执行体退出", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
