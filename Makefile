# 如果 go 不在 PATH 里，可覆盖：make run GO=/usr/local/go/bin/go
GO ?= go
NPM ?= npm
NPM_REGISTRY ?= https://registry.npmmirror.com

.PHONY: help tidy pg-up pg-down run build web fmt vet test mock-dc

help:
	@echo "tidy      拉齐依赖并生成 go.sum（首次必须执行）"
	@echo "pg-up     启动本地 PostgreSQL"
	@echo "pg-down   停止本地 PostgreSQL"
	@echo "run       本地启动 API（127.0.0.1:8321）"
	@echo "web       构建前端到 web/dist（上传 CDN）"
	@echo "build     编译 API 到 bin/server"
	@echo "test      运行单元测试"
	@echo "mock-dc   启动 mock 大数据平台（127.0.0.1:8322，联调数仓类智能体）"

tidy:
	$(GO) mod tidy

pg-up:
	docker compose up -d postgres

pg-down:
	docker compose down

run:
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
