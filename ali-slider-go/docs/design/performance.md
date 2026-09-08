# 性能测试与容量口径

本页分别记录 2026-09-08 完整架构迁移后的离线复测与此前的局部优化。完整 V8/网络/HTTP 链路的线上分位数和长时间资源稳定性尚未测量。

## 架构迁移后的离线复测（2026-09-08）

在全部行为、race 和架构检查通过后，使用 Apple M3 Max、Darwin ARM64、Go `1.26.6`、`CGO_ENABLED=0`、`GOMAXPROCS=16` 顺序测量。迁移前使用开始本轮时保存的源码快照编译出的测试程序；迁移后使用当前未提交工作树，已包含进程桥 stdout 生命周期修复。两者均保留此前的视觉优化。

| 测量 | 本轮迁移前 | 本轮迁移后 |
|---|---:|---:|
| 困难合成图，20 次/轮、3 轮均值的中位数 | 31.825 ms/op | 30.315 ms/op |
| 同图并行，32 次/轮、3 轮均值的中位数 | 3.189 ms/op | 3.163 ms/op |
| 单次视觉分配 | 约 5.15 MB、296 次 | 约 5.15 MB、296 次 |

本次短样本没有显示视觉性能回退，不把这几个百分点归因为目录拆分带来的提速。并行数字按整体墙钟折算，不能解释为单次延迟；测量没有控制系统其他进程、电源模式和温度。

当前 200 次 `Vision → Track → 纯 Go PE/Data` 的 P50 为 `32.151 ms`、P95 为 `33.478 ms`、P99 为 `35.346 ms`、最大 `36.177 ms`，通过既有 `P99 <=100 ms` 离线门槛。该测试使用本地 fixture 和离线 Builder，不运行完整应用会话，也不包含 V8 或上游网络。

当前 Mock HTTP 基准的 3 轮均值中位数为 POST `3.734 µs/op`、旧 GET `2.658 µs/op`、OpenAPI `1.999 µs/op`；分配分别为 66、42、23 次。应用结果到 HTTP DTO 的独立 timing map 比前轮适配器多一次约 48 字节分配。这里包含 `httptest` 请求与响应记录器，Solver 是替身，不能作为真实 HTTP 完整链延迟。

上述视觉、HTTP 和纯计算复测使用下方“重复测量”命令；普通测试、race、静态分析和平台构建均在性能采样前结束。

## 目录迁移前的离线测量（2026-09-08）

环境：Apple M3 Max、Darwin ARM64、Go `1.26.6`、`CGO_ENABLED=0`、`GOMAXPROCS=16`。修改前为 `d280770d`，修改后为当时完成同包拆分与局部优化的未提交工作树，先于本轮应用边界及目录迁移。每组取 3 轮 benchmark 均值的中位数；这不是请求延迟 P50。

| 测量 | 修改前 | 修改后 | 说明 |
|---|---:|---:|---|
| 困难合成图 `BenchmarkSolve`，20 次/轮 | 52.801 ms/op | 34.202 ms/op | 耗时减少约 35.2%；约 5.15 MB/op、296 allocs/op 基本不变 |
| 同图 `BenchmarkSolveParallel`，32 次/轮、16 路 | 5.030 ms/op | 3.358 ms/op | 按整体墙钟折算，吞吐约提高 49.8%；不是单请求延迟 |
| Mock HTTP `PostSolve` | 3.880 µs/op | 4.163 µs/op | 分配均为 65 次；小样本耗时略升，不宣称 POST 适配层提速 |
| Mock HTTP `LegacyQuery` | 3.675 µs/op | 2.929 µs/op | 分配从 56 降至 41 次，约 8,141 → 7,524 B/op |
| HTTP `OpenAPI` | 65.974 µs/op | 2.128 µs/op | 分配从 1,366 降至 23 次；合同在构造时编码一次 |

HTTP benchmark 包含 `httptest` 请求和响应记录器的创建，Solver 使用固定离线结果；OpenAPI 不调用 Solver。HTTP 采用默认 benchmark 时长，不与视觉的固定次数混作一组统计。测量期间未控制电源模式、温度或系统其他进程，短样本只用于判断本次局部趋势，不是容量承诺。

当时 200 次 `Vision → Track → 纯 Go PE/Data` 顺序测量为：P50 `32.049 ms`、P95 `35.018 ms`、P99 `35.617 ms`、最大 `37.623 ms`，通过现有 `P99 <=100 ms` 离线门槛。这条路径使用静态 fixture 与离线 Builder，不包含 Device、动态分片首次自检或任何上游网络。

## 优化依据

修改前 3 秒 CPU 采样中，Chamfer 兜底匹配占约 69.6%，其中 alpha 距离场占约 32.7%；8-bit sRGB 转换中的幂运算占约 6.2%。前轮只消除语义相同的重复计算：

- sRGB 的 256 个输入值按原公式预计算，Lab 后续运算与舍入不变。
- Chamfer 在单次调用内根据 ROI 尺寸与 alpha 相对偏移复用距离场；边缘裁剪或偏移变化时重建。背景轮廓仍逐位置计算，取消检查保留。
- GET query 直接解析字符串，保留旧 JSON 中转路径的 UTF-8 替换、别名优先级和校验顺序。
- OpenAPI 在 Handler 构造时编码，读取路径只写入既有字节。

等价回归对照原始公式：256 项 sRGB 表、8,192 组 Lab 输入，以及边缘裁剪/相同尺寸不同偏移/固定随机帧的逐位置 Chamfer 数值均按 float64 位值检查；现有正例、负例、oracle 和并发确定性测试继续保留。没有调整识别阈值、轨迹、Verify 次数或 Device 会话复用策略。

## 重复测量

从 module 目录运行所有离线 benchmark：

```bash
make bench
```

复现上表视觉口径：

```bash
CGO_ENABLED=0 GOPROXY=off GOMAXPROCS=16 \
  go test -run '^$' -bench '^BenchmarkSolve$' \
  -benchmem -benchtime=20x -count=3 ./internal/domain/vision

CGO_ENABLED=0 GOPROXY=off GOMAXPROCS=16 \
  go test -run '^$' -bench '^BenchmarkSolveParallel$' \
  -benchmem -benchtime=32x -count=3 ./internal/domain/vision

CGO_ENABLED=0 GOPROXY=off GOMAXPROCS=16 \
  go test -run '^$' -bench '^BenchmarkHTTP$' \
  -benchmem -count=3 ./internal/interfaces/httpapi
```

依赖未缓存时先按 [开发指南](../guides/development.md) 准备工具链和锁定依赖。比较前后快照时保持相同环境，顺序执行，不同时运行 race、覆盖率或其他负载测试。

纯计算分位数入口：

```bash
CGO_ENABLED=0 GOPROXY=off GOMAXPROCS=16 ALI_SLIDER_PERF=1 \
  go test -run '^TestPureComputeP99$' -count=1 -timeout=60s -v ./internal/infrastructure/aliyun
```

该门槛只针对 Darwin ARM64；其他平台跳过不表示通过。需要定位新热点时，可给单个 benchmark 增加 `-cpuprofile` 和 `-memprofile`，将输出放入临时目录，再用 `go tool pprof` 分析。

## 当前实现中的计时

`solve.Solver` 返回固定 `timingsMs` key：

| Key | 边界 | 主要内容 |
|---|---|---|
| `setup` | 请求校验后到 `RoundFactory.NewRound` 完成 | 画像选择、route/transport、单轮适配与兼容设备客户端准备 |
| `deviceSession` | Open 开始到首枚 token 可用 | 本轮独立 V8 Device Isolate 冷建和 Log1/2/3、实际画像绑定及 Captcha RPC 客户端创建；不含本地会话槽等待 |
| `init` | Captcha Init 请求往返 | InitCaptchaV3 |
| `traceless` | 无痕 SDK 分支 | 同会话组件运行、唯一 Verify 与结果合同复核 |
| `sliding` | 无图拖动 SDK 分支 | 轨迹生成、同会话组件回放、唯一 Verify 与结果合同复核 |
| `resolvePEKey` | Init 后到当前 PE 结构画像可用 | 缓存命中，或公开 SDK/PE 下载、V8 采样与纯 Go 完整差分；SDK 软 TTL 5 分钟，PE 硬 TTL 30 分钟 |
| `downloadAssets` | 两图下载整体墙钟 | 背景图和 shadow 并行下载 |
| `downloadBackground` | 单图下载 | 背景图 |
| `downloadShadow` | 单图下载 | shadow |
| `vision` | 两张 PNG 到识别结果 | PNG 解码、缺口定位、置信度和几何 |
| `buildVerifyData` | 轨迹到 PE data | 已验证分片使用动态 profile + 纯 Go Builder；其余使用禁网 V8 fallback；两者都做 Pack/Unpack、getter、事件和时钟合同自检 |
| `completeDevice` | 设备完成态 | 同一 FeiLin VM 事件回放/getter、Verify token 和最终 Log2 |
| `verify` | Captcha Verify 请求往返 | 唯一一次 VerifyCaptchaV3 |
| `clientCleanup` | release 开始到返回 | 关闭本轮Device/V8；不补货、不回池 |
| `total` | Solver 进入到本轮清理完成时计时 | 包含已执行阶段与 Device 清理；失败样本 best-effort 保存发生在计时写入之后 |

固定键中未执行的分支保持 0。PE 准备与双图下载重叠，双图本身也并行，因此阶段不能直接相加；毫秒取整也会产生差异。

`Result.ElapsedMS` 来自 Solver 的 `total`，不包含客户端到 HTTP 服务的建连、请求解析和响应传输。完整 HTTP P95 应以负载发生器的端到端墙钟为主，以应用日志和 `timingsMs` 做归因，不能只排序 `elapsedMs`。正式基准优先使用 POST JSON；deprecated GET query 只是协议兼容性输入，不是另一条更快的求解链。

## 指标边界与统计定义

| 指标 | 测量范围 |
|---|---|
| 视觉算法 | PNG 输入到坐标和置信度；不含 PE、Device 或网络 |
| 离线纯计算链 | 视觉、轨迹、离线 PE/Data 构造与自检 |
| Solver total | Solver 进入到本轮清理完成时计时，包含上游等待；不含末尾失败样本保存 |
| HTTP 完整请求 | 调用方发出请求到读完整个响应，包含建连、HTTP、Solver 和上游等待 |
| 吞吐 | 窗口内完成数 / 墙钟秒数；成功、业务拒绝、技术失败分别记录 |

Go benchmark 的 `ns/op` 是迭代均值；`RunParallel` 的均值按整体墙钟计算。两者不能推导尾延迟。请求分位数使用 nearest-rank：升序第 `ceil(q × N)` 个样本；200 次测量的 P95 为第 190 个，P99 为第 198 个。

## 容量与在线验收

每个 Solve 冷建独立 Device/V8 会话并在结束时关闭。公开 SDK/PE 源码与已验证画像按 TTL 复用；DeviceToken、CertifyId、轨迹和 data 不跨轮复用。

`MaxConcurrency` 只控制每 route/host 出站连接与 PE 空闲引擎保留量。HTTP 在途请求、活跃 PE 调用和 Device 会话没有本地信号量上限；64 个 Mock 请求可同时进入 Solver，并不证明生产服务可承受相同容量。当前重构保持此合同。

完整链验收需要单独声明运行平台、上游环境、场景/验证码类型、直连或代理、样本数和并发，再记录成功率、各类失败、端到端 P50/P95/P99/max 及 RSS、GC、goroutine、线程峰值。统计全部样本；业务失败和超时不能从报告中静默剔除。每轮最多一次 Verify，结果未知不重试。

线上执行需要覆盖目标与请求数量的明确授权。当前尚无本次快照的完整 Client/HTTP 验收，也没有据此承诺“所有请求低于 1 秒”或固定生产吞吐。

## 历史记录

此前的近似纯 Go 实现、预热池实验和组件 smoke 均绑定当次源码及环境。原始数据继续保留于 [历史证据索引](../README.md#历史证据)，尤其是 [2026-08-09 性能证据](../archive/evidence/validation-2026-08-09-performance.md) 与 [2026-08-11 运行时优化](../archive/reports/2026-08-11_js-web-captcha-runtime-performance-report.md)。这些结果不作为当前逐请求冷建路径的验收结论。
