# 架构说明

## 1. 系统定位

`ali-slider-go` 是独立 Go module，提供可并发复用的 `slider.Client` 和四个 HTTP 路径：`GET /`、`GET|POST /api/slider`、`GET /health` 和 `GET /openapi.json`。生产主链不依赖 Python、Node 进程、浏览器、OpenCV、GoCV 或 CGo；Go 通过 `purego` 加载 Rust `cdylib`，在同进程 V8 `149.4.0` Isolate 中执行逐挑战的设备 SDK/动态 PE。公开 SDK/PE 源码及结构画像缓存 5 分钟，但 DeviceToken、`CertifyId`、轨迹和 `data` 始终属于单轮。`GET /` 返回编译进 EXE 的第一方浏览器测试页，不代表服务端启动浏览器。

完整单轮编排已实现：

```text
同一 V8/FeiLin Isolate：Device Log1/Log2/Log3
  → InitCaptchaV3
  → 按精确 StaticPath 解析/缓存公开 SDK/PE 画像
  → 并发下载背景图与滑块图
  → 纯 Go 视觉定位
  → 当前动态 PE 原生生成 Data/getter/事件
  → 同一 FeiLin VM 回放事件并执行 Device 最终 Log2
  → 唯一 VerifyCaptchaV3
```

本地与 Mock 质量门槛已建立。当前内嵌 V8 生产 `KeyResolver` 的公开 Device/PE 组件探针在 Linux AMD64 最终包中以 `2.78s` 通过，Complete 后为 142 个指纹字段、4 个请求，`dataLength=1164`；它不创建 Captcha Init/Verify。2026-08-08 之前的单挑战 `T001` 及 2026-08-07 `196/200`、Client `P95=984ms` 均属于旧运行时路径。这些有限实测不能推导当前 V8 架构的完整成功率、性能或长时间资源稳定性。

Device RPC 响应按 action 解码：顶层 Code 统一校验，只有 Log1 将 `ResultObject` 解为对象并提取 `DeviceConfig`；Log2/Log3 允许非对象成功结果。该边界防止把真实上游的合法 schema 差异误归为协议失败。

## 2. 总体数据流

```mermaid
flowchart LR
    caller["授权调用方<br/>POST JSON / deprecated GET query"] --> http["server.Handler<br/>Origin / JSON|query / Trace / Timeout<br/>合法 Solve 直接调用"]
    page["内嵌 API 测试页<br/>GET / · 手工 POST"] --> http
    http --> client["slider.Client"]

    subgraph one_request["单轮私有状态"]
        setup["固定路由与设备画像"]
        device["V8 Device Isolate<br/>同一 FeiLin 状态<br/>Log1 → Log2 → Log3"]
        init["InitCaptchaV3"]
        pekey["KeyResolver<br/>公开 SDK/PE + 画像<br/>精确路径 · 5 分钟"]
        assets["双图并发下载<br/>首错取消"]
        vision["vision.Solve"]
        pe["V8 PE Isolate<br/>本轮 data/getter/events<br/>禁网·Go 独立复核"]
        complete["同一 Device VM Complete<br/>事件回放 + 最终 Log2"]
        verify["VerifyCaptchaV3<br/>不可逆尝试位"]
    end

    client --> setup --> device --> init --> pekey --> assets --> vision --> pe --> complete --> verify
    verify --> result["slider.Result"] --> http

    transports["TransportPool<br/>直连保留 / route 隔离"] --> device
    transports --> init
    transports --> pekey
    transports --> assets
    transports --> verify
    v8lib["purego + V8 wrapper<br/>C ABI / ICU 77"] --> device
    v8lib --> pe
    prewarm["DeviceSessionPool<br/>容量受 Client 资源预算约束"] -.-> device
    keycache["KeyResolver<br/>SDK/PE/画像 5 分钟缓存<br/>无挑战级状态"] -.-> pekey
    failure["artifact.Store<br/>失败/低置信限额样本"] -.-> assets
    failure -.-> vision
    failure -.-> verify
```

HTTP 层先完成浏览器跨源和输入校验；合法 `POST /api/slider` JSON 与 deprecated `GET /api/slider?...` query 都不经过本地 admission gate，直接调用 Solver，也不因本机在途请求数主动返回 `429` 或 `Retry-After`。GET 与 POST 不合并参数源，form body 不受支持。Handler 和 concrete Client 都会传播超时 context；Solver 必须尊重 context，不使用无界 goroutine 伪造“强制取消”。Client 连接池或预热池的资源预算不等于 HTTP 请求上限。

deprecated GET 并非 REST 语义上的安全读取；它只为旧 Python 客户端保留，会创建真实挑战和上游请求。Handler 使用 `checkSolveOrigin` 将 GET 的同源检查副本视为 unsafe POST，以复用 Go `CrossOriginProtection`；明确跨源 POST/GET 均在 Solver 前返回 403，同源、`Sec-Fetch-Site: none` 和无浏览器头的旧客户端则允许。

内嵌测试页不依赖外部 CDN/框架/字体，只能向同源 `/health` 和 `/api/slider` 发起 fetch。页面打开只检查 health；Solve 必须由用户显式提交，执行期间防双击，不做重试或批量压测。页面 CSP、随机 nonce、默认敏感字段遮罩和无浏览器持久化只保护本页边界，不替代 API 鉴权。

## 3. 包与层级

| 层 | 包 | 责任 |
|---|---|---|
| 进程入口 | `cmd/server` | 解析配置、先 bind 端口、构造 Client、预热、定时清理、信号关闭 |
| 公共 library | `pkg/slider` | `Client` / `Request` / `Result` / 稳定错误类别与生命周期 |
| HTTP 适配 | `internal/server` | 四路径、内嵌测试页、POST JSON/deprecated GET query、跨源边界、别名/空值解析、`MaxRequestBytes` 对 JSON body/raw query 各限 64 KiB、合法 Solve 直接调用、deadline、panic 恢复、OpenAPI、脱敏日志 |
| 单轮编排 | `internal/challenge` | Solver 状态机、Captcha RPC、资产下载、路由池、设备预热池 |
| 设备协议 | `internal/device` | 画像、纯 Go oracle/兼容会话、DeviceConfig 与交互事件合同 |
| 纯计算 | `internal/vision` | PNG 边界、多信号匹配、Chamfer 兜底、置信度 |
| 纯计算 | `internal/track` | 嵌入轨迹、缩放、稀疏化和有界人类化 |
| PE/设备运行时 | `internal/pe` | 持久 V8/FeiLin 设备会话、当前动态 PE 原生执行、公开脚本/画像缓存、Data/token/getter/事件/时钟独立复核 |
| V8 FFI | `internal/v8runtime` | 无 CGo 加载 wrapper、C ABI 校验、Isolate 生命周期、可取消 eval/call 和 Go host JSON 回调 |
| 协议原语 | `internal/protocol` | AES、签名、JS/RPC 编码、DeviceToken、Data pack/unpack |
| 运行时注入 | `internal/runtimekit` | 密码学熵、UUID、时钟与可取消 Sleep |
| 证据存储 | `internal/artifact` | 只保存失败/低置信图和脱敏 metrics，保留期与硬配额 |
| 配置 | `internal/config` | defaults → env → flags → 集中校验 |

生产 `go.mod` 仅新增 `github.com/ebitengine/purego`，用于无 CGo 加载动态库；Rust wrapper 锁定 `v8=149.4.0` 和 `deno_core_icudata=0.77.0`。Python 只曾用于开发期生成脱敏 oracle fixture；Node 只保留为可选回归 oracle/上下文差异测试，不进入生产构造器、Docker 运行层或 Windows 包。

## 4. 单轮状态机与唯一 Verify

```mermaid
stateDiagram-v2
    [*] --> RequestValidated
    RequestValidated --> DeviceReady
    DeviceReady --> ChallengeInitialized
    ChallengeInitialized --> PEKeyResolved
    PEKeyResolved --> AssetsReady
    AssetsReady --> VisionAccepted
    VisionAccepted --> VerifyDataBuilt
    VerifyDataBuilt --> DeviceCompleted
    DeviceCompleted --> VerifyAttempted
    VerifyAttempted --> Finished

    RequestValidated --> FailedBeforeVerify
    DeviceReady --> FailedBeforeVerify
    ChallengeInitialized --> FailedBeforeVerify
    PEKeyResolved --> FailedBeforeVerify
    AssetsReady --> FailedBeforeVerify
    VisionAccepted --> FailedBeforeVerify
    VerifyDataBuilt --> FailedBeforeVerify
    VerifyAttempted --> FailedAfterVerifyAttempt
```

不变量：

1. 每个 Solve 创建独立 RPCClient，RPCClient 只允许一次 Init。
2. RPCClient 记录 Init 签发的 `CertifyId`；Verify 必须使用同一 ID。
3. `verifyAttempted` 在构造/HTTP 发送之前从 false 不可逆地变为 true。DNS、TLS、超时或结果未知均不回滚。
4. 动态 PE 源码/画像无法解析、低置信、图像错误、PE/getter/时钟不一致及 Device Complete 失败都在 Verify 前终止。
5. Verify 业务拒绝是完成态：返回 `200 + ok=false`，不转成交通错误，不重试。

## 5. 并发与资源所有权

| 资源 | 所有者 | 并发策略 | 释放 |
|---|---|---|---|
| HTTP 请求、trace、请求 DTO、结果 DTO | 单请求 | `net/http` 按请求调度；POST JSON 或 deprecated GET query 输入合法后直接进入 Solver，无本地 admission 槽 | 响应后 |
| Device VM Session / DeviceToken / CertifyId / Verify 位 | 单轮 | Init 与 Complete 保持同一 VM；不跨挑战复用 | Solve 返回时 close/release |
| 预热 V8 Device Session | DeviceSessionPool | `ready + pending + leased <= capacity <= MaxConcurrency`；默认最多空闲 20 秒；一次性消费 | release 关闭 Isolate 后有界回补 |
| HTTP transport | TransportPool | 按规范 route 隔离；直连保留；有界 LRU；每 route/host 连接受 Client `MaxConcurrency` 预算约束，但不限制 Solve 调用数 | Client.Close 关闭空闲连接并清表 |
| 公开 SDK/PE 源码与结构画像 | KeyResolver | 完整 `StaticPath` 隔离；miss 全局串行；同一路径和 SDK 最多复用 5 分钟；不含 token/CertifyId/轨迹/data | TTL 到期替换或 Client 进程结束 |
| 动态 PE Isolate | 单轮 | 使用缓存源码和本轮输入启动；无 host 网络权限；输出经 Go 独立复核 | Data 构造完成即关闭 |
| 图像字节和视觉 scratch | 单轮 | 输入有字节/尺寸/像素硬边界 | 阶段结束后 GC/复用 |
| 默认轨迹 fixture | 进程 | `embed` + `sync.Once`；输出切片按请求独立 | 进程关闭 |
| artifact | Store | 同目录串行配额决策；随机 O_EXCL | 超期/配额淘汰 |

Client 的关闭顺序是：禁止新工作 → 等待活动 Solve/Prime/Purge → 关预热 Isolate → 关 V8 wrapper → 关 transport。`Close` 并发幂等。

HTTP 进程入口的 `maxHeaderBytes` 是 `server.MaxRequestBytes + 32 KiB`：64 KiB 是 Handler 对 raw query 数据的真实上限，多出的 32 KiB 只留给 request line 与普通 header。该分层避免真实 `net/http.Server` 在 Handler 之前误拒恰好达到上限的旧 query。

## 6. 超时与错误分类

- Handler 为每个通过跨源和输入校验的请求建立 `Options.Timeout` deadline，并保留调用方取消信号。
- concrete Solver 内再建立同一总时限，保护直接 library 调用。
- Device RPC 可在总 context 内使用更窄的请求超时。
- JSON/query HTTP 输入错误返回 `400` 且不调用 Solver；明确的浏览器跨源 POST/deprecated GET 返回 `403 ApiOriginError` 且不调用 Solver。Solver 参数错误返回 `400`，完成态业务结果返回 `200`，技术错误、超时或 panic 返回脱敏 `500`。两种 Solve 方法都不声明 429。
- 每个 `/api/slider` 响应保留服务端 trace，`X-Trace-ID` 与响应体 `traceId` 一致。
- `NetworkError` 只表示已标记的 DNS/TLS/HTTP/响应读取/超时失败。
- `deviceSession`、`resolvePEKey`、`buildVerifyData`、`completeDevice` 会识别 V8/公开脚本错误：下载/路由失败归 `NetworkError`，wrapper 缺失、ABI/架构错误或 V8 初始化失败归 `InternalError`，路径/脚本/schema/算法不受支持归 `ProtocolError`；都在 Verify 前停止。
- HTTP 2xx 中的 JSON/schema、签名、DeviceConfig、时钟和本地不变量失败归入 `ProtocolError`。
- 上游正文、代理口令、token 和 CertifyId 不进入错误 Message、String/GoString 或普通日志。

## 7. 性能边界

声明的验收基线是 Mac ARM64 16 核/48 GiB：

- 旧纯 Go oracle 路径的 `P99<=100ms` 门槛曾以 Mac ARM64 200 样本 `57.05075ms` 通过；当前生产链新增逐挑战 V8 PE Isolate，必须重新测量，旧值不再证明当前 `buildVerifyData`。
- 热态、直连、无排队 Client 完整链目标仍为 `P95<=1000ms`；2026-08-07 历史路径为 `984ms`，当前架构只有单次 `1579ms` 功能 smoke，尚无当前 P95。
- 2026-08-07 历史 Client harness 使用并发 32，但没有经过 HTTP Handler；32 是历史测量参数，不是 HTTP admission 上限，也不代表当前 V8 Isolate 容量。
- 预热只是性能优化，失败不改变协议正确性；但会影响首批与尾延迟，必须在报告中单列。
- 同等 32 路 Client harness 的 RSS、GC、goroutine 峰值及长时间稳定性仍需独立报告。

详细口径、命令与已测/待测项见 [performance.md](./performance.md)。

## 8. Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `pkg/slider/client.go` · `Client.Solve/Prime/Close` | library 拥有完整 Solver、预热与并发安全关闭生命周期。 | caller → Client → challenge.Solver → Result |
| `internal/challenge/solver.go` · `Solver.Solve` / `completeAndVerify` | 前置协议阶段通过后只有一个 Verify 调用点。 | Device → Init → Assets → Vision → PE → Complete → Verify |
| `internal/pe/keys.go`、`v8_runtime.go`、`device_runtime.go`；`internal/v8runtime`；对应离线/显式在线测试 | 同路径并发 miss 合并；公开脚本受 HTTPS host/路径/体积/5 分钟 TTL 约束；同一 Device Isolate 跨 Open/Complete；当前 PE 按轮执行并由 Go 独立复核。 | StaticPath → bounded cache → per-challenge Device/PE Isolates → validated data/token |
| `internal/challenge/rpc.go` · `RPCClient.Init/Verify` | Init 与唯一 Verify 有实例级状态与 CertifyId 绑定。 | Init consumes attempt → issued ID → Verify consumes attempt |
| `internal/server/server.go` · `MaxRequestBytes` / `checkSolveOrigin` / `decodeQueryRequest` / `requestFromPayload` / `handleSolve` / `callSolver`；`cmd/server/main.go` · `maxHeaderBytes`；`cmd/server/main_test.go` · `TestHeaderBudgetAcceptsMaximumLegacyQuery` | HTTP 入口保留足够 request-line/header 预算，再对 POST 和 deprecated GET 拒绝明确浏览器跨源请求，分别校验 JSON body 与 query；合法请求不经过本地 admission，在请求派生的 deadline context 中调用 Solver。 | request → HTTP budget → origin + source-specific gate → timeout context → Solve → mapped response |
| `internal/server/testpage.go` · `writeTestPage`；`internal/server/web/test.html` | 第一方页面编译进二进制，以随机 nonce CSP 约束为同源手工调用且不持久化结果。 | GET `/` → embedded page → explicit POST `/api/slider` |
| `internal/config/config.go:26` · `MaxConcurrency`；`pkg/slider/client.go:91` · `NewTransportPool`；`:122` · `NewDeviceSessionPool` | `MaxConcurrency` 是 Client 每 route/host 出站连接与预热资源预算，不是 HTTP 并发阈值。 | config → ClientOptions → transport/prewarm budget；valid Solve request → Solver |
| `internal/pe/v8_runtime_online_test.go` | 生产 `KeyResolver` 通过同一 V8 Device Isolate 完成 Open/Resolve/Build/Complete；Linux AMD64 最终包实测 142 字段、4 个请求、`dataLength=1164`，不调用 Captcha Init/Verify。 | public SDK/PE + Device RPC → persistent V8 Isolate + per-call PE Isolate → validated completion |
| `internal/challenge/performance_test.go`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 离线 32 路 Mock Solver 链正确性通过；200 样本纯计算 `P99=57.05075ms`。 | fixture → concurrent Solver chain / sorted compute samples → verified gates |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的 200/32/应用层零重试候选批次严格成功 `196/200`，Client 完整链墙钟 P95 `984ms`；最终 one-shot 加固后未在线重跑。 | public Client → one Solve per job → aggregate summary → bounded Client acceptance；不经过 HTTP Handler |
| `cmd/server/main.go` · `run` | 启动先 bind，再 purge/Prime，避免端口冲突仍发外部预热。 | parse → listen → client → prime → serve |
