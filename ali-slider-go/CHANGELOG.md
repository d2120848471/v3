# Changelog

本文件记录 `ali-slider-go` 的用户可见变化。格式参考 Keep a Changelog，版本遵循语义化版本。

> **发布状态**：`1.0.0` 仍为 Unreleased。纯 Go 完整实现、离线门禁、Docker smoke、纯计算 P99 和唯一授权候选批次均已有验证证据。候选批次达到成功率与 Client 完整链 P95 目标，但它先于传输 one-shot 加固；最终源码未获授权再跑第二批。HTTP Handler 不设本地 admission gate、不因本机在途数主动返回 429；HTTP 端到端 P95、无界请求下的生产资源峰值和长时间稳定性压测仍未完成。

## [1.0.0] - Unreleased

### Added

- 新建独立纯 Go module，固定 Go `1.26.5`，生产 `go.mod` 无第三方依赖且不使用 CGo。
- 增加可并发复用的公共 `slider.Client`：`NewClient`、`Solve`、`Prime`、`PurgeArtifacts` 和幂等 `Close`。
- 增加完整纯 Go Solver：Device → InitCaptchaV3 → 双图下载 → Vision → Track/PE → Device Complete → 唯一 VerifyCaptchaV3。
- 增加稳定请求、结果和分类错误合同，包含完整阶段 `timingsMs`。
- 增加 JS/RPC 编码、RPC v1 签名、AES-CBC、DeviceToken、data codec、Verify 参数和自校验。
- 增加设备画像、浏览器头、指纹、Log1/2/3、首枚/刷新 token、getter/event 和单会话完成态。
- 增加纯 Go PE builder：轨迹字段、坐标、逻辑时钟、getter plan、交互事件及 Pack/Unpack 自检。
- 增加 Captcha Init/Verify RPC client；每个 client 最多尝试一次 Verify，网络结果未知也不重发。
- 增加 HTTP(S)、SOCKS4、SOCKS5、SOCKS5H 和按 proxy route 隔离的有界连接池。
- 增加有界设备预热池：完整 key 隔离、20 秒默认年龄、并行 Prime、Lease、冷建、过期和异步补货。
- 增加固定 HTTPS CDN 的双图并发内存下载、逐跳重定向白名单和响应大小限制。
- 增加纯 Go PNG 安全解码、缺口定位、困难 contour/chamfer fallback、置信度和几何换算。
- 增加 embedded 轨迹 fixture、目标距离缩放、抽稀和有界扰动。
- 增加失败/低置信 artifact 私有存储；完整 Solver 自动写入脱敏图片/指标。
- 增加 `cmd/server`：配置、预热、HTTP 服务、非回环警告、启动/每小时 artifact 清理及信号优雅关闭。
- 增加 HTTP Handler、OpenAPI 3.0.3 JSON、Trace ID、请求级 timeout 和脱敏错误映射。
- 增加 `Makefile` 的 fmt/vet/test/race/staticcheck/govulncheck/coverage/Linux 静态构建目标。
- 增加 Go CI `.github/workflows/ali-slider-go-ci.yml`，固定工具版本、覆盖率阈值和静态二进制断言。
- 增加 `docs/evidence/validation-2026-08-07.md`，固化命令、聚合数字、构建哈希与在线快照时序边界。
- 增加 `docs/evidence/validation-2026-08-08-no-local-admission.md`，固化删除本地准入闸门后的离线合同、质量门禁、源码选择集摘要与临时 Linux 二进制哈希。
- 增加 multi-stage `Dockerfile`：Go `1.26.5` 构建、scratch runtime、非 root UID/GID `65532`。
- 增加架构、API、配置、安全、测试、性能、迁移和排障文档。

### Changed

- 运行时从 Python/OpenCV/浏览器 worker 模型切换为单一纯 Go 进程；旧 Python 源码已从当前工作树删除，需要审计或回滚时从 Git 历史提交 `0509bfd` 恢复。
- 服务合同只开放 `POST /api/slider`、`GET /health` 和 `GET /openapi.json`；`GET /api/slider` 有意返回 404。
- 移除 HTTP 本机 admission gate 与主动 `429 Retry-After`；每个通过输入校验的 POST 请求都在 timeout context 内直接调用 Solver。
- 图片默认在内存处理；成功路径不落图，失败/低置信按 artifact 策略私有落盘。
- 默认监听收紧为 `127.0.0.1:8000`；应用内仍不提供鉴权。
- prefix 收紧为 1–32 个 ASCII 字母数字；proxy 必须使用支持的 scheme 且不含 path/query/fragment。
- 普通日志只记录事件、traceId、HTTP 状态和耗时，不记录 token、`CertifyId`、代理凭据或正文。
- 默认设备预热容量随 Client 连接/预热资源上限收敛；逐请求 proxy 因 route key 不同按设计冷建。
- artifact 默认保留 7 天；服务启动时清理一次，此后每小时清理。

### Fixed

- 修复 Device RPC 响应 schema：保留 `ResultObject` 原始 JSON，仅在 Log1 解码对象内的 `DeviceConfig`；Log2/Log3 成功响应允许非对象结果，避免真实设备链被误判为协议错误。
- 增加 Device schema 离线回归和显式授权的在线 Log1/2/3 探针；在线设备初始化通过，耗时约 `0.53s`。
- 禁用 Verify POST 的 `GetBody` 回卷入口，阻止 `net/http` 在 HTTP/2 GOAWAY/REFUSED_STREAM 等已发送 body 的失败后透明重放。
- 设备 Log1/Log2/Log3 同样使用不可回卷请求体，保证一次性会话动作不会被 Transport 在已发送后透明重放。
- 修复零值 `ClientOptions{}` 漏补默认设备预热容量；显式关闭预热仍通过 `DefaultClientOptions()` 后覆盖为零。

### Security

- 限制 HTTP 请求体为 64 KiB，并在进入 Client 前校验 JSON、类型、长度、prefix 和 proxy。
- HTTP Handler 不内置访问频率或并发准入保护；对外暴露必须由受控网络、反向代理或 API gateway 限制身份、频率、并发和总量。
- 每个 `CertifyId` 最多尝试一次 Verify；前置失败为零 Verify，网络未知也不重试。
- 代理 route 和每 host 连接数限制在 `1..32`；直连显式忽略环境 proxy。
- 设备预热池只复用完整 endpoint/prefix/region/route/profile/timing key 一致的会话。
- 资产固定 HTTPS CDN，重定向逐跳校验；单图默认最多 8 MiB。
- PNG 在完整解码前检查签名、IHDR、尺寸、像素数和 shadow alpha。
- artifact 目录/文件使用 `0700/0600`，随机名 `O_EXCL` 创建，清理不跟随 symlink。
- Solver、HTTP、日志和 artifact 测试覆盖 token、CertifyId、代理密码及原始 cause 脱敏。
- Docker runtime 为 scratch 且以非 root UID/GID `65532` 运行。

### Testing

- Go `1.26.5` 下全量 `go test`、`go test -race`、`go vet`、staticcheck 和 govulncheck 已验证通过；普通门禁和 Darwin race 都固定 `CGO_ENABLED=0`。
- Linux AMD64 `CGO_ENABLED=0` 静态、stripped `cmd/server` 构建已验证通过。
- Linux AMD64 Docker 镜像构建及非 root `/health` smoke 已验证通过。
- 全项目统一语句覆盖率记录为 `83.0%`，达到 `>=80%` 门槛；发布 commit 仍由 CI 重跑归档。
- Python 协议 oracle 与 fuzz seeds 对照通过；`internal/protocol` 覆盖率 `91.4%`。
- Python edge-decoy 正负视觉 fixture 对照通过；`internal/vision` 覆盖率 `91.9%`。
- PE builder 的 Python oracle 解包语义对照通过；`internal/pe` 覆盖率 `95.1%`。
- 完整 Solver Mock 链通过：成功路径一次 Verify、低置信零 Verify、Verify 网络错误一次尝试。
- 完整 Solver 的 32 路并发 Mock 正确性测试通过；该测试不作为吞吐、资源或尾延迟报告。
- 设备预热池通过有界容量、key 隔离、过期、取消、失败、补货及并发 Lease/Close 的 race 测试。
- HTTP 合同、64 个并发合法请求全部进入 Solver、OpenAPI 不含本地 429、启动配置、日志脱敏、artifact 清理和 Client Close 均有离线测试。
- 困难视觉 benchmark 均值为 `53.10ms/op`；独立纯计算 200 样本 nearest-rank `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`，满足 `P99<=100ms` 硬门槛。
- 唯一授权候选批次恰好执行 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试：严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。该批先于传输 one-shot 加固，最终源码受授权上限约束未再在线重跑。
- 候选批次直接调用 `Client.Solve`；Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，总批次约 `6.16s`；`>=190/200` 与 Client 完整链 `P95<=1000ms` 均通过。

### Known limitations

- 1 秒仅为 Client 完整求解链 P95 目标；本批 `P99=1018ms`、`max=1555ms`，没有 P99 或全部调用小于 1 秒的承诺。
- 候选批次未经过 HTTP Handler，HTTP 端到端真实 P95 尚未单独测量。
- 候选批次没有保留精确起止时间或阶段聚合，不能事后补造；后续发布批次必须在发送首个挑战前启用完整审计采集。
- 已有离线 Mock 的短时 maxRSS 记录，但生产形态 32 路 RSS、GC、goroutine 峰值和长时间稳定性压测尚未执行。
- 应用内没有鉴权；对外暴露必须由受控网络、反向代理或 API gateway 提供访问控制。
- 应用内没有 admission gate；请求激增可导致连接等待、超时、GC 压力、内存耗尽或进程退出，不承诺无界并发下的延迟与可用性。
- 逐请求 proxy 允许调用方指定网络出口，外部部署必须限制调用方或实施 allowlist。
- Python 业务提交/重放、交互式 CLI、桌面 UI、PyInstaller 和 Swagger UI 不属于 1.0.0 范围。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `go.mod:3`；`Dockerfile:3`；`.github/workflows/ali-slider-go-ci.yml:41` | module、容器和 CI 统一固定 Go `1.26.5`。 | source → pinned toolchain → candidate artifacts |
| `pkg/slider/client.go:82`、`:160`、`:188`、`:203`、`:213` | 对外 Client 和完整生命周期已经实现。 | library caller → Solve/Prime/Purge → Close |
| `internal/challenge/solver.go:107`、`:149`、`:289` | 具体 Solver 已串接完整单轮链路。 | request → Device/Init/Vision/PE → one Verify |
| `internal/challenge/solver_test.go:227`、`:268`、`:286` | 成功、前置失败和网络未知的 Verify 次数已有离线计数证据。 | Mock transport → state gates → counted attempt |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](docs/evidence/validation-2026-08-07.md) | 32 路 Mock 正确性通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛。 | concurrent mock / explicit local sampler → verified compute gate |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165`；`internal/device/online_session_test.go:17` | Device schema 修复只在 Log1 解析对象，Log2/3 接受非对象成功结果；在线 Log1/2/3 约 `0.53s` 通过。 | raw response → action-specific decode → offline regression → authorized probe |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](docs/evidence/validation-2026-08-07.md) | 授权候选 200/32/应用层零重试批次成功 `196/200`，Client 完整链墙钟 P95 `984ms`，两项冻结门槛通过；该 harness 不经过 HTTP Handler，最终 one-shot 加固后未再在线重跑。 | authorized jobs → one Solve each → sanitized aggregate → bounded acceptance |
| `internal/challenge/device_pool.go:22`、`:134`、`:190` | 设备预热池已实现有界 Prime/Lease、key 隔离和冷建回退。 | startup Prime → request Lease → release/refill |
| `cmd/server/main.go:36`、`:49`、`:55`、`:60`、`:93`、`:111`、`:147` | 可执行服务、启动/定时清理和优雅关闭已经接线。 | config → Client → HTTP → cleanup |
| `Makefile:40`；`.github/workflows/ali-slider-go-ci.yml:97` | Linux AMD64 静态二进制已有本地和 CI 构建路径。 | commit → verified server binary |
| `internal/vision/solver_test.go:260`；`internal/challenge/performance_test.go:65` | `53.10ms/op` 是视觉均值；完整纯计算链 P99 由独立逐次采样得到 `57.05075ms`。 | benchmark mean / sorted samples → separate performance claims |
