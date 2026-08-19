# 性能测试与容量口径

> **当前结论**：生产运行时使用同进程V8 `149.4.0`采集动态Device，并对每个精确PE `StaticPath`做V8 oracle与纯Go完整差分。当前已移除Device预热/复用：每个合法Solve立即冷建独立Device/V8并在结束时关闭；公开SDK/PE源码和结构画像仍跨轮缓存；`OpenDevice → Close` 没有本地并发槽。2026-08-09记录的 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`及reserve/prewarm A/B均属于历史预热实验；当时5个对齐存活VM出现过字段合同异常，取消本地保护后该现象属于已知风险，不能当作当前逐请求冷建路径的正确性或性能结论。当前HTTP端到端、资源峰值和长稳仍无结论。

## 性能目标

| 维度 | 冻结口径 | 性质 | 当前状态 |
|---|---:|---|---|
| 已自校验 PE 的 `buildVerifyData` | 尽可能 `<=100 ms` | 本地计算目标 | 历史真实50次mean `1–2ms`；强制V8约 `530ms`，当前路径待复测 |
| 直连 Client 完整求解链 | mean 约 `1000 ms` | 本次优化目标 | 当前逐请求冷建路径未测；历史预热候选50次mean `2562ms`，未通过 |
| HTTP 同时在途挑战 | 无本地硬上限 | 服务合同 | 64 个并发合法请求全部进入 Solver；不是生产容量结论 |
| Client每route/host连接与PE预算 | `1..32` | 兼容配置边界 | `MaxConcurrency`只约束Transport/PE；不是Device或HTTP admission |
| 每轮独立Device VM的live数量 | 无本地上限 | 当前实现选择 | 来流直接放大V8、CPU、内存、线程和上游压力；历史5个对齐存活会话出现过字段合同异常，当前尚未在线重验 |
| 当前路径真实成功率 | 不低于基线 `46/50` | 正确性门槛 | 逐请求冷建实现尚未在线重测；历史预热候选为 `47/50` |

`P95 <=1000 ms` 不表示每个请求都必须低于 1 秒。设备 RPC、Captcha Init/Verify、TLS、CDN、DNS 和代理均可能产生不可控尾延迟；报告必须同时给出分位数、失败分类和网络环境。

## 当前实现中的计时

`challenge.Solver` 返回固定 `timingsMs` key：

| Key | 边界 | 主要内容 |
|---|---|---|
| `setup` | 请求校验后到客户端准备完成 | 画像选择、route/transport、设备与 Captcha client 构造 |
| `deviceSession` | Open开始到首枚token可用 | 本轮独立V8 Device Isolate冷建和Log1/2/3；不含本地live槽等待 |
| `init` | Captcha Init 请求往返 | InitCaptchaV3 |
| `resolvePEKey` | Init 后到当前 PE 结构画像可用 | 缓存命中，或公开 SDK/PE 下载、V8 采样与纯 Go 完整差分；SDK 软 TTL 5 分钟，PE 硬 TTL 30 分钟 |
| `downloadAssets` | 两图下载整体墙钟 | 背景图和 shadow 并行下载 |
| `downloadBackground` | 单图下载 | 背景图 |
| `downloadShadow` | 单图下载 | shadow |
| `vision` | 两张 PNG 到识别结果 | PNG 解码、缺口定位、置信度和几何 |
| `buildVerifyData` | 轨迹到 PE data | 已验证分片使用动态 profile + 纯 Go Builder；其余使用禁网 V8 fallback；两者都做 Pack/Unpack、getter、事件和时钟合同自检 |
| `completeDevice` | 设备完成态 | 同一 FeiLin VM 事件回放/getter、Verify token 和最终 Log2 |
| `verify` | Captcha Verify 请求往返 | 唯一一次 VerifyCaptchaV3 |
| `clientCleanup` | release 开始到返回 | 关闭本轮Device/V8；不补货、不回池 |
| `total` | `challenge.Solver.Solve` 墙钟 | 上述完整 Solver 生命周期 |

双图并行使 `downloadAssets` 不等于两个单图耗时之和。毫秒取整也会使阶段相加与 total 有小幅差异。

`Result.ElapsedMS` 来自 Solver 的 `total`，不包含客户端到 HTTP 服务的建连、请求解析和响应传输。完整 HTTP P95 应以负载发生器的端到端墙钟为主，以应用日志和 `timingsMs` 做归因，不能只排序 `elapsedMs`。正式基准优先使用 POST JSON；deprecated GET query 只是协议兼容性输入，不是另一条更快的求解链。

## 指标边界

| 指标 | 起点与终点 | 包含 | 不包含 |
|---|---|---|---|
| 视觉算法 | PNG bytes 进入 `vision.Solve` 到返回 | 解码、图像操作、fallback、坐标 | 轨迹、PE、网络 |
| 生产本地构建链 | 已取得两张 PNG 到 PE/Data 自检完成 | vision、track、已验证纯 Go Builder 或 V8 fallback、协议解包/合同自检 | 首次分片画像若已在前置 `resolvePEKey` 计时；Device Complete、Captcha Verify、其他网络与图片下载 |
| Solver total | `challenge.Solver.Solve` 进入到清理完成 | 本地计算与全部上游等待 | HTTP parse/write 和客户端网络 |
| 完整请求 | 负载发生器发出 HTTP 到读完 response | 服务 HTTP、Solver、上游等待 | 负载发生器准备数据的时间 |
| 吞吐 | 窗口内完成响应数 / 墙钟秒数 | 成功、业务失败、技术失败分别计数 | 不能只统计成功响应 |
| 在途并发 | 合法请求进入 `Solve` 到 Solver 返回 | Solver 运行及上游等待 | 跨源边界、JSON/query/字段校验在 Solver 前拒绝的请求；外层代理在到达本进程前拒绝的请求 |

`internal/challenge/performance_test.go` 仍提供显式启用的 200 样本纯 Go oracle/视觉回归。历史 Mac ARM64 结果为 `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`。它可用于监测视觉/纯 Go Builder 回归，但不包含新分片首次 V8 自校验、Device 和任何网络，因此不能代替完整 Solve 验收。

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

历史纯计算门禁和授权候选批次都使用 200 个逐次样本及 nearest-rank 口径。架构改变后必须对当前 V8 production path 重新采样；任何报告都禁止从旧实现、单个 smoke 或平均值推算当前 P95/P99。

## 正式基准环境

正式 Mac 基线：

```text
Go:       go1.26.5 darwin/arm64
CPU:      Apple M3 Max
逻辑 CPU: 16
内存:     51,539,607,552 bytes（48 GiB）
```

module、CI 和 Docker 构建均固定 Go `1.26.6`，wrapper 构建固定 Rust `1.88.0`/V8 `149.4.0`。Linux AMD64/ARM64 只验证了功能构建/native ABI，不能与 Mac 性能基线混为同一结论。

每份性能报告还必须记录 commit、工作树状态、Go toolchain、OS/arch、`GOMAXPROCS`、电源模式、温度/降频、后台负载、直连/代理、`MaxConcurrency`、同时存活的Device会话峰值及 benchmark 参数。

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

## 当前逐请求冷建与历史预热实验

兼容字段 `MaxConcurrency` 只控制Client每个route/host的出站连接与PE资源预算，保持 `1..32`校验；它不限制Device会话或HTTP请求进入。每个合法Solve立即从 `OpenDevice` 冷建整轮会话，并在 `Close` 后销毁。启动器不调用 `Client.Prime`，`Prime`只保留为不发外部请求的源码兼容空操作。旧 `--device-prewarm` 与 `--device-reserve` 参数只接受 `0`，非0配置明确失败。

当前每轮报告都应视为冷建：`deviceSession` 包含完整Device Log1/2/3，不含本地live额度等待；`clientCleanup`包含本轮Device/V8关闭。没有ready库存、热态命中、idle过期、recycle、后台补货或live并发槽。进程级KeyResolver仍复用公开SDK、精确PE源码和已验证结构画像，因此Device逐轮冷建不等于所有公开脚本与PE画像也逐轮下载/采样。

下方2026-08-09表格中的 `reserve=4`、`reserve=1`、`prewarm16` 和“最终4-live满池等待”等数据属于历史预热池实验。它们说明5个同时存活Device VM不安全、reserve没有改善当时总耗时，但不代表这些配置仍可启用，也不证明当前逐请求冷建路径的延迟。

## 历史纯 Go oracle P99 方法

仓库内的开发期冒烟入口为：

```bash
ALI_SLIDER_PERF=1 GOMAXPROCS=16 \
  go test -count=1 -v ./internal/challenge \
  -run '^TestPureComputeP99$'
```

该测试仅在声明的 `darwin/arm64` 基线执行，顺序采集 200 次 `Vision → Track → fake/纯 Go PE/Data`，按 nearest-rank 输出 P50/P95/P99/max。历史结果 `P99=57.05075ms`；它不包含当前 V8 PE Isolate，其他平台 `SKIP` 或该历史 PASS 都不能记作当前生产路径通过。

扩展稳定性画像可按以下流程：

1. 固定 commit、Go `1.26.6`、机器、电源模式和 `GOMAXPROCS=16`。
2. 在计时外读取 fixture、完成只读初始化并充分预热 Go runtime。
3. 逐次记录 `vision`、`buildVerifyData` 和完整纯计算链墙钟，不只保存聚合平均值。
4. 数据集同时包含普通图、困难 fallback、无缺口负例和授权脱敏失败样本；不得只挑最快样本。
5. 分别报告含 PNG 解码与不含解码结果；硬门槛以真实请求所需的含解码链为准。
6. 排序后计算 P50/P95/P99/max，并记录 B/op、allocs/op、RSS 和 GC。
7. 至少重复 5 轮；任一正式轮次 P99 超过 100 ms，则硬门槛失败。
8. race detector 单独用于正确性，不把插桩后的耗时作为性能数据。

当前架构的正式本地门禁还应分别覆盖：已验证纯 Go 快路、首次 V8 画像和不兼容分片 V8 fallback。在此之前，旧 200 样本只用于纯 Go/视觉回归趋势。

## Mock Solver/HTTP 分层容量方法

完整 Solver 和服务入口已存在，可直接建立受控 Mock 负载：

- 并发至少按 `1/4/8/16/32/64` 逐级测试，固定每档请求数和 Mock 阶段延迟。
- 记录每轮Device冷建、Close、同host连接等待和公开SDK/PE缓存命中；不存在Device热态命中、过期或补货。
- 64 并发直通回归使用当前 POST JSON 入口，必须证明每个合法 HTTP 请求都已进入 Solver；释放 Mock 后全部返回 200，且没有 `Retry-After`。
- deprecated GET 用单独功能回归锁定 64 KiB query、别名/重复/空值、GET/POST 分源和跨源拒绝；不把 GET 预取、爬虫、uptime 监控或浏览器自动化当作负载发生器，因为每次 GET 都有真实求解副作用。
- OpenAPI 的 Solve POST 与 deprecated GET response 都只声明 200/400/403/500，其中 403 仅是明确浏览器跨源拒绝；不得包含 429、`Retry-After` 或旧本地过载字段。
- 请求完成、取消、panic 或错误后都必须清理各自上下文与设备会话。
- 记录完成吞吐、P50/P95/P99/max、错误分类、goroutine、RSS 和 GC；外层代理 429 与第三方上游 429 分开归因。
- 在 `-race` 下复跑状态正确性，但性能数字来自无 race 插桩版本。

离线 `TestSolverOffline32ConcurrentSuccess` 证明状态隔离；历史 Client harness 以 32 并发完成 200 轮。热构建缓存下的 Mock 与 `200×32=6,400` 次离线 Solve 都使用 fake PE runtime，不启动 production V8 Isolate；这些数字只证明 Go 编排和状态隔离，不是当前 V8 的吞吐、RSS 或泄漏证明。生产形态的 V8/Go RSS、heap、GC、goroutine/原生线程峰值、持续吞吐和长时间稳定性仍待测。

取消本地 HTTP 和 Device live admission 后，Handler goroutine、已进入的 Solve 数和同时存活的Device/V8会话都可随来流增长；只有Transport连接数和PE预算仍由 `MaxConcurrency` 约束。当前尚未验证生产流量下无界冷建、上游连接等待、超时风暴、RSS/GC峰值或进程耗尽边界，因此64并发回归不得写成“服务支持无界并发”或生产容量结论。需要保护生产实例时，应在外层反向代理、负载均衡或API网关配置容量与速率策略，并把其429单独统计。

## 2026-08-09 历史预热池路径真实验收

口径为 Linux AMD64 最终 V8 wrapper、同一 `slider.Client`、直连、并发 10、每个 job 一次 Solve、应用层零重试，每个新 `CertifyId` 最多一次 Verify。严格成功要求 `T001`、`VerifyResult=true` 且 token 非空。数字为 Client Solve 墙钟，不经 HTTP Handler。

| 版本/模式 | 成功 | mean | P50 | P95 | max | 主要观察 |
|---|---:|---:|---:|---:|---:|---|
| 优化前基线 | `46/50` | 未记录 | `4722ms` | `7136ms` | `8578ms` | Device/PE 外层运行时每轮重建 |
| Device recycle + PE runtime 池 | `47/50` | `3040ms` | `3070ms` | `4663ms` | `5397ms` | `resolvePEKey` mean `1054ms`，V8 Build `608ms` |
| 精确分片自校验纯 Go 快路 | `49/50` | `2007ms` | `1771ms` | `3217ms` | `3258ms` | Build mean `2ms`，瓶颈转为冷分片画像 |
| 默认 `reserve=0` + 脱敏分片计数 | `48/50` | `1890ms` | `1909ms` | `2996ms` | `3154ms` | 32 个精确分片，32/32 兼容；后 25 mean `1376ms` |
| `reserve=1` A/B | `47/50` | `2112ms` | `2127ms` | `3135ms` | — | 总耗时反而上升 |
| `reserve=4` A/B | `47/50` | `2305ms` | — | `4027ms` | — | Device 等待下降，PE/Complete 争用上升 |
| prewarm16错误实验 | `33/50` | `2047ms` | `2167ms` | `3100ms` | `3255ms` | 16个预热会话全部在Complete报协议错，不可用 |
| 4-live冷溢出候选 | `48/50` | `2657ms` | `2760ms` | `4356ms` | `4711ms` | F015=2，分类错误0 |
| 最终4-live满池等待 | `47/50` | `2562ms` | `2157ms` | `4910ms` | `5859ms` | F015=3，分类错误0；后25 mean2232ms |

该历史安全候选将P50从 `4722ms`降到 `2157ms`，约下降54%；成功率从92%到94%。但mean `2562ms`明确未达到约1秒。最终阶段mean中 `buildVerifyData=1ms`、`resolvePEKey=204ms`，当时主瓶颈是直连预热池4槽边界下的 `deviceSession=1644ms`。当前生产实现已移除该预热池和live并发保护，这些数字不再作为当前路径验收；历史5+ live Device会话出现过字段合同异常，现作为已知风险保留，而不是当前限制。

长时运行的缓存现采用两层时限：每 5 分钟重下 SDK 并比较字节；SDK 未变时延长已验证分片的软 TTL；任一分片超过 30 分钟仍强制重下 PE 和完整 V8 差分。SDK 字节变化则立即清空所有画像。这能避免稳定服务每 5 分钟重复采样 32 个分片，但不会改善全新进程的首批冷启动。

完整 Evidence → Finding → Path、时间线和验收边界见 [2026-08-09 性能证据](./evidence/validation-2026-08-09-performance.md)。

## 2026-08-11 TRACELESS / SLIDING 小样本诊断

阶段计时证明，自动读取 `CaptchaType` 不是 4–5 秒的瓶颈：测试冷启动和外部 Init/资源/Verify 是公共成本，SLIDING 旧实现还把 86 点、2233ms 的逻辑轨迹按真实墙钟逐点休眠。最终实现不再做轨迹 timer/macrotask 等待，而是同步推进验证码上下文的 `Date.now()`、`performance.now()` 和事件 `timeStamp`；1800ms 逻辑轨迹离线约 `2.6–3.0ms`。TRACELESS 同时关闭官方 success 延迟。

无 Node 的 Linux ARM64 容器加载生产 V8 `149.4.0` wrapper 后，各执行一个真实挑战：SLIDING `success=1`、类型阶段 `1390ms`、完整 Solve `2750ms`；TRACELESS `success=1`、类型阶段 `1168ms`、完整 Solve `2488ms`。这是两个单样本，不是成功率或 P95/P99；同 engine 动态资源缓存命中收益仍未单独测量。完整阶段、缓存边界、失败实验与命令见 [2026-08-11 运行时性能优化报告](./2026-08-11_js-web-captcha-runtime-performance-report.md)。

## 2026-08-08 历史功能 smoke

当时的动态修复快照只执行 1 个新挑战、并发 1、一次 Solve、零重试：严格成功 `1/1`，墙钟 `1579ms`。该数字属于本次内嵌 V8 迁移之前的运行时，只作历史对照，不是当前均值、P50、P95 或 P99。

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

当时的成功率 `196/200 >=190/200`，Client 完整求解链墙钟 `P95=984ms <=1000ms`，两项冻结目标通过。`P99=1018ms` 明确高于 1 秒；本批 harness 不经过 HTTP Handler，并使用后来证明缺失当前动态设备/PE 边界的实现，所以这些分位数只能作为历史对照。

## 优化优先级

1. 保持 oracle、负例和唯一 Verify，不接受通过跳过校验、放宽阈值或重试换速度。
2. 先用 `timingsMs`、CPU profile 和 allocation profile 定位热点。
3. 优先减少热路径分配、重复颜色转换和临时切片；缓冲复用必须按请求隔离并通过 race。
4. 保留双图并发、route 隔离和连接池；禁止跨代理共享连接或跨请求共享 token/session。
5. 分离记录Device逐轮冷建/关闭、同host连接等待和公开SDK/PE缓存命中，不把缓存命中误写成Device会话复用。
6. 外部阶段慢时定位 DNS、TLS、代理、CDN 或上游，不得靠放大 timeout 伪装性能优化。

## 结果模板

| 字段 | 要求 |
|---|---|
| 版本 | commit、dirty 状态、Go `1.26.6`、Rust `1.88.0`、V8 `149.4.0` |
| 环境 | OS、arch、CPU、内存、`GOMAXPROCS`、电源模式 |
| 数据集 | fixture/授权样本数量及普通、困难、负例分布 |
| 模式 | 逐请求冷建、直连/代理、并发度、`MaxConcurrency`与Device live峰值 |
| 样本 | 实测数、重复轮次 |
| 延迟 | mean、P50、P95、P99、max，单位 ms |
| 资源 | B/op、allocs/op、RSS、Go GC、V8 heap、goroutine/原生线程峰值 |
| 正确性 | oracle 通过数或在线严格成功数；失败分类 |
| 结论 | 逐项写明通过、失败或未测 |

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/challenge/solver.go:107`、`:149`、`:431` | 完整 Solver 已实现，并固定 12 个阶段耗时 key。 | Solve → stage timings → Result |
| `internal/challenge/solver_test.go` · `TestSolverOfflineCompleteSuccess` | 完整离线链成功路径和阶段输出已有 Mock 证据。 | device/init/assets/vision/PE/verify → outcome |
| `internal/pe/keys.go` · `NewKeyResolverWithCapacity`；`internal/pe/device_runtime.go` · `OpenDevice` / `Close`；`pkg/slider/client.go` · `NewClient` / `Prime` | 当前每个合法Solve立即冷建/关闭独立Device/V8，本地没有live并发槽；公开SDK/PE缓存仍复用；`Prime`是无外部请求的兼容空操作。 | Solve → Open → Complete → Close → Verify |
| `internal/server/server.go` · `decodeQueryRequest` / `handleSolve`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestSolveParameterSourcesStaySeparated` / `TestConcurrentRequestsAlwaysEnterSolver` | Handler 同时兼容 deprecated GET query 与 POST JSON，两者共用 Solver 链但不合并参数源；64 个并发合法 POST 全部进入 Solver。 | valid request → timeout context → Solve |
| `internal/server/openapi.go` · `legacyQueryParameters` / `solveResponses`；`internal/server/server_test.go` · `TestOnlyFrozenRoutesAreExposed` | OpenAPI 在四路径内同时声明 Solve POST/deprecated GET，两者只有 200/400/403/500，并回归禁止 429 与旧本地过载字段。 | API document → response contract → no local 429 |
| `pkg/slider/client.go:183` | 公共 Result 的 elapsedMs 来自 Solver total，而非客户端完整 HTTP 墙钟。 | Solver timings → library result |
| `internal/vision/solver_test.go:260`、`:271` | 视觉 benchmark 只提供均值/并行趋势；项目 P99 结论来自独立逐次纯计算门禁。 | repeated vision Solve → mean throughput only |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 32 路 Mock 正确性通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛；6,400 次离线 Solve 压力通过。 | concurrent mock / explicit local sampler / repeated stress → verified compute and capacity gates |
| `internal/pe/keys.go` · `Prepare` / `runtimeProfileCacheFresh` / `ProfileCacheStats`；`internal/pe/v8_runtime.go` · `runtimeProfileMatchesPureGo`；`pkg/slider/online_acceptance_test.go` | 精确分片只有经当前V8完整差分后才走纯Go；软5/硬30分钟且统计不暴露路径/key。历史预热池50次mean `2562ms`，未达1s；当前逐请求冷建路径待复测。 | StaticPath → source/profile singleflight → V8 oracle → verified pure-Go or V8 fallback → one Verify |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 200/32/应用层零重试候选批次成功率 `98%`、Client 完整求解链 P95 `984ms`，两项门槛通过；不经过 HTTP Handler，最终 one-shot 加固后未再在线重跑。 | authorized jobs → one Solve each → aggregate percentiles → bounded acceptance |
| `go.mod:3`；`Dockerfile:3` | 正式构建工具链固定 Go `1.26.6`。 | source → pinned build → comparable baseline |
