# Ali Slider Go

`ali-slider-go` 是原 Python 实现的 Go 重写：Go 主进程负责编排、RPC、图片下载、缺口识别、轨迹和唯一一次 Verify；Node.js 24 在每轮挑战中执行当前公开 SDK/动态 PE。设备侧从 Init 到 Complete 保持同一个 FeiLin VM，PE 侧按本轮 `StaticPath`、`CertifyId`、轨迹、时钟和 DeviceConfig 原生生成 `data`。公开 SDK/PE 源码及结构画像缓存 5 分钟，挑战级 token、`CertifyId`、轨迹和 `data` 不跨轮复用。旧 Python 实现已从当前工作树删除；审计或回滚时可从 Git 历史提交 `0509bfd` 恢复。

> 仅限自有系统或获得明确授权的测试环境。服务没有应用内鉴权，默认只监听 `127.0.0.1:8000`；不要把未加保护的端口暴露到公网。

## 已实现能力

- Go 主链不依赖 Python、浏览器、OpenCV、GoCV 或 CGo。Node.js 24 是设备指纹与动态 PE 的逐挑战运行时；Docker 镜像和 Windows 便携包均携带固定版本 `24.14.1`。
- 对外只提供 reusable Go library 和四个 HTTP 路径：`GET /` 内嵌 API 测试页、`GET|POST /api/slider`、`GET /health`、`GET /openapi.json`；Solve 的 GET 仅作为已废弃的旧 Python query 兼容入口。
- 每个 `CertifyId` 最多尝试一次 Verify；网络结果未知也不重试。
- 通过浏览器跨源与输入校验的 GET/POST Solve 请求直接进入 Solver；HTTP 层不设置本地 admission gate，也不因在途请求数主动返回 `429` 或 `Retry-After`。
- HTTP(S)、SOCKS4、SOCKS5、SOCKS5H 代理；同一轮 Node 设备会话、Init、公开脚本、图片与 Verify 固定同一路由。
- 双图并发下载、纯 Go 视觉、动态 PE 原生执行、PE/DeviceToken 合同自检、可选设备会话预热和共享连接池。
- 失败/低置信图片与脱敏指标私有落盘；服务启动时及每小时清理，library 调用方负责定期调用 `PurgeArtifacts`，默认保留 7 天；成功路径不落图。
- 普通文本日志只记录事件、trace、状态和耗时，不记录 token、`CertifyId`、代理凭据或原始正文。

当前验证快照：全项目统一语句覆盖率 `81.9%`，高于 `>=80%` 门槛；protocol `91.4%`（含 fuzz seeds），vision `91.9%`，PE `80.9%`。Go 1.26.5 下全量 test/race、vet、staticcheck、govulncheck、Linux AMD64/ARM64 与 Windows AMD64 交叉构建及 Docker 非 root `/health` smoke 均通过。当前公开 SDK/PE 的完整 Device 组件探针约 `0.85s` 通过，完成态为 142 个指纹字段、4 个请求。Mac ARM64 纯 Go/视觉 200 样本 `P99=57.05075ms` 及 2026-08-07 在线批次都属于旧近似 PE 路径，只保留为历史基线；当前 Node 生产链的正式成功率和性能仍需重新授权测量。

2026-08-07 唯一授权候选批次已按冻结上限一次性完成：harness 直接调用 `pkg/slider.Client.Solve`，恰好处理 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试；严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，整批约 `6.16s`。该候选批次达到 `>=190/200` 和 Client 完整链 `P95<=1000ms`；1 秒是 P95 目标，不是 P99 或最大耗时保证。该批不经过 HTTP Handler，不能作为 HTTP 端到端 P95 证据。

该批次发生在 `net/http` one-shot 加固之前：当时未出现网络错误，也没有证据表明发生了透明重放，但没有对 HTTP/2 内部回卷做线级观测。最终源码已对 Verify 和一次性 Device POST 清空 `Request.GetBody` 并以离线测试锁定不可回卷；受 200 次授权上限约束，没有再跑第二批真实挑战。因此上述在线数字证明的是唯一授权候选批次，不是最终加固快照的第二份在线报告。当次还未保留精确起止时间和阶段聚合，不能事后补造，故不满足后续完整发布报告模板。完整命令、哈希与边界见 [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)。

## 快速启动

### Windows 便携包

不安装开发环境即可使用：在 GitHub **Actions** 的 `ali-slider-go-ci` 最新成功运行中下载 `ali-slider-go-windows-amd64.zip`，完整解压后双击 `start.bat`，等待 `event=listen status=ready`。

这是本机控制台 HTTP 服务，不是桌面 GUI；服务内置浏览器 API 测试页。包内包含 Go 服务 EXE、Node 24.14.1 运行时及许可证、中文说明、构建信息和 SHA-256；支持 Windows 10 / Windows Server 2016 或更高版本的 AMD64/x64 机器，默认监听 `127.0.0.1:8000`。测试页需现代 Edge、Chrome 或 Firefox；没有现代浏览器时 EXE 和 PowerShell/API 仍可用。完整下载、运行、SmartScreen 和 NTFS ACL 说明见 [Windows AMD64 便携包](docs/windows.md)。

### 源码运行

要求 Go `1.26.5` 或更高的兼容补丁版本，以及 Node.js `24.14.1`。每轮求解都会使用 Node；若使用 Docker 或 Windows 便携包，无需另行安装。

```bash
cd ali-slider-go
go test -count=1 ./...
go run ./cmd/server
```

默认地址：`http://127.0.0.1:8000`。

浏览器打开 `http://127.0.0.1:8000/` 可使用内嵌测试页。页面不会自动求解或重试；只有点击“发送一次求解”才会访问真实上游。页面始终使用推荐的 POST JSON，不会生成旧 GET query。页面不使用 Cookie 或浏览器存储，默认遮罩 RPC key、代理、`securityToken` 和 `certifyId`，刷新即清除当前结果。

```bash
curl --fail --silent http://127.0.0.1:8000/health

curl --silent --show-error \
  -H 'Content-Type: application/json' \
  -d '{"SceneId":"1ug4aptr","prefix":"fsgtmi"}' \
  http://127.0.0.1:8000/api/slider
```

旧 Python GET query 仅作兼容，新代码不应继续使用：

```bash
curl --fail-with-body --get \
  --data-urlencode 'SceneId=1ug4aptr' \
  --data-urlencode 'prefix=fsgtmi' \
  http://127.0.0.1:8000/api/slider
```

该 GET 会发起真实求解，不是无副作用的读取。URL 可能进入浏览器历史、网关或代理访问日志；不要用 query 传 `AaduaneId` 或带 username/password 的代理。

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

HTTP 输入不合法时返回 `400` 且不进入 Solver；明确的浏览器跨源 GET 或 POST 返回 `403 ApiOriginError`。无浏览器来源头的 curl/程序客户端保持允许。通过跨源和输入边界校验后，每个请求都直接调用一次 Solver。Solver 的参数错误仍返回 `400`，完成态业务结果返回 `200`，协议、网络、视觉、内部错误、超时或 panic 返回脱敏 `500`。`/api/slider` 的这些响应都带 `X-Trace-ID`，并与响应体 `traceId` 一致。

`SceneId/sceneId`、`prefix/Prefix`、`AaduaneId/aaduaneId`、`proxy/Proxy` 是仅有的 8 个精确请求名称。同组同时出现时规范字段优先，同名 query 重复时最后一个值生效，空值回退默认。GET 只读 query，POST 只读 JSON body，两个参数源不合并；不支持 `application/x-www-form-urlencoded` 或 multipart form body。JSON body 和 query 分别最大 65,536 字节；非法 query URL encoding 返回 `400 ApiRequestError`。完整合同见 [docs/api.md](docs/api.md)。

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

`Client` 可并发复用；每次 `Solve` 的挑战、token、图片、轨迹、PE data 和 Verify 状态独立。`Close` 会拒绝新工作、等待正在执行的调用返回，再关闭 Node 设备会话与连接池。

## 配置

优先级固定：命令行参数 > `ALI_SLIDER_*` 环境变量 > 默认值。

```bash
go run ./cmd/server \
  --host=127.0.0.1 \
  --port=8000 \
  --max-concurrency=32 \
  --timeout=25s \
  --device-prewarm=32 \
  --pe-key-node=node
```

`--max-concurrency` 是兼容旧配置名，只限定 Client 每个 route/host 的出站连接数，并作为设备会话预热资源预算；它不限制 HTTP 在途请求数，也不会触发本地 `429`。`--device-prewarm` 必须位于 `0..max-concurrency`。

预热会提前建立含 Node/FeiLin VM 的设备 Log1/2/3 会话，但启动时会产生对应外部请求，并固定进程画像。预热会话默认最多空闲 20 秒且一次性消费，不会把 DeviceToken 缓存几分钟；设置 `--device-prewarm=0` 可完全关闭。所有参数、环境变量与边界见 [docs/configuration.md](docs/configuration.md)。

`--pe-key-node` 是为兼容既有配置保留的名称，实际指向整个设备/PE Node 运行时。服务通过同一路由访问严格 HTTPS 白名单内的公开 SDK/PE；公开源码及精确 `StaticPath` 结构画像缓存 5 分钟。每轮仍新建或租用短龄设备 VM，并用本轮 `CertifyId`、DeviceConfig、轨迹和时钟运行当前 PE，因此不是把设备或 `data` 写死。

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

镜像使用非 root UID/GID `65532`、Node `24.14.1` Alpine 运行层、Go 1.26.5 构建层和 `CGO_ENABLED=0` 的 Go 二进制。Node 运行层保证每轮设备/PE 执行不会因宿主缺少运行时而失败；复现方式见 [docs/testing.md](docs/testing.md)。

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
make build-windows
```

`staticcheck` 与 `govulncheck` 由开发/CI 环境提供，不写入生产 `go.mod`。CI 位于根目录 [`.github/workflows/ali-slider-go-ci.yml`](../.github/workflows/ali-slider-go-ci.yml)，路径隔离，不运行真实挑战。

所有项目测试和发布构建都固定 `CGO_ENABLED=0`。Go 1.26 的 Linux race runtime 强制要求 CGo，因此 CI 把严格 race 门禁放在 Darwin job；Linux job继续以 `CGO_ENABLED=0` 运行 unit/integration、覆盖率、静态分析和静态构建。Linux 本地使用 `make check-linux`，完整 `make check` 在 Darwin 基线运行。

## 项目结构

```text
ali-slider-go/
├── cmd/server/            HTTP 服务启动器
├── pkg/slider/            可复用公共 Client 与稳定合同
├── internal/server/       HTTP、内嵌测试页、OpenAPI、输入校验、trace 与 timeout
├── internal/challenge/    完整编排、Captcha RPC、下载、transport、预热池
├── internal/device/       画像、指纹、Log1/2/3、DeviceToken 会话
├── internal/pe/           动态 PE/设备 Node 运行时、脚本缓存与独立合同自检
├── internal/protocol/     编码、签名、AES、token、data codec
├── internal/vision/       PNG 安全解码与缺口识别
├── internal/track/        嵌入轨迹、缩放与扰动
├── internal/artifact/     脱敏失败样本与过期清理
├── packaging/windows/     Windows 双击入口与最终用户说明
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
- [Windows AMD64 便携包](docs/windows.md)
- [Python → Go 迁移](docs/migration.md)
- [故障排查](docs/troubleshooting.md)
- [2026-08-08 无本地 admission 历史快照证据](docs/evidence/validation-2026-08-08-no-local-admission.md)
- [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)
- [变更记录](CHANGELOG.md)

## 验收边界

- 历史纯 Go/视觉计算基线：Mac ARM64、200 样本 nearest-rank `P99=57.05075ms`，当时满足 `P99<=100ms`；该测试不包含当前逐挑战 Node PE VM，不能作为当前生产链性能结论。
- 历史热态、直连、无排队 Client 完整链：2026-08-07 旧近似 PE 路径墙钟 `P95=984ms`、`P99=1018ms`、`max=1555ms`；不得改写成当前架构或 HTTP 端到端 P95 已通过。
- 2026-08-07 历史授权候选批次：恰好 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`。这些数字属于后来证明缺少动态 Device/PE 边界的实现，只保留作对照。
- 2026-08-08 针对当前动态 PE 修复快照执行了 1 个新挑战、一次 Solve、零重试，得到 `T001`、`VerifyResult=true` 和非空 token；该单次 smoke 只证明当前 Device/PE 合同恢复，不构成成功率或性能报告。
- 普通单测、CI、Docker 构建都不得运行挑战批次；公开 CDN/Device 组件探针也必须用独立环境变量显式启用。

2026-08-07 的短批次只证明旧实现当时达到冻结的成功率和 Client 完整链 P95 目标；当前 Node Device/PE 架构仍缺正式成功率、P95/P99、RSS、GC、goroutine/进程峰值及长时间稳定性报告。不得从历史约 6.16 秒批次或离线 Mock 外推当前容量，也不得把并发 32 解释为 HTTP admission 上限。
