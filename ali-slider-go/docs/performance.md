# 性能测试与容量口径

> **验收结论**：Mac ARM64 纯计算 200 样本 nearest-rank `P99=57.05075ms`，满足 `<=100ms`。2026-08-07 唯一授权候选批次恰好 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`；Client 完整求解链墙钟 `P95=984ms`，满足 `<=1000ms`。本批 `P99=1018ms`、`max=1555ms`，因此 1 秒只可描述 P95，不能描述 P99 或全部调用；该 harness 不经过 HTTP Handler，不是 HTTP 端到端基准。该批先于最终传输 one-shot 加固，最终源码未获授权在线重跑。当前 HTTP Handler 不设本地 admission 或在途硬上限；64 并发直通仅验证合同，生产形态下无界增长的 goroutine、内存、超时堆积和长时间资源稳定性尚未压测。

## 性能目标

| 维度 | 冻结口径 | 性质 | 当前状态 |
|---|---:|---|---|
| 纯 Go 本地计算 P99 | `<=100 ms` | 硬门槛 | `57.05075ms`，通过 |
| 热态直连 Client 完整求解链 P95 | `<=1000 ms` | 优化目标 | `984ms`，通过 |
| HTTP 同时在途挑战 | 无本地硬上限 | 服务合同 | 64 个并发合法请求全部进入 Solver；不是生产容量结论 |
| Client 每 route/host 连接与预热预算 | `1..32` | 兼容配置边界 | `MaxConcurrency` 只约束出站 Transport 与预热资源，不是 HTTP admission |
| 授权在线成功率 | `>=190/200` | 正确性门槛 | `196/200`（`98%`），通过 |

`P95 <=1000 ms` 不表示每个请求都必须低于 1 秒。设备 RPC、Captcha Init/Verify、TLS、CDN、DNS 和代理均可能产生不可控尾延迟；报告必须同时给出分位数、失败分类和网络环境。

## 当前实现中的计时

`challenge.Solver` 返回固定 `timingsMs` key：

| Key | 边界 | 主要内容 |
|---|---|---|
| `setup` | 请求校验后到客户端准备完成 | 画像选择、route/transport、设备与 Captcha client 构造 |
| `deviceSession` | Lease/Open 开始到首枚 token 可用 | 预热命中，或冷建 Log1/2/3 |
| `init` | Captcha Init 请求往返 | InitCaptchaV3 |
| `downloadAssets` | 两图下载整体墙钟 | 背景图和 shadow 并行下载 |
| `downloadBackground` | 单图下载 | 背景图 |
| `downloadShadow` | 单图下载 | shadow |
| `vision` | 两张 PNG 到识别结果 | PNG 解码、缺口定位、置信度和几何 |
| `buildVerifyData` | 轨迹到 PE data | 轨迹缩放、逻辑时钟、PE、Pack/Unpack 自检 |
| `completeDevice` | 设备完成态 | getter/event、Verify token 和最终 Log2 |
| `verify` | Captcha Verify 请求往返 | 唯一一次 VerifyCaptchaV3 |
| `clientCleanup` | release 开始到返回 | 关闭已消费会话并触发有界补货 |
| `total` | `challenge.Solver.Solve` 墙钟 | 上述完整 Solver 生命周期 |

双图并行使 `downloadAssets` 不等于两个单图耗时之和。毫秒取整也会使阶段相加与 total 有小幅差异。

`Result.ElapsedMS` 来自 Solver 的 `total`，不包含客户端到 HTTP 服务的建连、请求解析和响应传输。完整 HTTP P95 应以负载发生器的端到端墙钟为主，以应用日志和 `timingsMs` 做归因，不能只排序 `elapsedMs`。

## 指标边界

| 指标 | 起点与终点 | 包含 | 不包含 |
|---|---|---|---|
| 视觉算法 | PNG bytes 进入 `vision.Solve` 到返回 | 解码、图像操作、fallback、坐标 | 轨迹、PE、网络 |
| 纯计算链 | 已取得两张 PNG 到 PE/Data 构建与自检完成 | vision、track、PE data、协议编码/加密/自检 | Device Complete、Captcha Verify、其他网络与图片下载 |
| Solver total | `challenge.Solver.Solve` 进入到清理完成 | 本地计算与全部上游等待 | HTTP parse/write 和客户端网络 |
| 完整请求 | 负载发生器发出 HTTP 到读完 response | 服务 HTTP、Solver、上游等待 | 负载发生器准备数据的时间 |
| 吞吐 | 窗口内完成响应数 / 墙钟秒数 | 成功、业务失败、技术失败分别计数 | 不能只统计成功响应 |
| 在途并发 | 合法请求进入 `Solve` 到 Solver 返回 | Solver 运行及上游等待 | 跨源边界、JSON/字段校验在 Solver 前拒绝的请求；外层代理在到达本进程前拒绝的请求 |

`internal/challenge/performance_test.go` 提供显式启用的 200 样本逐次纯计算 P99 门禁；普通 CI 跳过时钟硬断言。最终源码快照的 Mac ARM64 结果为 `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`，达到硬门槛。它只使用固定困难 fixture，不包含任何网络阶段；扩展数据集和多轮重复仍可用于稳定性画像，但不能抹掉或夸大本次结论。

## 统计定义

### 平均值

平均值为 `sum(duration) / N`。Go benchmark 的 `ns/op` 是多次迭代的平均耗时，适合比较优化趋势，但不能证明尾延迟门槛。

### P95 与 P99

项目使用 nearest-rank：

```text
P(q) 的一基序号 = ceil(q × N)
P(q) 的零基下标 = ceil(q × N) - 1
```

当 `N=200` 时：

- P95 是升序第 190 个样本。
- P99 是升序第 198 个样本。

本项目的纯计算门禁和唯一授权候选批次都使用 200 个逐次样本及 nearest-rank 口径。扩大到至少 10,000 个样本并重复多轮可作为性能稳定性画像，但不是把当前已通过结果改写成“未测”的理由；任何报告都禁止从平均值推算 P99。

## 正式基准环境

正式 Mac 基线：

```text
Go:       go1.26.5 darwin/arm64
CPU:      Apple M3 Max
逻辑 CPU: 16
内存:     51,539,607,552 bytes（48 GiB）
```

module、CI 和 Docker 构建均固定 Go `1.26.5`。Linux AMD64 已验证为静态构建目标，但不能把其功能构建结果与 Mac 性能基线混为同一结论。

每份性能报告还必须记录 commit、工作树状态、Go toolchain、OS/arch、`GOMAXPROCS`、电源模式、温度/降频、后台负载、直连/代理、预热容量及 benchmark 参数。

## 已记录视觉基准

```bash
go test ./internal/vision \
  -run '^$' \
  -bench '^BenchmarkSolve$' \
  -benchmem \
  -benchtime=10x \
  -count=1
```

困难合成视觉 benchmark 的记录均值为 `53.10ms/op`。该均值本身不能证明尾延迟；项目另以完整纯计算链逐次采样得到 `P99=57.05075ms`。两项结果的统计量和测量范围不同，报告中不得混写。

并行趋势：

```bash
GOMAXPROCS=16 go test ./internal/vision \
  -run '^$' \
  -bench '^BenchmarkSolveParallel$' \
  -benchmem \
  -benchtime=10s \
  -count=5
```

`BenchmarkSolveParallel` 只反映视觉函数的并行吞吐趋势，不包含设备会话、HTTP、上游网络、成功率或 32 个完整挑战的内存峰值。

## 设备预热的性能含义

兼容字段 `MaxConcurrency` 控制 Client 每个 route/host 的出站连接与预热资源预算，保持 `1..32` 校验；默认预热容量随该资源预算收敛，默认会话最大空闲年龄为 20 秒。它不限制 HTTP 在途请求。启动器先完成 `net.Listen` 绑定，再在开始 `Serve` 前调用 `Client.Prime`，把设备 Log1/2/3 尽量移出请求关键路径，同时避免端口占用时先访问上游。

必须同时报告两种模式：

- 冷建：`--device-prewarm=0`，`deviceSession` 包含完整设备初始化。
- 热态：预热完成、key 匹配且会话未过期，`deviceSession` 主要为 Lease。

预热池只在 endpoint、prefix、region、route、profile ID、timeout 和设备时序范围完全一致时复用。逐请求代理与默认直连池 key 不同，会走冷建；这属于安全隔离，不应通过跨 route 复用来“优化”。消费后会话立即关闭，再异步有界补货；`ready + pending + leased` 不超过容量。

预热失败会记录 warning，成功库存仍保留，后续请求可冷建兜底。不得把 Prime 失败隐藏为性能成功。

## 纯计算 P99 方法

仓库内的开发期冒烟入口为：

```bash
ALI_SLIDER_PERF=1 GOMAXPROCS=16 \
  go test -count=1 -v ./internal/challenge \
  -run '^TestPureComputeP99$'
```

该测试仅在声明的 `darwin/arm64` 基线执行，顺序采集 200 次 `Vision → Track → PE/Data`，按 nearest-rank 输出 P50/P95/P99/max，并断言 P99 不超过 100ms。最终验证结果为 `P99=57.05075ms`，判定通过；其他平台看到 `SKIP` 仍不能记作该平台通过。

扩展稳定性画像可按以下流程：

1. 固定 commit、Go `1.26.5`、机器、电源模式和 `GOMAXPROCS=16`。
2. 在计时外读取 fixture、完成只读初始化并充分预热 Go runtime。
3. 逐次记录 `vision`、`buildVerifyData` 和完整纯计算链墙钟，不只保存聚合平均值。
4. 数据集同时包含普通图、困难 fallback、无缺口负例和授权脱敏失败样本；不得只挑最快样本。
5. 分别报告含 PNG 解码与不含解码结果；硬门槛以真实请求所需的含解码链为准。
6. 排序后计算 P50/P95/P99/max，并记录 B/op、allocs/op、RSS 和 GC。
7. 至少重复 5 轮；任一正式轮次 P99 超过 100 ms，则硬门槛失败。
8. race detector 单独用于正确性，不把插桩后的耗时作为性能数据。

当前 200 样本结果用于冻结的 Mac ARM64 纯计算硬门槛；扩展数据集和重复轮次用于评估跨样本、跨温度及跨机器稳定性。

## Mock Solver/HTTP 分层容量方法

完整 Solver 和服务入口已存在，可直接建立受控 Mock 负载：

- 并发至少按 `1/4/8/16/32/64` 逐级测试，固定每档请求数和 Mock 阶段延迟。
- 同时测冷建与热态；记录预热命中、冷建、过期和补货失败。
- 64 并发直通回归必须证明每个合法 HTTP 请求都已进入 Solver；释放 Mock 后全部返回 200，且没有 `Retry-After`。
- OpenAPI 的 Solve response 只声明 200/400/403/500，其中 403 仅是明确浏览器跨源拒绝；不得包含 429、`Retry-After` 或旧本地过载字段。
- 请求完成、取消、panic 或错误后都必须清理各自上下文与设备会话。
- 记录完成吞吐、P50/P95/P99/max、错误分类、goroutine、RSS 和 GC；外层代理 429 与第三方上游 429 分开归因。
- 在 `-race` 下复跑状态正确性，但性能数字来自无 race 插桩版本。

离线 `TestSolverOffline32ConcurrentSuccess` 证明状态隔离；唯一授权候选 Client harness 又以 32 并发完成 200 轮并形成短批次延迟报告。热构建缓存下，32 并发 Mock 五次包测试用时 `0.166–0.193s`，命令 wall `0.43–0.46s`，maxRSS `155.2–177.6MiB`；最终源码的 `200×32=6,400` 次完整离线 Solve 以 `32.27s`/maxRSS `242.2MiB` 通过。这是短时、含 Go 驱动进程的正向筛查，不是生产内存预算或绝对泄漏证明；生产形态的 GC、goroutine 峰值、持续吞吐和长时间稳定性仍待测。

取消本地 HTTP admission 后，Handler goroutine 与已进入的 Solve 数可随来流增长；Transport 连接数和预热库存有界，并不能把 HTTP 在途数变成有界。当前尚未验证生产流量下请求堆积、上游连接等待、超时风暴、RSS/GC 峰值或进程耗尽边界，因此 64 并发回归不得写成“服务支持无界并发”或生产容量结论。需要保护生产实例时，应在外层反向代理、负载均衡或 API 网关配置容量与速率策略，并把其 429 单独统计。

## 2026-08-07 授权在线历史批次

- 当次授权上限为 200 个新挑战，每轮新 `CertifyId`，每个 job 一次 Solve、应用层零重试；最终源码还在传输层禁止一次性 POST 回卷。
- 当次 harness 并发参数为 32；热 Client、直连。该数字是历史测试参数，不是当前 HTTP 服务上限；代理结果必须单独分组。
- 严格成功定义：`T001`、`VerifyResult=true`、token 非空。
- 同一批数据计算 `>=190/200` 成功门槛和 Client 完整求解链 P95 `<=1000 ms` 目标；HTTP 端到端需另用负载发生器测量。
- 外层代理 429、第三方上游 429、超时、低置信、其他上游 HTTP、业务拒绝和未知 Verify 结果分别计数；当前 Handler 不主动生成本地 admission 429。
- 不要求 10 分钟持续压测；不得扩大授权流量来“刷”成功率。

唯一授权候选批次已恰好执行一次，未通过额外挑战替换失败样本：

| 分类或延迟 | 结果 |
|---|---:|
| attempts / concurrency / retry | `200 / 32 / 0` |
| strict success | `196`（`98%`） |
| business failure | `3` |
| VisionError / network error | `1 / 0` |
| wall P50 / P95 / P99 / max | `816 / 984 / 1018 / 1555 ms` |
| success wall P95 | `989ms` |
| batch wall | 约 `6.16s` |

成功率 `196/200 >=190/200`，Client 完整求解链墙钟 `P95=984ms <=1000ms`，两项冻结目标通过。`P99=1018ms` 明确高于 1 秒；项目没有 P99 小于 1 秒的目标或结论。本批 harness 不经过 HTTP Handler。

## 优化优先级

1. 保持 oracle、负例和唯一 Verify，不接受通过跳过校验、放宽阈值或重试换速度。
2. 先用 `timingsMs`、CPU profile 和 allocation profile 定位热点。
3. 优先减少热路径分配、重复颜色转换和临时切片；缓冲复用必须按请求隔离并通过 race。
4. 保留双图并发、route 隔离和连接池；禁止跨代理共享连接或跨请求共享 token/session。
5. 在授权环境启用预热；单独记录启动成本、命中率、过期和冷建回退。
6. 外部阶段慢时定位 DNS、TLS、代理、CDN 或上游，不得靠放大 timeout 伪装性能优化。

## 结果模板

| 字段 | 要求 |
|---|---|
| 版本 | commit、dirty 状态、Go `1.26.5` |
| 环境 | OS、arch、CPU、内存、`GOMAXPROCS`、电源模式 |
| 数据集 | fixture/授权样本数量及普通、困难、负例分布 |
| 模式 | 冷建/热态、直连/代理、并发度 |
| 样本 | 预热数、实测数、重复轮次 |
| 延迟 | mean、P50、P95、P99、max，单位 ms |
| 资源 | B/op、allocs/op、RSS、GC、goroutine 峰值 |
| 正确性 | oracle 通过数或在线严格成功数；失败分类 |
| 结论 | 逐项写明通过、失败或未测 |

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/challenge/solver.go:107`、`:149`、`:431` | 完整 Solver 已实现，并固定 12 个阶段耗时 key。 | Solve → stage timings → Result |
| `internal/challenge/solver_test.go` · `TestSolverOfflineCompleteSuccess` | 完整离线链成功路径和阶段输出已有 Mock 证据。 | device/init/assets/vision/PE/verify → outcome |
| `internal/challenge/device_pool.go:15`、`:22`、`:190` | 预热池有 20s 默认年龄、完整 key 隔离和有界 Lease/补货。 | Prime → Lease or cold open → release/refill |
| `internal/server/server.go` · `handleSolve`；`internal/server/server_test.go` · `TestConcurrentRequestsAlwaysEnterSolver` | Handler 无本地 admission；64 个并发合法请求全部进入 Solver。 | valid request → timeout context → Solve |
| `internal/server/openapi.go` · `openAPIDocument`；`internal/server/server_test.go` · `TestOnlyFrozenRoutesAreExposed` | OpenAPI 只声明 200/400/403/500，并回归禁止 429 与旧本地过载字段。 | API document → response contract → no local 429 |
| `pkg/slider/client.go:183` | 公共 Result 的 elapsedMs 来自 Solver total，而非客户端完整 HTTP 墙钟。 | Solver timings → library result |
| `internal/vision/solver_test.go:260`、`:271` | 视觉 benchmark 只提供均值/并行趋势；项目 P99 结论来自独立逐次纯计算门禁。 | repeated vision Solve → mean throughput only |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 32 路 Mock 正确性通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛；6,400 次离线 Solve 压力通过。 | concurrent mock / explicit local sampler / repeated stress → verified compute and capacity gates |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 200/32/应用层零重试候选批次成功率 `98%`、Client 完整求解链 P95 `984ms`，两项门槛通过；不经过 HTTP Handler，最终 one-shot 加固后未再在线重跑。 | authorized jobs → one Solve each → aggregate percentiles → bounded acceptance |
| `go.mod:3`；`Dockerfile:3` | 正式构建工具链固定 Go `1.26.5`。 | source → pinned build → comparable baseline |
