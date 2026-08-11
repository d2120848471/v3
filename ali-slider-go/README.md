# Ali Slider Go

`ali-slider-go` 是原 Python 实现的 Go 重写：Go 主进程负责编排、RPC、图片下载、缺口识别、轨迹和唯一一次 Verify；动态 Device SDK 和 PE oracle 在同进程 V8 `149.4.0` 中执行，不启动 Node 子进程。设备侧从 Init 到 Complete 保持同一个 FeiLin/V8 会话；PE 侧按精确 `StaticPath` 用当前 V8 采样，与纯 Go Builder 的 payload、事件和时钟做完整差分，仅差分为零时在 TTL 内纯算本轮 `data`，其余保留 V8 fallback。SDK 每 5 分钟字节复核，PE 最多 30 分钟强制重采样；token、`CertifyId`、轨迹和 `data` 不跨轮复用。旧 Python 实现已从当前工作树删除；审计或回滚时可从 Git 历史提交 `0509bfd` 恢复。

> 仅限自有系统或获得明确授权的测试环境。服务没有应用内鉴权，默认只监听 `127.0.0.1:8000`；不要把未加保护的端口暴露到公网。

## 已实现能力

- Go 主链不依赖 Python、浏览器、OpenCV、GoCV 或 CGo；通过 `purego` 加载 Rust `cdylib` 封装的 V8/ICU，生产包不含 Node。
- 对外只提供 reusable Go library 和四个 HTTP 路径：`GET /` 内嵌 API 测试页、`GET|POST /api/slider`、`GET /health`、`GET /openapi.json`；Solve 的 GET 仅作为已废弃的旧 Python query 兼容入口。
- 每个 `CertifyId` 最多尝试一次 Verify；网络结果未知也不重试。
- 通过浏览器跨源与输入校验的 GET/POST Solve 请求直接进入 Solver；HTTP 层不设置本地 admission gate，也不因在途请求数主动返回 `429` 或 `Retry-After`。
- HTTP(S)、SOCKS4、SOCKS5、SOCKS5H 代理；同一轮 V8 设备会话、Init、公开脚本、图片与 Verify 固定同一路由。
- 双图并发下载、纯 Go 视觉、精确 PE 动态自校验 + 纯算快路/V8 fallback、PE/DeviceToken 合同自检、可选设备会话预热和共享连接池。
- 失败/低置信图片与脱敏指标私有落盘；服务启动时及每小时清理，library 调用方负责定期调用 `PurgeArtifacts`，默认保留 7 天；成功路径不落图。
- 普通文本日志只记录事件、trace、状态和耗时，不记录 token、`CertifyId`、代理凭据或原始正文。

当前运行时验证：Linux AMD64/ARM64 均已通过 Rust wrapper 单测和 Go→V8 实际 ABI 测试；Windows AMD64 DLL 已交叉构建，原生执行由 Windows CI 门禁负责。Linux AMD64 当前公开分片的强制 V8/纯 Go解包、事件和时钟差分为0；最终安全候选真实50次为 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`，3次非成功结果均为业务 `F015`，分类错误为0。P50显著快于优化前 `4722ms` 基线，但并发10下平均仍未达到约1秒。

2026-08-07 唯一授权候选批次已按冻结上限一次性完成：harness 直接调用 `pkg/slider.Client.Solve`，恰好处理 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试；严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，整批约 `6.16s`。该候选批次达到 `>=190/200` 和 Client 完整链 `P95<=1000ms`；1 秒是 P95 目标，不是 P99 或最大耗时保证。该批不经过 HTTP Handler，不能作为 HTTP 端到端 P95 证据。

该批次发生在 `net/http` one-shot 加固之前：当时未出现网络错误，也没有证据表明发生了透明重放，但没有对 HTTP/2 内部回卷做线级观测。最终源码已对 Verify 和一次性 Device POST 清空 `Request.GetBody` 并以离线测试锁定不可回卷；受 200 次授权上限约束，没有再跑第二批真实挑战。因此上述在线数字证明的是唯一授权候选批次，不是最终加固快照的第二份在线报告。当次还未保留精确起止时间和阶段聚合，不能事后补造，故不满足后续完整发布报告模板。完整命令、哈希与边界见 [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)。

## 快速启动

### Windows 便携包

不安装开发环境即可使用：在 GitHub **Actions** 的 `ali-slider-go-ci` 最新成功运行中下载 `ali-slider-go-windows-amd64.zip`，完整解压后双击 `start.bat`，等待 `event=listen status=ready`。

这是本机控制台 HTTP 服务，不是桌面 GUI；服务内置浏览器 API 测试页。包内包含 Go 服务 EXE、`ali_slider_v8_runtime.dll`、第三方许可说明、中文说明、构建信息和 SHA-256；支持 Windows 10 / Windows Server 2016 或更高版本的 AMD64/x64 机器，默认监听 `127.0.0.1:8000`。测试页需现代 Edge、Chrome 或 Firefox；没有现代浏览器时 EXE 和 PowerShell/API 仍可用。完整下载、运行、SmartScreen 和 NTFS ACL 说明见 [Windows AMD64 便携包](docs/windows.md)。

### 源码运行

源码构建要求 Go `1.26.5` 和 Rust `1.88.0`；Docker 镜像或 Windows 便携包已包含 V8 wrapper，运行机不需安装 Go、Rust 或 Node。Linux 源码启动示例：

```bash
cd ali-slider-go
cargo build --manifest-path native/v8runtime/Cargo.toml --release --locked
go test -count=1 ./...
ALI_SLIDER_V8_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.so" \
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

`TRACELESS` 与图片拼图使用同一公共结果格式。服务返回阿里 Verify 的 `securityToken`、`VerifyCode`、`VerifyResult`、`certifyId` 和本轮 `sceneId`；调用方自行组装业务需要的 `captcha_verify_param`，服务不预拼接该字段。

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
	if err := client.CheckRuntime(); err != nil {
		log.Fatal(err)
	}

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

`Client` 可并发复用；每次 `Solve` 的挑战、token、图片、轨迹、PE data 和 Verify 状态独立。`CheckRuntime` 可在接流量前校验动态库、C ABI 和 V8/ICU 初始化；HTTP 服务已在报告 ready 前自动调用。`Close` 会拒绝新工作、等待正在执行的调用返回，再关闭 V8 设备 Isolate、wrapper 与连接池。

## 配置

优先级固定：命令行参数 > `ALI_SLIDER_*` 环境变量 > 默认值。

```bash
go run ./cmd/server \
  --host=127.0.0.1 \
  --port=8000 \
  --max-concurrency=32 \
  --timeout=25s \
  --device-prewarm=4 \
  --device-reserve=0 \
  --v8-library=/opt/ali-slider/libali_slider_v8_runtime.so
```

`--max-concurrency` 是兼容旧配置名，只限定 Client 每个 route/host 的出站连接数和 PE 资源预算；它不限制 HTTP 在途请求数，也不会触发本地 `429`。`--device-prewarm` 必须位于 `0..min(max-concurrency,4)`。`--device-reserve` 为 `0..4`，要求prewarm非0且两者之和不得超过4；真实A/B显示reserve会加重争用，因此默认0。

预热会提前建立含 FeiLin 状态的 V8 设备 Isolate 和 Log1/2/3 会话，因此启动时会产生对应外部请求。每个 live slot 使用独立画像；同一 slot 内画像只读，Complete 后只回收外层 Isolate，下一轮重建独立浏览器 context、session ID 和 token。默认直连池满4槽时，新请求等待已有槽位 recycle，不会额外创建同池 Device VM，也不会把 DeviceToken 缓存几分钟。代理或非默认 prefix 不跨池复用，而是走本轮冷会话。预热会话默认最多空闲20秒；设置 `--device-prewarm=0` 可完全关闭。所有参数、环境变量与边界见 [docs/configuration.md](docs/configuration.md)。

`--v8-library` 指向当前平台的 wrapper：Linux 为 `libali_slider_v8_runtime.so`，Windows 为 `ali_slider_v8_runtime.dll`。服务通过同一路由访问严格 HTTPS 白名单内的公开 SDK/PE；SDK 软 TTL 为 5 分钟，精确 PE/profile 硬 TTL 为 30 分钟。未知或到期分片先用当前 V8 运行假输入，只有与纯 Go 完整差分一致才开启快路。每轮仍使用本轮 `CertifyId`、DeviceConfig、轨迹和时钟，因此不是把设备、key 或 `data` 写死。

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

镜像使用非 root UID/GID `65532`、Debian bookworm-slim 运行层、Go 1.26.5 构建层、Rust 1.88.0 V8 构建层和 `CGO_ENABLED=0` 的 Go launcher。最终形态是 Go launcher + 同架构 V8 `.so` + glibc，不是 `scratch`/单静态 ELF，也不含 Node；复现方式见 [docs/testing.md](docs/testing.md)。

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
├── internal/pe/           动态 PE/设备 V8 运行时、脚本缓存与独立合同自检
├── internal/v8runtime/    无 CGo 动态库加载、C ABI、Isolate 生命周期与 Go host 回调
├── internal/protocol/     编码、签名、AES、token、data codec
├── internal/vision/       PNG 安全解码与缺口识别
├── internal/track/        嵌入轨迹、缩放与扰动
├── internal/artifact/     脱敏失败样本与过期清理
├── native/v8runtime/      Rust cdylib、V8/ICU 封装、锁定依赖与许可说明
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
- [2026-08-09 动态 PE 与完整 Solve 性能证据](docs/evidence/validation-2026-08-09-performance.md)
- [2026-08-11 PixCake TRACELESS 无痕验证码适配报告](docs/2026-08-11_js-web-pixcake-traceless-report.md)
- [2026-08-07 脱敏验证证据](docs/evidence/validation-2026-08-07.md)
- [变更记录](CHANGELOG.md)

## 验收边界

- 历史纯 Go/视觉计算基线：Mac ARM64、200 样本 nearest-rank `P99=57.05075ms`，当时满足 `P99<=100ms`；该测试不包含当前精确分片首次 V8 自校验、Device 或网络，不能作为完整生产链性能结论。
- 历史热态、直连、无排队 Client 完整链：2026-08-07 旧近似 PE 路径墙钟 `P95=984ms`、`P99=1018ms`、`max=1555ms`；不得改写成当前架构或 HTTP 端到端 P95 已通过。
- 2026-08-07 历史授权候选批次：恰好 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`。这些数字属于后来证明缺少动态 Device/PE 边界的实现，只保留作对照。
- 2026-08-08 针对当前动态 PE 修复快照执行了 1 个新挑战、一次 Solve、零重试，得到 `T001`、`VerifyResult=true` 和非空 token；该单次 smoke 只证明当前 Device/PE 合同恢复，不构成成功率或性能报告。
- 2026-08-09 最终安全候选真实50次为 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`；3次均为业务 `F015`，协议/网络/视觉错误为0；32/32精确分片通过V8/纯Go差分。默认直连组件A/B同时证明5个对齐存活的Device VM已破坏字段合同，因此预热池默认和硬上限为4，不能用扩大同池VM数量换速度。
- 普通单测、CI、Docker 构建都不得运行挑战批次；公开 CDN/Device 组件探针也必须用独立环境变量显式启用。

2026-08-07 的短批次只证明旧实现当时达到冻结的成功率和 Client 完整链 P95 目标。2026-08-09 当前路径已有 50 次 Client 冷态数据，但仍缺 HTTP 端到端、RSS、GC、goroutine/线程峰值及长时间稳定性报告。不得从历史批次、后半稳定窗口或离线 Mock 外推当前容量，也不得把并发 32 解释为 HTTP admission 上限。
