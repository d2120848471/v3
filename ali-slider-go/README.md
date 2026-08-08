# Ali Slider Go

`ali-slider-go` 是原 Python 实现的独立纯 Go 重写：单一静态二进制内完成设备会话、Captcha Init、图片下载、缺口识别、轨迹/PE 数据构造和唯一一次 Verify。旧 Python 实现已从当前工作树删除；审计或回滚时可从 Git 历史提交 `0509bfd` 恢复。Go module 内只保留不含可执行 Python 的脱敏静态回归 fixture。

> 仅限自有系统或获得明确授权的测试环境。服务没有应用内鉴权，默认只监听 `127.0.0.1:8000`；不要把未加保护的端口暴露到公网。

## 已实现能力

- 严格纯 Go：运行、测试、Linux 构建和容器不依赖 Python、Node.js、浏览器、OpenCV、GoCV、CGo、动态库或子进程。
- 对外只提供 reusable Go library、`POST /api/slider`、`GET /health`、`GET /openapi.json`。
- 每个 `CertifyId` 最多尝试一次 Verify；网络结果未知也不重试。
- 通过输入校验的 `POST /api/slider` 直接进入 Solver；HTTP 层不设置本地 admission gate，也不因在途请求数主动返回 `429` 或 `Retry-After`。
- HTTP(S)、SOCKS4、SOCKS5、SOCKS5H 代理；同一轮设备、Init、图片与 Verify 固定同一路由。
- 双图并发下载、纯 Go 视觉、PE/DeviceToken、可选设备会话预热和共享连接池。
- 失败/低置信图片与脱敏指标私有落盘；服务启动时及每小时清理，library 调用方负责定期调用 `PurgeArtifacts`，默认保留 7 天；成功路径不落图。
- 普通文本日志只记录事件、trace、状态和耗时，不记录 token、`CertifyId`、代理凭据或原始正文。

当前验证快照：全项目统一覆盖率 `83.0%`，protocol `91.4%`（含 fuzz seeds），vision `91.9%`，PE `95.1%`；Go 1.26.5 下 race、vet、staticcheck、govulncheck、Linux AMD64 静态交叉构建及 Docker 非 root `/health` smoke 均通过。Device Log1/2/3 在线探针通过，耗时约 `0.53s`；Mac ARM64 纯计算 200 样本 nearest-rank `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`，满足 `P99<=100ms` 硬门槛。

2026-08-07 唯一授权候选批次已按冻结上限一次性完成：harness 直接调用 `pkg/slider.Client.Solve`，恰好处理 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试；严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，整批约 `6.16s`。该候选批次达到 `>=190/200` 和 Client 完整链 `P95<=1000ms`；1 秒是 P95 目标，不是 P99 或最大耗时保证。该批不经过 HTTP Handler，不能作为 HTTP 端到端 P95 证据。

该批次发生在 `net/http` one-shot 加固之前：当时未出现网络错误，也没有证据表明发生了透明重放，但没有对 HTTP/2 内部回卷做线级观测。最终源码已对 Verify 和一次性 Device POST 清空 `Request.GetBody` 并以离线测试锁定不可回卷；受 200 次授权上限约束，没有再跑第二批真实挑战。因此上述在线数字证明的是唯一授权候选批次，不是最终加固快照的第二份在线报告。当次还未保留精确起止时间和阶段聚合，不能事后补造，故不满足后续完整发布报告模板。完整命令、哈希与边界见 [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)。

## 快速启动

要求 Go `1.26.5` 或更高的兼容补丁版本。

```bash
cd ali-slider-go
go test -count=1 ./...
go run ./cmd/server
```

默认地址：`http://127.0.0.1:8000`。

```bash
curl --fail --silent http://127.0.0.1:8000/health

curl --silent --show-error \
  -H 'Content-Type: application/json' \
  -d '{"SceneId":"1ug4aptr","prefix":"fsgtmi"}' \
  http://127.0.0.1:8000/api/slider
```

成功或完成态业务失败都返回 HTTP `200`，调用方应同时检查：

```json
{
  "ok": true,
  "securityToken": "<redacted>",
  "VerifyCode": "T001",
  "VerifyResult": true,
  "certifyId": "<redacted>",
  "sceneId": "1ug4aptr",
  "proxied": false,
  "elapsedMs": 0,
  "timingsMs": {},
  "traceId": "000000000000"
}
```

HTTP 输入不合法时返回 `400` 且不进入 Solver；通过输入校验后，每个请求都直接调用一次 Solver。Solver 的参数错误仍返回 `400`，完成态业务结果返回 `200`，协议、网络、视觉、内部错误、超时或 panic 返回脱敏 `500`。`POST /api/slider` 的这些响应都带 `X-Trace-ID`，并与响应体 `traceId` 一致。

`SceneId/sceneId`、`prefix/Prefix`、`AaduaneId/aaduaneId`、`proxy/Proxy` 都兼容；同组同时出现时规范字段优先。完整合同见 [docs/api.md](docs/api.md)。

## Go library

```go
package main

import (
	"context"
	"log"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

func main() {
	options := slider.DefaultClientOptions()
	client, err := slider.NewClient(options)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Prime 会发送设备 Log1/2/3；仅在已授权环境主动调用。
	if err := client.Prime(context.Background()); err != nil {
		log.Printf("prewarm degraded: %v", err)
	}

	result, err := client.Solve(context.Background(), slider.Request{})
	if err != nil {
		log.Fatal(err)
	}
	_ = result // 不要把完整 Result 写入日志。
}
```

`Client` 可并发复用；每次 `Solve` 的挑战、token、图片、轨迹和 Verify 状态独立。`Close` 会拒绝新工作、等待正在执行的调用返回，再关闭设备会话与连接池。

## 配置

优先级固定：命令行参数 > `ALI_SLIDER_*` 环境变量 > 默认值。

```bash
go run ./cmd/server \
  --host=127.0.0.1 \
  --port=8000 \
  --max-concurrency=32 \
  --timeout=25s \
  --device-prewarm=32
```

`--max-concurrency` 是兼容旧配置名，只限定 Client 每个 route/host 的出站连接数，并作为设备会话预热资源预算；它不限制 HTTP 在途请求数，也不会触发本地 `429`。`--device-prewarm` 必须位于 `0..max-concurrency`。

预热会把设备 Log1/2/3 移出请求关键路径，但启动时会产生对应外部请求，并固定进程画像；设置 `--device-prewarm=0` 可完全关闭。所有参数、环境变量与边界见 [docs/configuration.md](docs/configuration.md)。

## Docker

构建 Linux AMD64 镜像：

```bash
docker build --platform linux/amd64 -t ali-slider-go:local .
```

镜像保持应用默认回环监听。需要从宿主访问时必须显式改为容器内 `0.0.0.0`，并建议只发布到宿主回环：

```bash
docker run --rm \
  -e ALI_SLIDER_HOST=0.0.0.0 \
  -p 127.0.0.1:8000:8000 \
  ali-slider-go:local
```

镜像使用非 root UID/GID `65532`、`scratch` 运行层、Go 1.26.5 构建层和静态二进制。当前 Linux AMD64 镜像构建及关闭预热后的非 root `/health` smoke 已验证通过；复现方式见 [docs/testing.md](docs/testing.md)。

## 质量门禁

```bash
make fmt-check
make vet
make test
make race
make staticcheck
make vuln
make cover
make build-linux
```

`staticcheck` 与 `govulncheck` 由开发/CI 环境提供，不写入生产 `go.mod`。CI 位于根目录 [`.github/workflows/ali-slider-go-ci.yml`](../.github/workflows/ali-slider-go-ci.yml)，路径隔离，不运行真实挑战。

所有项目测试和发布构建都固定 `CGO_ENABLED=0`。Go 1.26 的 Linux race runtime 强制要求 CGo，因此 CI 把严格 race 门禁放在 Darwin job；Linux job继续以 `CGO_ENABLED=0` 运行 unit/integration、覆盖率、静态分析和静态构建。Linux 本地使用 `make check-linux`，完整 `make check` 在 Darwin 基线运行。

## 项目结构

```text
ali-slider-go/
├── cmd/server/            HTTP 服务启动器
├── pkg/slider/            可复用公共 Client 与稳定合同
├── internal/server/       HTTP、OpenAPI、输入校验、trace 与 timeout
├── internal/challenge/    完整编排、Captcha RPC、下载、transport、预热池
├── internal/device/       画像、指纹、Log1/2/3、DeviceToken 会话
├── internal/pe/           纯 Go PE data 构造与自检
├── internal/protocol/     编码、签名、AES、token、data codec
├── internal/vision/       PNG 安全解码与缺口识别
├── internal/track/        嵌入轨迹、缩放与扰动
├── internal/artifact/     脱敏失败样本与过期清理
├── docs/                  架构、API、配置、测试、性能与安全文档
├── Dockerfile
└── Makefile
```

## 文档

- [架构与状态所有权](docs/architecture.md)
- [HTTP API 与 OpenAPI](docs/api.md)
- [配置参考](docs/configuration.md)
- [测试与质量门禁](docs/testing.md)
- [性能口径与验收](docs/performance.md)
- [安全边界](docs/security.md)
- [Python → Go 迁移](docs/migration.md)
- [故障排查](docs/troubleshooting.md)
- [2026-08-08 无本地 admission 验证证据](docs/evidence/validation-2026-08-08-no-local-admission.md)
- [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)
- [变更记录](CHANGELOG.md)

## 验收边界

- 纯本地计算硬门槛：Mac ARM64、200 样本 nearest-rank `P99=57.05075ms`，已满足 `P99<=100ms`；benchmark 均值不能替代该分位数结果。
- 热态、直连、无排队 Client 完整求解链：候选批次墙钟 `P95=984ms`，达到 `P95<=1000ms`；`P99=1018ms`、`max=1555ms`，不得声称所有调用或 P99 均小于 1 秒，也不得改写成 HTTP 端到端 P95 已通过。
- 2026-08-07 授权在线候选批次：恰好 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`，成功率 `98%`，门槛通过；one-shot 加固后的最终源码未获授权再跑第二批。
- 普通单测、CI、Docker 构建都不得运行该在线批次。

2026-08-07 的短批次已证明冻结的成功率和 Client 完整链 P95 目标；同等 32 路 Client harness 的 RSS、GC、goroutine 峰值及长时间稳定性压测仍未完成，不得从约 6.16 秒的批次或离线 Mock 测试外推长期容量，也不得把并发 32 解释为 HTTP admission 上限。
