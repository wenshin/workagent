# 如果 go 不在 PATH 里，可覆盖：make run GO=/usr/local/go/bin/go
GO ?= go
NPM ?= npm
NPM_REGISTRY ?= https://registry.npmmirror.com

# 前端产物不入库，但 go:embed 依赖它，因此编译类目标都会先确保 dist 存在
WEB_SOURCES := $(shell find web/src web/public -type f 2>/dev/null) \
	web/index.html web/package.json web/package-lock.json \
	web/tsconfig.json web/vite.config.ts
DIST_INDEX := internal/app/dist/index.html

.PHONY: help tidy pg-up pg-down run build web fmt vet test

help:
	@echo "tidy      拉齐依赖并生成 go.sum（首次必须执行）"
	@echo "pg-up     启动本地 PostgreSQL"
	@echo "pg-down   停止本地 PostgreSQL"
	@echo "run       本地启动服务（127.0.0.1:8000）"
	@echo "web       构建前端并复制到 internal/app/dist（go:embed 依赖它）"
	@echo "build     编译到 bin/server（自动先构建前端）"
	@echo "test      运行单元测试（自动先构建前端）"

tidy:
	$(GO) mod tidy

pg-up:
	docker compose up -d postgres

pg-down:
	docker compose down

run: $(DIST_INDEX)
	$(GO) run ./cmd/server

web: $(DIST_INDEX)

build: $(DIST_INDEX)
	mkdir -p bin && $(GO) build -o bin/server ./cmd/server

fmt:
	$(GO) fmt ./...

vet: $(DIST_INDEX)
	$(GO) vet ./...

test: $(DIST_INDEX)
	$(GO) test ./...

$(DIST_INDEX): $(WEB_SOURCES)
	@echo "==> 构建前端"
	cd web && $(NPM) ci --registry=$(NPM_REGISTRY) && $(NPM) run build
	rm -rf internal/app/dist && cp -r web/dist internal/app/dist
	@touch $(DIST_INDEX)
