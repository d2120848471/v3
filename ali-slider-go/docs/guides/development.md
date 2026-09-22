# 开发与验证

以下命令从 `ali-slider-go/` 执行。普通测试使用 Mock、静态 fixture 或本地原生库，不访问真实验证码、Device RPC、CDN 或代理；依赖下载、工具获取和漏洞数据库查询可以联网。每次源码快照的通过状态以当次输出为准。

## 工具链与日常检查

Go 基线 `1.26.6`，Rust wrapper 基线 `1.88.0`，V8 `149.4.0`、ICU data `0.77.0`。版本来源为 [go.mod](../../go.mod)、[Cargo.toml](../../native/v8runtime/Cargo.toml)、[Dockerfile](../../build/docker/Dockerfile) 和 [CI](../../../.github/workflows/ali-slider-go-ci.yml)。

依赖未缓存时先下载并验证锁定版本：

```bash
go mod download
go mod verify
```

日常 Go 门禁：

```bash
export CGO_ENABLED=0
export GOPROXY=off
make fmt-check
make vet
make test
```

`make test` 执行 `go test -count=1 ./...`，包括应用 ports 的替身测试、适配器 Mock 集成、HTTP 合同与 `tests/architecture`。需要单独检查结构与公开 API 时：

```bash
go test -count=1 ./tests/architecture
```

`TestProductionDependencies` 核对生产代码的依赖方向，应用不得导入 infrastructure、HTTP 或 SDK；domain 不可依赖网络、进程等外部副作用。`TestPublicSDKCompatibility` 比较迁移前公共方法、结构字段/JSON tag 和错误常量的黄金快照。新增内部目录要保持边界，不能为让检查通过而保留旧路径空壳或更新兼容黄金值来掩盖变化。

## 测试分工

| 位置 | 覆盖内容 |
|---|---|
| `internal/domain/{device,pe,protocol,track,vision}` | 纯计算、编码、画像、轨迹、PNG/边界、静态 Python oracle 和 fuzz seeds |
| `internal/application/solve` | 假 Round 下的阶段顺序、类型分流、取消、失败前停止与释放 |
| `internal/application/service` | 默认请求值、关闭拒绝新工作、等待活动调用与资源关闭顺序 |
| `internal/infrastructure/{httpclient,aliyun,artifact}` | Fake RoundTripper、上游 schema、代理/路由、双图下载、唯一 Verify、失败样本 |
| `internal/infrastructure/engine` | Device bridge、脚本/profile 缓存、V8/纯 Go 差分与 fallback；可选 Node oracle |
| `internal/platform/v8runtime` | 真库开启时的 C ABI、host callback、取消、heap/时间边界和 Isolate 生命周期 |
| `internal/interfaces/httpapi` | 五路径、输入/别名/分源、旧 GET、Baxia、状态、脱敏、OpenAPI、内嵌页与跨源边界 |
| `internal/bootstrap`、`internal/bootstrap/server`、`cmd/server`、`pkg/slider` | 配置映射、生产组装、HTTP header 预算、进程入口与公共兼容 |
| `tests/architecture` | 生产导入方向与公共 API 黄金快照 |

64 路 Mock HTTP 同时进入 Solver 的测试证明没有本地请求闸门；它不证明真实 V8 和上游容量。32 路完整离线链同样只覆盖并发正确性。生产的 Device/V8 会话逐轮冷建并关闭，不能把脚本/profile 缓存当作挑战会话复用。

## Race、静态检查与覆盖率

```bash
make race
make staticcheck
make vuln
make cover
```

严格 `CGO_ENABLED=0` race 在 Darwin 执行，Linux 的 Go race runtime 要求 CGo。Linux 本地用 `make check-linux`，CI 的 macOS job 补齐无 CGo race；不要改成 `CGO_ENABLED=1` 后把结果记为同一门禁。

`make check` 包含 fmt-check、vet、test、race、staticcheck、vuln、native-test 和 Go Linux launcher 构建；`make check-linux` 省去本机不可用的 race。覆盖率与完整平台包构建另有独立入口。工具由开发环境或 CI 提供，不写入生产 go.mod；CI 固定 `staticcheck@v0.7.0`、`govulncheck@v1.1.4`。

覆盖率门槛保持：全项目 `>=80%`，`internal/domain/protocol` 和 `internal/domain/vision` 分别 `>=90%`。全项目必须使用同一快照的一份 profile，不能拼接不同时间的包级数字：

```bash
coverage_file="$(mktemp)"
trap 'rm -f "$coverage_file"' EXIT
go test -count=1 -coverprofile="$coverage_file" ./...
go tool cover -func="$coverage_file" | tail -n 1
```

CI 另从各核心包 profile 计算 90% 门槛。历史覆盖率只是历史快照，不能替代本轮或发布 commit 的测量。

## Oracle、fuzz 与性能

```bash
go test -count=1 ./internal/domain/protocol -run 'PythonOracle|Oracle'
go test -count=1 ./internal/domain/pe -run '^TestBuildMatchesPythonOracleSemantics$'
go test -count=1 ./internal/domain/vision -run '^TestPythonOracleEdgeDecoyFixtures$'
```

测试读取已提交的 JSON/PNG，运行时不启动 Python。PE 比较解包后的完整语义，zlib 输出不要求逐字节相同。视觉正/负 fixture 分别锁定缺口和低置信拒绝。

普通 `go test` 执行 fuzz seeds，显式短跑使用：

```bash
go test -run '^$' -fuzz '^FuzzUnpackData$' -fuzztime=1s ./internal/domain/protocol
go test -run '^$' -fuzz '^FuzzProtocolDecoders$' -fuzztime=1s ./internal/domain/protocol
go test -run '^$' -fuzz '^FuzzPercentAndFormEncoding$' -fuzztime=1s ./internal/domain/protocol
go test -run '^$' -fuzz '^FuzzPNGPairSeeds$' -fuzztime=1s ./internal/domain/vision
make bench
```

`make bench` 只运行 domain/vision 和 Mock HTTP benchmark。逐次纯计算分位数位于 `infrastructure/aliyun` 的完整离线集成测试中，命令与范围见 [性能说明](../design/performance.md)。benchmark 均值和并行吞吐不代表请求 P95/P99。

## 原生 V8 与可选 Node oracle

```bash
make native-test
make native-build
```

Go 真库集成测试需要显式选择当前平台 wrapper；下例为 Linux：

```bash
CGO_ENABLED=0 \
  ALI_SLIDER_V8_TEST_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.so" \
  go test -count=1 ./internal/platform/v8runtime ./internal/infrastructure/engine
```

未提供 `ALI_SLIDER_V8_TEST_LIBRARY` 时相关真库用例会跳过，普通 Go PASS 不表示 ABI 已执行。macOS 将文件名换成 `.dylib`，Windows 使用 `.dll`。运行库与 Go launcher 必须匹配系统及 CPU 架构。

engine 中保留可选 Node bridge/上下文差分测试；生产 `bootstrap.NewService` 固定构造 V8 路径，不依赖 Node。纯 Go Builder 在 `domain/pe`，脚本下载、采样和 fallback 在 `infrastructure/engine`，包职责和路径变更见 [迁移指南](migration.md)。

## 平台构建与便携包

```bash
make build-linux
make build-windows
make build-linux-amd64
make build-linux-arm64
```

前两个目标只生成 Go 主程序；Linux 双架构 BuildKit 目标导出 launcher、同架构 `.so` 与第三方 notices。`native/v8runtime/Cargo.toml` 路径保持不变；Dockerfile 已迁至 `build/docker/Dockerfile`，context 仍为 module 根目录。

本地 Docker 原生测试入口：

```bash
docker buildx build --platform linux/amd64 \
  -f build/docker/Dockerfile --target native-test --progress plain .
docker buildx build --platform linux/amd64 \
  -f build/docker/Dockerfile --target v8-test --progress plain .
```

构建与无上游 smoke：

```bash
docker build --platform linux/amd64 \
  -f build/docker/Dockerfile -t ali-slider-go:test .
docker run --detach --rm --name ali-slider-go-smoke \
  --env ALI_SLIDER_HOST=0.0.0.0 \
  --publish 127.0.0.1:8000:8000 \
  ali-slider-go:test --device-prewarm=0
curl --fail --silent http://127.0.0.1:8000/health
docker stop ali-slider-go-smoke
```

镜像为 Debian/glibc，使用非 root UID/GID `65532`，包含 V8 wrapper。Go 禁用 CGo 不等于完整运行环境静态或单文件。Docker/Go/Rust 的依赖获取可能联网，项目测试与 smoke 不请求验证码上游。

Windows 原生 CI 在 `windows-2025` 运行 Go/Rust/真 DLL 测试，再组装并解压最终 ZIP：检查七文件白名单、PE/DLL、README BOM、SHA-256、含中文/空格路径的 `start.bat`、参数转发、页面/health/OpenAPI、跨源 403 和非法输入 400。smoke 的异常输入在 Solver 前拒绝，避免意外请求上游。源码位于 [build/packaging/windows](../../build/packaging/windows)，包内路径不变，见 [Windows 指南](windows.md)。

## CI 与在线入口

[工作流](../../../.github/workflows/ali-slider-go-ci.yml) 保留 quality、Darwin race、Linux AMD64/ARM64 runtime/package 和 Windows package 全部门禁，官方 actions 继续固定完整 commit SHA。测试不启用 `online` tag，也不配置真实目标凭据；PR 只验证，符合条件的 push/手动运行才上传产物。

需要检查显式在线测试仍能编译时，使用只编译、不匹配用例的入口：

```bash
go test -tags=online -run '^$' ./...
```

生产 V8 组件探针在 [engine/v8_runtime_online_test.go](../../internal/infrastructure/engine/v8_runtime_online_test.go)，兼容 Device 探针在 [aliyun/device/online_session_test.go](../../internal/infrastructure/aliyun/device/online_session_test.go)，完整挑战验收在 [bootstrap/online_acceptance_test.go](../../internal/bootstrap/online_acceptance_test.go)。在线执行还需要对应环境开关及覆盖目标、数量、并发与时间窗的明确授权；旧批次授权和成功记录不能复用成新一轮授权。以上编译命令不会开启这些开关。

线上样本、完整 Client/HTTP 延迟和平台原生运行各自验证不同事实，必须分别记录；历史报告见 [归档索引](../README.md#历史证据)。
