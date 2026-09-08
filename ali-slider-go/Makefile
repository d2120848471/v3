GO ?= go
DOCKER ?= docker
CARGO ?= cargo
BINARY := dist/ali-slider-go-linux-amd64
WINDOWS_BINARY := dist/ali-slider-go-windows-amd64.exe
V8_MANIFEST := native/v8runtime/Cargo.toml
DOCKERFILE := build/docker/Dockerfile

# 所有工程门禁默认禁用 CGo，保证测试与发布构建使用同一纯 Go 边界。
# Go 1.26 的 Linux race runtime 强制依赖 CGo，因此严格 race 门禁在
# Darwin 上运行；根 CI 已单列对应 macOS job。
export CGO_ENABLED := 0

.PHONY: fmt fmt-check vet test race staticcheck vuln cover bench native-test native-build \
	build-linux build-linux-amd64 build-linux-arm64 build-windows check check-linux

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

# 只运行离线视觉和 Mock HTTP 基准；不启用 online build tag 或真实挑战。
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/domain/vision ./internal/interfaces/httpapi

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

native-test:
	$(CARGO) test --manifest-path $(V8_MANIFEST) --locked

native-build:
	$(CARGO) build --manifest-path $(V8_MANIFEST) --release --locked

# build-linux 保留原有 Go ELF 输出；完整可运行产物还需同目录 V8 wrapper。
build-linux:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

# BuildKit package stage 同时导出 Go 服务和对应架构的 V8 wrapper。
build-linux-amd64:
	mkdir -p dist/linux-amd64
	$(DOCKER) buildx build -f $(DOCKERFILE) --platform linux/amd64 --target package \
		--output type=local,dest=dist/linux-amd64 .

build-linux-arm64:
	mkdir -p dist/linux-arm64
	$(DOCKER) buildx build -f $(DOCKERFILE) --platform linux/arm64 --target package \
		--output type=local,dest=dist/linux-arm64 .

build-windows:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o $(WINDOWS_BINARY) ./cmd/server

check: fmt-check vet test race staticcheck vuln native-test build-linux

check-linux: fmt-check vet test staticcheck vuln native-test build-linux
