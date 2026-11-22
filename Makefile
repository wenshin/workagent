# 如果 go 不在 PATH 里，可覆盖：make run GO=/usr/local/go/bin/go
GO ?= go
NPM ?= npm
NPM_REGISTRY ?= https://registry.npmmirror.com

.PHONY: help tidy pg-up redis-up pg-down run build web fmt vet test mock-dc sandbox-up sandbox-check sandbox-down

help:
	@echo "tidy      拉齐依赖并生成 go.sum（首次必须执行）"
	@echo "pg-up     启动本地 PostgreSQL"
	@echo "redis-up  启动本地 Redis（登录态与会话流缓存）"
	@echo "pg-down   停止本地 PostgreSQL 与 Redis"
	@echo "run       本地启动 API（127.0.0.1:8321）"
	@echo "web       构建前端到 web/dist（上传 CDN）"
	@echo "build     编译 API 到 bin/server"
	@echo "test      运行单元测试"
	@echo "mock-dc   启动 mock 大数据平台（127.0.0.1:8322，联调数仓类智能体）"

tidy:
	$(GO) mod tidy

pg-up:
	docker compose up -d postgres

redis-up:
	docker compose up -d redis

pg-down:
	docker compose down

# 服务端执行 agent 一律走远端沙箱（开发 / CI / 生产一致）：先 make sandbox-up 起执行体，
# 再导出 SANDBOX_RUNNER_ADDR / SANDBOX_RUNNER_TOKEN；缺任一项服务会直接启动失败，不退回本机执行。
run:
	@[ -n "$$SANDBOX_RUNNER_ADDR" ] && [ -n "$$SANDBOX_RUNNER_TOKEN" ] || \
		echo "提示：需要 SANDBOX_RUNNER_ADDR / SANDBOX_RUNNER_TOKEN（见 README 快速开始）"
	$(GO) run ./cmd/server

build:
	mkdir -p bin && $(GO) build -o bin/server ./cmd/server

web:
	cd web && $(NPM) ci --registry=$(NPM_REGISTRY) && $(NPM) run build
	@echo "前端产物已生成：web/dist（整目录上传 CDN）"

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

mock-dc:
	$(GO) run ./cmd/mock-datacenter

# 执行面（容器化沙箱）：构建镜像 → 部署执行体 → 跑部署后验。
# 前置：kubectl 指向本地集群（OrbStack / kind / k3d），docker 可用。
sandbox-up:
	./scripts/sandbox-exec.sh up

sandbox-check:
	./scripts/sandbox-exec.sh check

sandbox-down:
	./scripts/sandbox-exec.sh down
