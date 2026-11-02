# 如果 go 不在 PATH 里，可覆盖：make run GO=/usr/local/go/bin/go
GO ?= go
NPM ?= npm
NPM_REGISTRY ?= https://registry.npmmirror.com

.PHONY: help tidy pg-up pg-down run build web fmt vet test

help:
	@echo "tidy      拉齐依赖并生成 go.sum（首次必须执行）"
	@echo "pg-up     启动本地 PostgreSQL"
	@echo "pg-down   停止本地 PostgreSQL"
	@echo "run       本地启动服务（127.0.0.1:8000）"
	@echo "web       构建前端并复制到 internal/app/dist（go:embed 依赖它）"
	@echo "build     编译到 bin/server（需先执行 web）"
	@echo "test      运行单元测试"

tidy:
	$(GO) mod tidy

pg-up:
	docker compose up -d postgres

pg-down:
	docker compose down

run:
	$(GO) run ./cmd/server

web:
	cd web && $(NPM) install --registry=$(NPM_REGISTRY) && $(NPM) run build
	rm -rf internal/app/dist && cp -r web/dist internal/app/dist

build:
	mkdir -p bin && $(GO) build -o bin/server ./cmd/server

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...
