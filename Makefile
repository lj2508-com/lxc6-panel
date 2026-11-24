BINARY  := webmgr
VERSION := $(shell grep -oP 'version\s*=\s*"\K[^"]+' main.go 2>/dev/null || echo dev)
LDFLAGS := -s -w
GOFLAGS := -trimpath

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build release clean fmt vet test help

all: build

## build: 为当前平台编译
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o build/$(BINARY) .
	@echo "==> build/$(BINARY)"

## release: 交叉编译多平台
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "==> $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done
	@echo "==> dist/ 完成"

## fmt: 格式化
fmt:
	gofmt -w *.go

## vet: 静态检查
vet:
	go vet ./...

## test: 运行测试
test:
	go test ./...

## clean: 清理产物
clean:
	rm -rf build dist

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
