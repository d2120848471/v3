# 架构与资源所有权

HTTP 和 Go SDK 共用 `application/service.Service`。应用 `solve.Solver` 只决定单轮阶段、类型分流、校验、取消和结果；具体网络、Device/V8、动态脚本与文件写入由消费方定义的 ports 接入。生产对象只在 `bootstrap` 组装，内部目录不承诺外部兼容。

公开合同见 [SDK](../reference/go-sdk.md) 和 [HTTP API](../reference/http-api.md)，路径变更见 [迁移指南](../guides/migration.md)。

## 目录与职责

| 路径 | 当前责任 |
|---|---|
| [cmd/server](../../cmd/server) | 读取进程参数、构造 logger 和信号 context，调用 `bootstrap/server.Run` |
| [pkg/slider](../../pkg/slider) | 稳定 SDK 导出签名、DTO/错误格式及选项映射；`NewClient` 委托生产组装 |
| [internal/bootstrap](../../internal/bootstrap) | `NewService` 组装 SDK/HTTP 共享的生产资源，不依赖 HTTP 适配 |
| [internal/bootstrap/server](../../internal/bootstrap/server) | `Run` 管理监听、HTTP 服务、运行时自检、清理调度及关闭 |
| [internal/bootstrap/config](../../internal/bootstrap/config) | `Config`、flags/env/default 解析与启动配置校验 |
| [internal/interfaces/httpapi](../../internal/interfaces/httpapi) | HTTP 路由、JSON/query 输入、同源边界、trace/deadline、响应、OpenAPI 和内嵌页面 |
| [internal/application/service](../../internal/application/service) | 公共默认场景与超时常量；`Service` 协调并发调用、默认请求值和共享资源关闭 |
| [internal/application/solve](../../internal/application/solve) | 单轮业务编排、ports 与内部请求/结果；不构造网络或 V8 适配器 |
| [internal/domain/device](../../internal/domain/device) | 设备画像、指纹、配置/交互 DTO 与纯计算规则 |
| [internal/domain/pe](../../internal/domain/pe) | 纯 `Builder`、输入与结果、事件/时钟/data 自检；不下载脚本或启动引擎 |
| [internal/domain/protocol](../../internal/domain/protocol) | 编码、签名、加解密、token 与 data codec |
| [internal/domain/track](../../internal/domain/track)、[vision](../../internal/domain/vision) | 轨迹生成与转换、图像识别和几何规则 |
| [internal/foundation/runtimekit](../../internal/foundation/runtimekit) | 可注入的熵、UUID、时钟与可取消等待 |
| [internal/infrastructure/aliyun](../../internal/infrastructure/aliyun) | 实现单轮 port，持有挑战和 RPC 状态，映射上游 schema，下载图片并协调 Device/PE 适配 |
| [internal/infrastructure/aliyun/device](../../internal/infrastructure/aliyun/device) | 保留的兼容 Device RPC/session 实现；画像和 DTO 来自 domain |
| [internal/infrastructure/httpclient](../../internal/infrastructure/httpclient) | 代理规范化、HTTP(S)/SOCKS 与按路由隔离的 Transport 池 |
| [internal/infrastructure/engine](../../internal/infrastructure/engine) | 动态 Device 宿主、SDK/PE 下载与缓存、V8/纯 Go 差分、V8 fallback 及可选 Node oracle |
| [internal/infrastructure/artifact](../../internal/infrastructure/artifact) | 失败样本私有存储、脱敏 metrics、保留期和配额 |
| [internal/platform/v8runtime](../../internal/platform/v8runtime) | `purego` 加载动态库、版本化 C ABI、Isolate、host 回调和可取消执行 |
| [native/v8runtime](../../native/v8runtime) | 独立 Rust `cdylib` 项目，包装 V8/ICU；不属于 Go 包树 |
| [tests/architecture](../../tests/architecture) | 生产依赖方向和公共 SDK 黄金快照回归 |
| [build](../../build)、[docs](../README.md) | Docker/Windows 构建输入，以及指南、参考、设计与历史归档 |

## 依赖方向与生产组装

下面的箭头表示源码依赖。组装层可以同时知道 ports 与实现，但应用层不知道具体适配器：

```text
cmd/server ────────────────→ bootstrap/server
pkg/slider ────────────────→ bootstrap + application
bootstrap/server ─────────→ bootstrap + application + interfaces/httpapi
bootstrap ────────────────→ application + infrastructure
interfaces/httpapi ───────→ application/service + application/solve
application/service ──────→ application/solve
application/solve ────────→ domain + foundation
infrastructure/aliyun ────→ application/solve（实现消费方 ports）
infrastructure ──────────→ domain + foundation + platform
platform/v8runtime ──────→ purego / Rust C ABI
```

`bootstrap.NewService(Options)` 创建 `httpclient.TransportPool`、`engine.KeyResolver` 与 `artifact.Store`，把它们注入 `aliyun.NewRoundFactory`、`solve.New` 和 `service.New`。`bootstrap.resources` 实现共享资源 port；`bootstrap.failureRecorder` 把应用失败样本映射成 artifact metrics。共享选项的解析和校验在 bootstrap，SDK 只保留原公开字段的映射。

HTTP 的 `httpapi.New` 消费应用执行合同；`slider.Client` 把公开 `Request` 映射成 `solve.Request`，把 `solve.Outcome`/`solve.Failure` 映射回公开结果和错误。两种入口使用同一个 `service.Service` 实现，HTTP 不经过公共 SDK，业务应用也不反向导入 SDK、HTTP 或 infrastructure。

## 消费方 ports

Ports 按调用者需要定义，接口中不传入 `http.Request`、`http.RoundTripper`、V8 Isolate 或磁盘路径：

| 定义位置 | 合同 | 消费与生产实现 |
|---|---|---|
| `application/service` | `Executor.Solve` | `Service` 调用；`solve.Solver` 实现 |
| `application/service` | `Resources.CheckRuntime/PurgeArtifacts/Close` | `Service` 协调；`bootstrap.resources` 委托实际运行时、存储与连接池 |
| `application/solve` | `RoundFactory.NewRound` | `Solver` 在 setup 阶段调用；`aliyun.RoundFactory` 固定本轮画像、路由与客户端，尚不访问上游 |
| `application/solve` | `Round` | `Solver` 按阶段调用；Aliyun 适配器独占本轮设备/挑战状态，实施 RPC、下载及引擎调用 |
| `application/solve` | `FailureRecorder.SaveFailure` | `Solver` 提交脱敏 `FailureSample`；bootstrap 适配到 `artifact.Store`，记录失败沿用 best-effort 语义，不替换业务结果 |

`Round` 包含 `OpenDevice`、`Init`、`PreparePuzzle`、`DownloadImages`、`BuildPuzzle`、`CompleteDevice`、`SolveTraceless`、`SolveSliding`、`Verify` 与 `CloseDevice`。`PreparePuzzle` 和 `DownloadImages` 允许并发，其他动作按阶段顺序执行。`PuzzleInput`、`PuzzleProof`、`CompletionPlan` 和 `Verification` 表达应用需要的结果；上游 RPC 原文与动态宿主细节留在适配器。

视觉置信度门禁、轨迹生成、类型分流、阶段计时和失败分类属于应用编排；RPC 签名/响应 schema、连接路由、唯一网络尝试位和官方 SDK 消息合同属于具体适配。算法本身由 domain 提供，动态下载与宿主执行由 engine 提供。这些边界通过真实导入关系实现。

## 单轮流程与失败边界

```text
service 默认值 → solve 请求校验 → NewRound → OpenDevice → Init
  ├─ PUZZLE
  │    PreparePuzzle 与 DownloadImages 并行
  │    → vision/置信度 → track/BuildPuzzle → CompleteDevice
  │    → CloseDevice → Verify
  ├─ TRACELESS → 同一 Device 会话内的官方 SDK → SDK Verify → CloseDevice
  └─ SLIDING   → track → 同一会话内回放官方组件 → SDK Verify → CloseDevice
结果或错误 → service 调用计数归零
```

应用等待 PE 准备和双图下载都收束，两者均失败时保留 PE 准备错误优先级。图片下载适配器内部并行下载两张图，首个失败取消另一侧。低置信、设备、协议、构造或 context 门禁失败会停止后续阶段；所有早退路径都调用同一个幂等释放函数。

Puzzle 在完成 Device 后、Verify 之前即可关闭本轮 Device；RPCClient 在发送 Verify 前消耗唯一尝试位，并禁止请求 body 的透明回卷重放。网络结果未知也不恢复尝试位。TRACELESS/SLIDING 的 SDK Init 由桥接层绑定到已签发响应，不产生第二次真实 Init；其 Verify 次数、顺序及 success 与挑战/token 的绑定由适配器复核。应用再核对 `Verification` 与本轮 `CertifyID`，不会补发 Verify。

`solve.Failure` 提供稳定 `Kind/Stage/Message`；具体适配器使用 `solve.DependencyError` 传递分类，不要求应用识别网络库或引擎错误类型。SDK 和 HTTP 分别映射到既有公开错误，未分类错误/panic 在 HTTP 层脱敏处理。HTTP 细节以 [接口参考](../reference/http-api.md) 为准。

## 跨请求生命周期

| 资源 | 所有者与复用范围 | 释放时机 |
|---|---|---|
| 默认请求值、活动调用计数、关闭状态 | `service.Service`，SDK/HTTP 各宿主通常复用一个实例 | `Close` 标记关闭并等待已开始的操作 |
| Transport 与代理路由表 | bootstrap 组装的 `TransportPool`，按规范路由隔离；直连保留，代理 LRU | 淘汰时关闭空闲连接，service 关闭时清表 |
| 公开 SDK、精确 PE 源码与已验证 profile | `engine.KeyResolver`，按完整 StaticPath/SDK 内容复用；同分片请求合并 | 按访问检查 TTL，内容变化使画像失效；Go 引用随对象释放由 GC 回收 |
| PE Isolate | 每次调用独占、每次新 browser context，完成后可保留预加载空闲引擎 | 不健康、超出空闲保留量或 resolver 关闭时释放 |
| V8 动态库 | `KeyResolver` 持有，Device 与 PE 引擎引用 | service 等待活动操作结束后关闭 |
| Device Isolate、context、画像、session/token | 单轮 `Round` 独占，Open 到 Complete/SDK Verify 保持同一会话 | 本轮成功、失败或取消时关闭；不预热、不回池 |
| `CertifyID`、轨迹、data、Verify 尝试位与图片 | 单轮 `Round`/`Solver` 独占，不进入跨轮缓存 | 本轮引用释放 |
| 组件 JS/CSS 响应字节 | 单个 Device engine 的 host 缓存，精确 URL/headers 隔离；最多 8 项、30 秒 TTL | 随本轮 engine 释放引用 |
| 失败样本 | `artifact.Store` 私有目录；按目录串行配额处理 | 默认保留 7 天，启动及每小时清理；SDK 由宿主调度 |

SDK 软 TTL 为 5 分钟，精确 PE/profile 硬 TTL 为 30 分钟，均在后续访问时检查。SDK 字节未变可延长未达硬 TTL 的画像；字节变化使旧画像失效。TTL 不代表后台清理或条目数上限，缓存不保存挑战 token、DeviceConfig、轨迹和 data。

`MaxConcurrency` 是兼容字段名：限制每个 Transport 的 `MaxConnsPerHost` 和 PE **空闲**引擎保留量；没有空闲 PE 时可以新建。HTTP 请求、Device 会话与活跃 PE 执行都没有本地信号量上限，出站连接等待仍受 context 约束。

`Service.Close` 的顺序是：拒绝新工作 → 等待已开始的 `Solve`、`CheckRuntime`、`Prime`、`PurgeArtifacts` → 调用 `Resources.Close` → 关闭 engine 及 wrapper → 清空 Transport 池并关闭空闲连接。并发重复关闭会等待同一次关闭完成。它不主动取消 Solve，总超时或调用方 context 负责取消；`Prime` 只保留兼容空操作。

## 运行时与验证范围

Go 基线 `1.26.6`，唯一外部生产 Go module 为 `purego v0.10.2`；Go 构建禁用 CGo。独立 Rust wrapper 基线 `1.88.0`，锁定 V8 `149.4.0` 和 ICU data `0.77.0`。Linux 发布使用 Debian/glibc，Windows 包含同平台 DLL 与 notices。生产不启动 Node；Node bridge 只在可选 oracle 测试中运行，Python oracle 是静态 fixture。

`CheckRuntime` 校验本地库、C ABI 与 V8/ICU，不访问 Device RPC。Device JS 只能通过受限 Go host 发网络请求，PE 采样/fallback host 禁网；V8 Isolate 与 Go 同进程，不是操作系统沙箱。约束和残余风险见 [安全边界](security.md)。

[架构依赖测试](../../tests/architecture/dependencies_test.go) 检查生产导入方向，禁止应用导入 infrastructure/HTTP/SDK，并禁止 domain 使用 `net/http`、`os/exec` 等外部副作用依赖。[公共 API 测试](../../tests/architecture/public_api_test.go) 对照迁移前黄金快照。`make test` 自然包含这两类检查；行为测试、原生 ABI 与平台打包各覆盖不同边界，见 [开发指南](../guides/development.md)。离线或局部性能不能替代完整 Client/HTTP 和长时间资源验证，见 [性能说明](performance.md)。
