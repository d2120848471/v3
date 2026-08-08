GO ?= go
BINARY := dist/ali-slider-go-linux-amd64
WINDOWS_BINARY := dist/ali-slider-go-windows-amd64.exe

# 所有工程门禁默认禁用 CGo，保证测试与发布构建使用同一纯 Go 边界。
# Go 1.26 的 Linux race runtime 强制依赖 CGo，因此严格 race 门禁在
# Darwin 上运行；根 CI 已单列对应 macOS job。
export CGO_ENABLED := 0

.PHONY: fmt fmt-check vet test race staticcheck vuln cover build-linux build-windows check check-linux

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

race:
	@test "$$($(GO) env GOOS)" = "darwin" || { \
		echo "strict CGO_ENABLED=0 race requires Darwin; use make check-linux and the CI macOS race job" >&2; \
		exit 2; \
	}
	$(GO) test -count=1 -race ./...

staticcheck:
	staticcheck ./...

vuln:
	govulncheck ./...

cover:
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

build-linux:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

build-windows:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o $(WINDOWS_BINARY) ./cmd/server

check: fmt-check vet test race staticcheck vuln build-linux

check-linux: fmt-check vet test staticcheck vuln build-linux
