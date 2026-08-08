# 2026-08-08 无本地 admission 验证证据

本页固化“删除 HTTP Handler 本地并发准入闸门与主动 429”的离线验证。本轮不改 DeviceSession 池、Transport 连接预算或 Solver 协议链，没有访问 Device/Captcha/CDN，没有生成新挑战。

> **发布边界**：当前工作树未提交。本页使用选定源码/fixture 摘要、临时目录中的 Linux 二进制哈希和本地 Docker image digest 标识快照，不是发布 manifest、SBOM 或 commit attestation。

## 1. 合同结论

- 每个通过 JSON、字段、prefix 和 proxy 校验的 `POST /api/slider` 都在继承请求取消信号的 timeout context 中直接调用一次 Solver。
- Handler 不维护活动请求数、不做 admission、不因本机在途数主动返回 429，也不生成 `Retry-After`、`TooManyChallenges` 或动态退避字段。
- `POST /api/slider` 的当前应用响应语义为：输入错误 400，完成态业务拒绝 200 + `ok=false`，成功 200，协议/网络/视觉/内部错误或 panic 脱敏映射为 500。
- `MaxConcurrency` 作为兼容旧名保留，只是 Client 每 route/host 出站连接与设备预热资源边界；它不限制 HTTP 请求进入或 `Client.Solve` 调用数。
- 外层反向代理/API gateway 仍可按自身策略返回 429。上游非 2xx（包括上游 429）仍按 Solver 网络错误映射为本服务 500，不透传上游状态或退避头。

## 2. 结论矩阵

| 门禁 | 实测 | 判定与边界 |
|---|---:|---|
| 64 个并发合法 HTTP 请求进入 Solver | `64/64`，释放后全部 200，无 `Retry-After` | 通过；这是无本地 gate 的合同回归，不是生产容量报告 |
| OpenAPI Solve 响应 | 只含 200/400/500；无 429 和旧 overload 字段 | 通过 |
| 定向 package test | server/config/slider/cmd 全部 PASS | 通过 |
| 全量 test | 12/12 package PASS | 通过 |
| 全量 race | 12/12 package PASS，`CGO_ENABLED=0` | 通过 |
| vet / staticcheck / govulncheck / gofmt | PASS / PASS / No vulnerabilities found / clean | 通过；漏洞库更新时间见第 4 节 |
| 统一覆盖率 | 全项目 `83.0%`，server `88.2%` | 通过既有 `>=80%` 项目门槛 |
| Linux AMD64 静态构建 | ELF x86-64、statically linked、stripped | 通过；产物仅在独立临时目录生成 |
| Linux AMD64 Docker smoke | runtime 用户 `65532:65532`；`/health` 返回 ready | 通过；关闭预热，只访问宿主回环 |
| 真实在线挑战 | `0` | 本轮不需要且未执行 |

## 3. 快照摘要

按照 2026-08-07 证据页中相同的选择规则，当前集合仍为 44 个“非测试 Go 源码 + `go.mod` + `Dockerfile` + JSON/PNG fixture”文件：

```bash
find . -type f \( \( -name '*.go' ! -name '*_test.go' \) \
  -o -name go.mod -o -name Dockerfile -o -name '*.json' -o -name '*.png' \) -print0 |
  sort -z |
  xargs -0 shasum -a 256 |
  sed 's#  \./#  #' |
  shasum -a 256
```

```text
03d0d63a5525f9faae3c975023dd7ef218559549a14b5e1acf617a928031bb48  -
```

该摘要不包含测试源码、Makefile、CI、文档或本页，因此只能用来区分选定生产源码/fixture 快照。

## 4. 可复现离线验证

工具版本：

```text
go version go1.26.5 darwin/arm64
staticcheck 2026.1 (v0.7.0)
govulncheck v1.1.4
vulnerability DB updated: 2026-07-27 20:14:16 +0000 UTC
```

定向合同验证：

```bash
go test -count=1 ./internal/server ./internal/config ./pkg/slider ./cmd/server
go test -count=20 -run '^TestConcurrentRequestsAlwaysEnterSolver$' ./internal/server
```

两条命令均 PASS。第二条重复 20 次执行 64 并发直通合同。

全量质量门禁：

```bash
CGO_ENABLED=0 GOPROXY=off go test -count=1 ./...
CGO_ENABLED=0 GOPROXY=off go test -race -count=1 ./...
CGO_ENABLED=0 GOPROXY=off go vet ./...
staticcheck_bin=/tmp/ali-slider-go-tools-codex/bin/staticcheck
govulncheck_bin=/tmp/ali-slider-go-tools-codex/bin/govulncheck
CGO_ENABLED=0 GOPROXY=off "$staticcheck_bin" ./...
CGO_ENABLED=0 GOPROXY=off "$govulncheck_bin" ./...
test -z "$(gofmt -l .)"
```

结果：12 个 package 的 test 与 race 全部 PASS；vet/staticcheck/gofmt PASS；`govulncheck` 输出 `No vulnerabilities found.`。两个固定版本工具安装在隔离的 `/tmp/ali-slider-go-tools-codex`，未写入生产依赖或全局工具目录；生产 `go.mod` 仍无第三方依赖。

统一覆盖率：

```bash
validation_dir=$(mktemp -d)
CGO_ENABLED=0 GOPROXY=off \
  go test -count=1 -coverprofile="$validation_dir/coverage.out" ./...
go tool cover -func="$validation_dir/coverage.out" | tail -n 1
```

```text
total: (statements) 83.0%
internal/server: 88.2%
internal/protocol: 91.4%
internal/vision: 91.9%
internal/pe: 95.1%
```

Linux AMD64 构建：

```bash
validation_dir=$(mktemp -d)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOPROXY=off \
  go build -trimpath -ldflags='-s -w -buildid=' \
  -o "$validation_dir/ali-slider-go-linux-amd64" ./cmd/server
file "$validation_dir/ali-slider-go-linux-amd64"
shasum -a 256 "$validation_dir/ali-slider-go-linux-amd64"
```

```text
ELF 64-bit LSB executable, x86-64, statically linked, stripped
b36e5b11e7619987f75072046bc27c773bca2adb65ec2b639df8282702a83daa
```

Linux AMD64 Docker 构建与健康检查：

```bash
docker build --platform linux/amd64 \
  -t ali-slider-go:validation-20260808 .

docker run -d --rm \
  --name ali-slider-go-validation-20260808 \
  -e ALI_SLIDER_HOST=0.0.0.0 \
  -e ALI_SLIDER_DEVICE_PREWARM=0 \
  -p 127.0.0.1::8000 \
  ali-slider-go:validation-20260808

docker inspect ali-slider-go-validation-20260808 \
  --format '{{.Config.User}} {{.State.Status}} {{.State.Running}}'
docker port ali-slider-go-validation-20260808 8000/tcp
curl --fail --silent --show-error http://127.0.0.1:<动态端口>/health
docker stop ali-slider-go-validation-20260808
```

```text
image: sha256:83ac9824e462bdda66873ff0ddd04bf074c200334b0b00e1a180eeca81bf332e
platform: linux/amd64
runtime: 65532:65532 running true
health: {"ok":true,"status":"ready"}
container: stopped and removed
```

运行容器时显式设置 `ALI_SLIDER_DEVICE_PREWARM=0`，只访问宿主回环 `/health`；未触发设备预热或真实挑战。验证镜像保留在本地，供后续复核。

## 5. Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/server/server.go` · `handleSolve` / `callSolver` | 合法请求不经本地 admission，直接在 timeout context 中调用 Solver；panic 仍被脱敏映射。 | POST → decode/validate → timeout context → Solve ×1 → 200/400/500 |
| `internal/server/openapi.go` · `openAPIDocument` | Solve 合同只声明 200/400/500，无本地 429 或退避 header。 | GET `/openapi.json` → responses → current contract |
| `internal/server/server_test.go` · `TestConcurrentRequestsAlwaysEnterSolver` | 64 个并发合法请求全部进入阻塞 Solver，释放后全部 200 且无 `Retry-After`。 | 64 POST → 64 Solver entries → release → 64 responses |
| `internal/server/server_test.go` · OpenAPI 回归 | 运行时 schema 不再宣告 `429/Retry-After/TooManyChallenges` 和旧 overload 字段。 | generated document → negative assertions → contract gate |
| `internal/config/config.go` 与 `pkg/slider/client.go` · `MaxConcurrency` | 兼容字段仅作出站连接/预热资源预算，不做 HTTP/Solve admission。 | flag/env → ClientOptions → TransportPool/DevicePrewarm |
| 本页第 4 节命令输出 | 变更后的测试、race、静态分析、覆盖率和 Linux 静态构建均通过。 | source snapshot → offline gates → candidate binary |
| 当前 Docker image digest、runtime user 与 `/health` 输出 | 本轮源码可构建为 Linux AMD64 scratch 镜像，并以非 root 运行健康检查。 | source snapshot → image → loopback smoke → container removed |

## 6. 未扩大的结论

- 64 并发回归只证明“旧 32 路 gate 已不存在”，不证明本机、容器或生产环境可无界并发稳定运行。
- Docker smoke 只证明当前镜像的启动、非 root 身份与本机 `/health`；不证明 Solver 在线链路、吞吐或容量。
- Transport 连接预算、route 上限、DeviceSession 库存、请求 timeout 和操作系统资源仍会形成自然等待或失败。
- 在无外层控制的流量峰值下，可能出现连接等待、超时风暴、goroutine 增长、GC 压力、内存耗尽或进程退出。当前没有生产长稳结论。
- 2026-08-07 的 `200/32`、`196/200`、Client P95 `984ms` 仍是当时 Client harness 的历史实测；它不经过 HTTP Handler，不是当前 HTTP admission 或无界并发证据。
