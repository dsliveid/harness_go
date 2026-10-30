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

## 前端产物目录的占位文件：web/dist 目录必须存在（//go:embed all:dist 的编译期要求），
## 但内容不入库，故用 .gitkeep 占位
DIST_KEEP := web/dist/.gitkeep

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILDAT)

.PHONY: all build web build-all lint vet test fmt tidy run clean check check-eol

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

## 换行符自检：检出应为 LF 的文件是否被 CRLF 污染（只读检测，不改动任何 git 配置）
## 仓库一致性由 .gitattributes 保证，本目标仅用于发现"编辑器写盘"造成的静默漂移
check-eol:
	@bad=$$(git ls-files --eol | grep 'w/crlf' | grep -v -E '\.(ps1|cmd|bat)$$' || true); \
	if [ -n "$$bad" ]; then \
		echo "✗ 检测到 CRLF 污染（这些文件应为 LF）："; \
		echo "$$bad"; \
		echo ""; \
		echo "修复方式：git rm -r --cached . && git reset --hard HEAD"; \
		echo "（无需改动任何 git 配置，.gitattributes 已声明统一 LF）"; \
		exit 1; \
	else \
		echo "✓ 换行符检查通过：无 CRLF 污染"; \
	fi

clean:
	rm -rf $(BIN_DIR) web/dist
	@mkdir -p web/dist && touch $(DIST_KEEP)
