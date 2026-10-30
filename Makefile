# harness_go — 构建与质量门禁
#
# Windows 上若无 make，可直接执行：
#   go build -trimpath -ldflags "-s -w" -o bin/harness-go.exe ./cmd/harness-go
#   go vet ./... ; go test -race ./...

BINARY   := harness-go
BIN_DIR  := bin
VERSION  ?= 0.0.0-dev
COMMIT   ?= none
BUILDAT  ?= unknown

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILDAT)

.PHONY: all build web build-all lint vet test fmt tidy run clean check

all: build

## 仅编译 Go 二进制（前端产物由 //go:embed all:dist 内嵌）
## 注意：web/dist/.gitkeep 必须入库，否则 embed 目录不存在会导致编译失败；
##       未构建前端时二进制仍可启动，`/` 返回「前端未构建」提示页
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/harness-go

## 编译前端产物
web:
	cd web && npm ci && npm run build

## 完整构建：前端 → 后端单二进制
build-all: web build

## 静态检查（golangci-lint v2，含 depguard 分层规则）
lint:
	golangci-lint run

vet:
	go vet ./...

## 单元测试（默认无 CGO，与单二进制交叉编译保持一致）
test:
	go test ./...

## 带竞态检测（需 CGO + gcc；CI 上跑）
test-race:
	CGO_ENABLED=1 go test -race ./...

fmt:
	gofmt -l -w .
	cd web && npx prettier --write "src/**/*.{ts,tsx,css}" 2>/dev/null || true

tidy:
	go mod tidy

run: build
	./$(BIN_DIR)/$(BINARY)

## 出口门禁：lint + vet + test
check: vet test lint

clean:
	rm -rf $(BIN_DIR) web/dist
