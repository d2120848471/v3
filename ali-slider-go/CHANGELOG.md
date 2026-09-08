# Changelog

本文件记录 `ali-slider-go` 的用户可见变化。格式参考 Keep a Changelog，版本遵循语义化版本。

> **发布状态**：`1.0.0` 仍为 Unreleased。当前使用同进程 V8，每轮独立冷建并关闭 Device，会话不跨请求复用。目录重构保持 HTTP 与公共 Go SDK 合同；完整 Client/HTTP 线上性能和长时间资源稳定性仍需验证。下方历史记录的路径、版本、预热池和测量数字仅描述当时快照。

## [1.0.0] - Unreleased

### 应用边界与目录重构（2026-09-08）

- 单轮编排迁入 `internal/application/solve`，通过消费方 `RoundFactory`、`Round`、`FailureRecorder` 接入外部能力；应用不依赖 infrastructure、HTTP 或公共 SDK。
- `internal/application/service` 统一 SDK/HTTP 的默认请求值、活动调用与关闭协调；`internal/bootstrap` 成为共享资源组装点，HTTP 启动位于 `internal/bootstrap/server`，`cmd/server` 保留薄入口，`pkg/slider` 保留公开 DTO、方法和兼容映射。
- 纯 device/PE/protocol/track/vision 归入 domain；网络、Aliyun 单轮/RPC、engine 和 artifact 归入 infrastructure；V8 ABI 归入 platform，运行时基础注入归入 foundation。原扁平 internal 路径不保留兼容空壳。
- 保持 module 根及 `pkg/slider` 公共包路径。新增生产依赖方向与迁移前 SDK 黄金快照检查，`make test`/CI 自然覆盖。
- 增加迁移前 24 组 HTTP 实际响应快照对比，以及 SDK 不得间接引入 HTTP 接口包的依赖检查。
- 修正可选 Node bridge 的 stdout 所有权：子进程退出后，末阶段输出仍可读取，读端由会话关闭。新增确定性退出时序与读端释放回归，避免并发 `Wait` 提前关闭管道。
- Dockerfile 迁到 `build/docker/Dockerfile`，context 仍为 module 根；Windows 打包素材迁到 `build/packaging/windows`，最终 ZIP 结构不变。Makefile、CI 与换行规则同步迁移，工具版本及覆盖率 80%/90% 门槛不变。
- 文档分为 guides、reference、design 与 archive；根/module README 保留接入与导航。原始历史报告和证据保留，前轮离线数字与本轮复测分开标识。目录与命令映射见 [迁移指南](docs/guides/migration.md)。

### 目录迁移前的兼容整理与离线优化（2026-09-08）

- 按职责拆分公共 SDK 的配置与错误、HTTP 的输入输出、Solver 的合同与初始化；保持公共 Go 签名及既有 HTTP 字段、状态、别名和唯一 Verify 行为。
- PE 设备运行时按会话、桥接、代理与合同校验分文件，保留原函数体与可选 Node oracle，补充当前包级说明。
- 内部 Solver 明确要求调用方注入并管理 PE resolver；动态设备路径跳过未使用的兼容客户端构造。
- OpenAPI 增加稳定 operationId，并在 Handler 构造时编码复用；旧 GET query 去掉 JSON 编解码中转，保留原有文本语义。
- 视觉 sRGB 使用原公式查找表；Chamfer 在单轮内复用相同几何条件的 alpha 距离场，新增逐位等价与裁剪边界回归。
- 整理项目与文档入口，区分当前行为与历史证据；新增 `make bench` 和可执行 SDK 示例。离线困难图基准耗时约减少 35%，范围与命令见 [性能文档](docs/design/performance.md)，不代表线上完整链结果。

### 会话生命周期变更（2026-08-19）

- 服务启动不再调用 `Prime`，也不创建 Device/V8 会话或访问 Device RPC；`Client.Prime` 仅作为兼容 no-op 保留。
- 每个合法 `Solve` 都立即冷建独立的 Device/V8 完整会话，并在完成或失败后关闭，不再回收 Isolate、补充预热库存或等待本地 live 会话槽。
- 取消 Device/V8 本地并发保护；`MaxConcurrency` 只保留为每 route/host 的 Transport 连接与 PE 资源预算，不限制 Device 会话数或 HTTP 在途请求，也不触发本地 `429`。2026-08-09 发现的5个对齐存活VM字段合同异常保留为已知风险，当前无界冷建路径尚未真实在线验收。
- 删除内部 `DeviceSessionPool`、Lease 注入和 `Recycle` 入口；`clientCleanup` 现在包含真实 Device/V8 关闭与槽位释放耗时。
- 公开 SDK、精确 PE 源码及 profile 缓存继续保留；DeviceToken、`CertifyId`、轨迹和 `data` 仍不跨轮复用。
- `DevicePrewarmCapacity`、`DeviceSessionReserve`、`--device-prewarm`、`--device-reserve` 及对应环境变量仅为旧配置兼容保留，现在只接受 `0`，非零值会在配置或 Client 创建阶段报错。
- Go module 与 Docker 构建层同步升级到 Go `1.26.6`，修复 Go `1.26.5` 标准库漏洞导致的 `govulncheck` 门禁失败。

下方其他条目保留 `1.0.0` 开发过程的里程碑；其中提到的预热、Lease、Recycle 和补货已被上述当前变更取代，不再是待发布版的运行行为。

## 历史开发里程碑

以下原始条目记录早期 1.0.0 开发快照。旧路径和阶段性通过状态不表示当前目录或本轮验证结果；当前配置/接口/命令以 [文档索引](docs/README.md) 为准。

### Added

- 增加 `native/v8runtime` Rust `cdylib`，锁定 `v8=149.4.0` 与 ICU 77 data，暴露版本化 C ABI、状态 Isolate、Go host 回调、context 终止、heap/输入/输出上限。
- 增加 `internal/v8runtime` 的 `purego` 动态库加载层，支持 Linux AMD64/ARM64 和 Windows AMD64，生产 launcher 保持 `CGO_ENABLED=0`。
- 增加服务启动 `Client.CheckRuntime`：在预热与 ready 之前检查 wrapper 文件、C ABI 和 V8/ICU 初始化。
- 增加 Linux AMD64/ARM64 Rust native + Go→V8 ABI 门禁、Windows AMD64 DLL 原生门禁和 `THIRD-PARTY-NOTICES.txt`。
- 新建独立 Go module，固定 Go `1.26.5`；生产仅引入 `purego` 动态库加载依赖，Go launcher 保持 `CGO_ENABLED=0`。
- 增加可并发复用的公共 `slider.Client`：`NewClient`、`Solve`、`Prime`、`PurgeArtifacts` 和幂等 `Close`。
- 增加完整 Go 编排：V8 Device → InitCaptchaV3 → 双图下载 → Vision → Track/V8 动态 PE → 同一 Device Isolate Complete → 唯一 VerifyCaptchaV3。
- 增加 `TRACELESS` 无痕分支：同一 SDK/FeiLin VM 完成官方 Init/Verify，并返回与图片拼图一致的 Verify 结果字段。
- 增加 `SLIDING` 无图拖动分支：根据 Init 类型自动分流，在同一 SDK/FeiLin VM 渲染官方组件、回放 `370px` 拖动轨迹、验收唯一 Verify，并保持公共结果格式兼容。
- 增加稳定请求、结果和分类错误合同，包含完整阶段 `timingsMs`。
- 增加 JS/RPC 编码、RPC v1 签名、AES-CBC、DeviceToken、data codec、Verify 参数和自校验。
- 增加设备画像、浏览器头、指纹、Log1/2/3、首枚/刷新 token、getter/event 和单会话完成态。
- 保留纯 Go PE builder 作为历史 oracle/离线兼容实现：轨迹字段、坐标、逻辑时钟、getter plan、交互事件及 Pack/Unpack 自检。
- 增加动态 PE `KeyResolver`：公开 SDK 与精确 PE 源码/profile 分层缓存；每个精确分片由当前 V8 采样并与纯 Go 完整差分，兼容后纯算本轮 `data`，不兼容则使用 V8 fallback；两者都校验 schema、坐标、getter、事件和时钟合同。
- 增加持久 V8 设备会话：同一个公开 SDK/FeiLin Isolate 从 Log1/2/3 保留至 PE getter，回放本轮原生交互事件后生成同 session 的 Verify DeviceToken。
- 增加 Captcha Init/Verify RPC client；每个 client 最多尝试一次 Verify，网络结果未知也不重发。
- 增加 HTTP(S)、SOCKS4、SOCKS5、SOCKS5H 和按 proxy route 隔离的有界连接池。
- 增加有界设备预热池：route/prefix/timing key隔离、每slot独立画像、20秒默认年龄、默认直连池最多保留4个VM、并行Prime、满池等待、Lease、过期和异步补货。
- 增加固定 HTTPS CDN 的双图并发内存下载、逐跳重定向白名单和响应大小限制。
- 增加纯 Go PNG 安全解码、缺口定位、困难 contour/chamfer fallback、置信度和几何换算。
- 增加 embedded 轨迹 fixture、目标距离缩放、抽稀和有界扰动。
- 增加失败/低置信 artifact 私有存储；完整 Solver 自动写入脱敏图片/指标。
- 增加 `cmd/server`：配置、预热、HTTP 服务、非回环警告、启动/每小时 artifact 清理及信号优雅关闭。
- 增加 HTTP Handler、OpenAPI 3.0.3 JSON、Trace ID、请求级 timeout 和脱敏错误映射。
- 增加 `GET /` 第一方内嵌 API 测试页：同源 health/solve、手工单次提交、取消/客户端等待上限、状态/耗时/trace 展示与敏感字段默认遮罩；不增加 Node、CDN 或 ZIP 文件。
- 增加 `Makefile` 的 fmt/vet/test/race/staticcheck/govulncheck/coverage/Linux 静态构建目标。
- 增加 Go CI `.github/workflows/ali-slider-go-ci.yml`，固定工具版本、覆盖率阈值和静态二进制断言。
- 增加 Windows AMD64 便携包：Go 服务 EXE、静态 CRT V8 DLL、第三方 notices、双击启动脚本、中文说明、构建信息和 SHA-256。
- CI 增加 Windows 2025 原生 test/vet、PE 构建、最终 ZIP 解压 smoke 与单层 artifact 上传；官方 actions 使用完整 commit SHA 固定。
- 增加 `docs/evidence/validation-2026-08-07.md`，固化命令、聚合数字、构建哈希与在线快照时序边界。
- 增加 `docs/evidence/validation-2026-08-08-no-local-admission.md`，固化删除本地准入闸门后的离线合同、质量门禁、源码选择集摘要与临时 Linux 二进制哈希。
- 增加 `docs/evidence/validation-2026-08-09-performance.md`，固化动态 PE 自校验、真实 50 次 A/B、Evidence → Finding → Path 与未达 1 秒的明确边界。
- 增加 multi-stage `Dockerfile`：Go `1.26.5` launcher 构建、Rust `1.88.0` V8 构建/native 测试、Go→V8 ABI 测试、Debian bookworm-slim 非 root UID/GID `65532` 运行层。
- 增加架构、API、配置、安全、测试、性能、迁移和排障文档。

### Changed

- TRACELESS 显式关闭官方 success 回调延迟；SLIDING 让 `Date.now()`、`performance.now()` 与事件 `timeStamp` 同步推进逻辑轨迹，回放循环只清空 microtask，不使用真实 timer/macrotask 等待。
- Device V8 engine 增加30秒、8项的版本化组件 JS/CSS 响应缓存；仅允许指定阿里 CDN 动态资源路径的安全 GET，挑战 API、Cookie/Authorization、不可缓存响应及不同代理/画像/Isolate 均严格隔离。
- 生产设备/动态 PE 从 Node 子进程迁到同进程 V8：Device 从 Open 到 Complete 保持同一 Isolate；PE 的首次画像与不兼容 fallback 使用单独禁网 context，已完整差分的分片使用纯 Go Builder；Node 仅保留为可选测试 oracle。
- Device Complete 后可回收外层 V8 Isolate，下一轮仍重建独立浏览器 context、session ID、DeviceToken 和 FeiLin 状态；回收失败时关闭并冷补货。
- PE 构建改为逐精确 `StaticPath` 的动态门禁：首次/到期时使用当前 V8 采样并与纯 Go 逐字段差分，仅完整一致才启用纯 Go 快路，其余保留 V8 fallback。
- SDK 缓存改为 5 分钟字节复核；SDK 未变时延长精确分片软 TTL，但每个 PE/profile 最多 30 分钟强制重下与重做 V8 差分。
- SDK 与同分片源码/profile miss 分别单飞，不同分片可并发；PE Prepare 与双图下载重叠。
- 新增 `DeviceSessionReserve` / `--device-reserve` / `ALI_SLIDER_DEVICE_RESERVE` 的0..4有界A/B开关；`prewarm + reserve`硬限制不超过4，因reserve真实测试增加争用，默认为0。
- 运行时配置改为 `--v8-library` / `ALI_SLIDER_V8_LIBRARY` / `V8RuntimeLibrary`；默认加载可执行文件同目录的平台 wrapper。
- Docker 运行层改为 Debian bookworm-slim，包含 Go launcher 和同架构 V8 `.so`；Windows 包改为 EXE + 静态 CRT V8 DLL，两者均不含 Node。
- 运行时从 Python/OpenCV/浏览器 worker 模型切换为 Go 主进程 + 进程内 V8 Isolate；公开脚本与结构画像按 5 分钟复用，挑战级状态逐轮生成。旧 Python 源码已从当前工作树删除，需要审计或回滚时从 Git 历史提交恢复。
- 服务合同只开放 `/`、`/api/slider`、`/health` 和 `/openapi.json` 四个路径；Solve 同时支持推荐的 POST JSON 和已废弃的旧 Python GET query。
- 恢复 `GET /api/slider?...` 兼容合同：8 个精确字段/别名、同名 query 取最后值、规范名压过别名、空值回退默认；GET/POST 参数源不合并，不支持 form body。
- 移除 HTTP 本机 admission gate 与主动 `429 Retry-After`；每个通过浏览器跨源和输入校验的 GET/POST Solve 请求都在 timeout context 内直接调用 Solver。
- 图片默认在内存处理；成功路径不落图，失败/低置信按 artifact 策略私有落盘。
- 默认监听收紧为 `127.0.0.1:8000`；应用内仍不提供鉴权。
- prefix 收紧为 1–32 个 ASCII 字母数字；proxy 必须使用支持的 scheme 且不含 path/query/fragment。
- 普通日志只记录事件、traceId、HTTP 状态和耗时，不记录 token、`CertifyId`、代理凭据或正文。
- 默认设备预热容量改为4，并随更小的 `MaxConcurrency` 收敛；逐请求proxy因route key不同按设计冷建。
- artifact 默认保留 7 天；服务启动时清理一次，此后每小时清理。
- Windows Artifact 权限改为继承解压目录 NTFS ACL；Unix 继续强制 `0700/0600`。

### Fixed

- 修复 V8 142 在 Linux AMD64 共享库链接时的 TLS relocation 问题：升级到包含上游 shared-library TLS 修复的 V8 `149.4.0`。
- 修复 FeiLin Promise/microtask 递归导致 OOM、ICU data 缺失导致 `Intl.DateTimeFormat` abort，以及 `performance` 属性不可配置造成的 PE Proxy 不兼容。
- 修复部署缺少/错架构 wrapper 时服务仍报告 ready：现在启动即失败，不再等到全部 Solve 返回 `InternalError`。
- 修复把动态 PE 简化成静态 key + Go 近似构造导致新 `.058` 分片持续返回 `F001`：删除随机兜底，按每轮 `StaticPath`、`CertifyId`、DeviceConfig、轨迹和时钟执行当前 PE；同时兼容当前 10 字段与历史 11 字段 TrackList。
- 修复纯 Go Device 指纹没有保持旧版 Init→Verify 的同一 FeiLin VM：设备 Node 会话跨阶段持久化，Init 固定验证 111 个字段，Verify 接受真实事件回放后动态扩展的安全字段数（实测 142），但仍独立验证 token、session、时钟和 action 序列。
- 修复缓存边界：SDK 每 5 分钟做字节复核，精确 PE/profile 最多 30 分钟强制重采样；DeviceToken、`CertifyId`、轨迹、`data` 不进入分钟级缓存。预热会话的挑战状态最多空闲 20 秒且一次性消费，仅外层 Isolate 可回收。
- 修复 HTTP(S) 与 SOCKS 代理下设备流量未统一路由：V8 JS 只能调用受限 Go HTTP host，全部请求复用本轮 Go transport，代理凭据不注入 JS。
- 修复 Docker `scratch` 和旧 Windows 单 EXE 没有逐挑战运行时：容器切到 Debian/glibc 并携带 `.so`，Windows ZIP 携带静态 CRT DLL 与第三方 notices。
- 修复 Linux ARM64 包曾混入 AMD64 Go launcher，以及 Alpine 构建层生成的 musl 解释器与 Debian 运行层不兼容：BuildKit 现使用真实 `TARGETARCH`，Go 构建层统一为 bookworm/glibc，CI 强制 launcher 与 `.so` 同架构。
- 修复容器 CI smoke 未设置容器内 `ALI_SLIDER_HOST=0.0.0.0` 而无法从宿主端口映射访问；应用本身仍保留 `127.0.0.1` 安全默认。
- 修复 Device RPC 响应 schema：保留 `ResultObject` 原始 JSON，仅在 Log1 解码对象内的 `DeviceConfig`；Log2/Log3 成功响应允许非对象结果，避免真实设备链被误判为协议错误。
- 增加 Device schema 离线回归和显式授权的在线 Log1/2/3 探针；在线设备初始化通过，耗时约 `0.53s`。
- 禁用 Verify POST 的 `GetBody` 回卷入口，阻止 `net/http` 在 HTTP/2 GOAWAY/REFUSED_STREAM 等已发送 body 的失败后透明重放。
- 设备 Log1/Log2/Log3 同样使用不可回卷请求体，保证一次性会话动作不会被 Transport 在已发送后透明重放。
- 修复零值 `ClientOptions{}` 漏补默认设备预热容量；显式关闭预热仍通过 `DefaultClientOptions()` 后覆盖为零。
- 修复 Windows 无法表达 POSIX `0700` 导致 Artifact 保存/清理失败；保留目录真实性、同文件复核、symlink 拒绝、排他创建和配额保护。
- 修复 Darwin ARM64 race 插桩下 32 路离线正确性测试误触 5 秒 Solver 时限；只放宽测试时限，不改变生产 timeout 或性能门槛。
- 修复同分片 profile 高并发下的 SDK 时间戳竞态和晚到 miss 重复采样；时间在获锁后读取，分片下载在进入 singleflight 后二次检查缓存。
- Device Complete 成功后在 Verify 前提前 release，使新 context 回收与 Verify 网络重叠；`sync.Once` 保证任何返回路径只 release 一次。
- Rust V8 wrapper 增加精确源码键的进程内 code cache，最多 64 项/64 MiB，不改变 child context 隔离或 ABI。
- 修复进程级固定画像被全部预热会话共享：现在每个live slot生成独立画像，Solver以租到会话的实际画像统一构造RPC headers、图片请求和PE输入。
- 修复大预热使Device VM同时Open/Complete后出现137/138字段或request sequence错误：默认直连公开组件A/B确认预热池库存边界为4，配置、Client、池和V8动作gate统一执行该边界。
- 修复满池matching Lease绕过池额外冷建及release广播早于refill登记的竞态；现在等待已有slot recycle，并在广播前占住pending。
- 修复无图 `TRACELESS` Init 被拼图图片校验拒绝，并避免无痕挑战误入图片、视觉、轨迹和 PE 链路。
- 修复无图 `SLIDING` Init 被双图校验拒绝并误入 Puzzle 链；补齐 CSSStyleDeclaration、元素事件/冒泡、鼠标坐标和 `insertAdjacentHTML` 的最小浏览器合同，使官方拖动成功分支能够到达 `onBizSuccess`。

### Security

- 分别限制 HTTP JSON body 和 raw query 为 64 KiB，并在进入 Client 前校验 JSON/URL encoding、类型、长度、prefix 和 proxy。
- 测试页使用每响应 128-bit 随机 CSP nonce，只允许同源 connect；不使用 Cookie、浏览器存储、遥测、外部资源或危险 DOM 注入 API，结果刷新即清除。
- 使用 Go 1.26 标准库 `http.CrossOriginProtection` 拒绝明确浏览器跨源 GET/POST Solve；有副作用的 GET 在检查副本中按 POST 对待，返回脱敏 `403 ApiOriginError` 且不进入 Solver，同源页和无浏览器来源头的 curl/程序客户端保持兼容。
- HTTP Handler 不内置访问频率或并发准入保护；对外暴露必须由受控网络、反向代理或 API gateway 限制身份、频率、并发和总量。
- 每个 `CertifyId` 最多尝试一次 Verify；前置失败为零 Verify，网络未知也不重试。
- 动态公开脚本限制为精确 HTTPS host/默认端口/路径格式和 2 MiB；V8 C ABI/host 回调有输入输出、heap 和超时上限。Device Isolate 只能通过 Go host 访问 `g.alicdn.com`、`x.alicdn.com` 或 `*.aliyuncs.com` HTTPS 且逐跳校验重定向；PE Isolate禁网。挑战输出由 Go 独立复核。
- 代理 route 和每 host 连接数限制在 `1..32`；直连显式忽略环境 proxy。
- 设备预热池只复用完整 endpoint/prefix/region/route/profile/timing key 一致的会话。
- 资产固定 HTTPS CDN，重定向逐跳校验；单图默认最多 8 MiB。
- PNG 在完整解码前检查签名、IHDR、尺寸、像素数和 shadow alpha。
- Unix artifact 目录/文件使用 `0700/0600`；Windows 继承 NTFS ACL；随机名以 `O_EXCL` 创建，清理不跟随 symlink。
- Solver、HTTP、日志和 artifact 测试覆盖 token、CertifyId、代理密码及原始 cause 脱敏。
- Docker runtime 固定 Debian bookworm-slim + V8 wrapper，Go 服务以非 root UID/GID `65532` 运行。

### Testing

- Go `1.26.5` 下全量 `go test`、`go test -race`、`go vet`、staticcheck 和 govulncheck 已验证通过；普通门禁和 Darwin race 都固定 `CGO_ENABLED=0`。
- Linux AMD64/ARM64 Rust wrapper 单测、Go→V8 实际 ABI 测试、launcher + `.so` 包构建已验证通过。
- Windows AMD64 发布工作流要求原生 Rust/Go→DLL 测试、最终 ZIP 解压启动、HTTP 合同、文件白名单和 SHA-256 全部通过。
- Linux AMD64/ARM64 Docker 镜像构建及非 root `/health` smoke 已验证通过。
- 全项目统一语句覆盖率为 `80.4%`，达到 `>=80%` 门槛；`internal/v8runtime` 为 `91.4%`，发布 commit 仍由 CI 重跑归档。
- Python 协议 oracle 与 fuzz seeds 对照通过；`internal/protocol` 覆盖率 `91.4%`。
- Python edge-decoy 正负视觉 fixture 对照通过；`internal/vision` 覆盖率 `91.9%`。
- PE builder 的 Python oracle 解包语义对照通过；加入逐挑战 Device/PE runtime 后，`internal/pe` 离线覆盖率为 `73.9%`，真实 `.so` 路径另由 Linux 双架构 V8 集成门禁覆盖。
- 动态 PE/设备 runtime 的 5 分钟精确缓存、24 路并发 miss 合并、不安全路径、下载上限、wrapper/ABI 失败分类、持久 Isolate、指纹字段、getter/事件/时钟合同和 V8 host 生命周期均有测试；生产 V8 公开 `.058` 分片探针可显式复现。
- 完整 Solver Mock 链通过：成功路径一次 Verify、低置信零 Verify、Verify 网络错误一次尝试。
- TRACELESS 离线合同与显式在线单次组件 smoke 通过；在线结果 `T001 / true`，未调用站点短信 API。
- SLIDING 离线合同与显式在线单次组件 smoke 通过；`SceneId=159tlu75`、`prefix=1ulc59` 得到 `T001 / true / requests=7`，未调用 DJI 登录或短信 API；单样本不外推成功率或性能。
- 完整 Solver 的 32 路并发 Mock 正确性测试通过；该测试不作为吞吐、资源或尾延迟报告。
- 设备预热池通过有界容量、key 隔离、过期、取消、失败、补货及并发 Lease/Close 的 race 测试。
- HTTP 四路径、Solve 两种 method、旧 GET query 重复键/默认值/64 KiB/非法 encoding、GET/POST 参数源隔离、内嵌页安全头/nonce/无 Solver 副作用、两种 method 跨源 403 与旧客户端兼容、64 个并发合法请求全部进入 Solver、OpenAPI 不含本地 429 均有离线测试。
- 困难视觉 benchmark 均值为 `53.10ms/op`；独立纯计算 200 样本 nearest-rank `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`，满足 `P99<=100ms` 硬门槛。
- 唯一授权候选批次恰好执行 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试：严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。该批先于传输 one-shot 加固，最终源码受授权上限约束未再在线重跑。
- 候选批次直接调用 `Client.Solve`；Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，总批次约 `6.16s`；`>=190/200` 与 Client 完整链 `P95<=1000ms` 均通过。
- 2026-08-08 旧运行时快照曾用 1 个新挑战、一次 Solve、并发 1、零重试得到 `1/1 T001`，墙钟约 `1579ms`；它只作历史对照。
- 2026-08-09 当前路径强制 V8/纯 Go 差分为 0；V8 约 `530ms`，纯 Go 约 `0.364ms`。
- 快路后中间候选真实50次为 `49/50`、mean `2007ms`，`buildVerifyData` mean `2ms`；随后 `48/50`、mean `1890ms` 的中间批次发生在live Device边界确认之前，不是最终安全默认路径。
- 最终安全候选真实50次为 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`；32/32精确分片通过完整V8/纯Go差分，后25次mean `2232ms`。默认直连公开Device组件以4个池槽服务10个job全部通过；5个对齐存活会话已复现字段合同失败。

### Known limitations

- 本次1秒是并发10、50次Client完整求解链算术平均目标；最终安全候选mean `2562ms`，明确未通过。后25次mean `2232ms`不能冒充全批结果，也不能提高默认直连池4槽边界换速度。
- 候选批次未经过 HTTP Handler，HTTP 端到端真实 P95 尚未单独测量。
- 候选批次没有保留精确起止时间或阶段聚合，不能事后补造；后续发布批次必须在发送首个挑战前启用完整审计采集。
- 已有离线 Mock 的短时 maxRSS 记录，但生产形态 32 路 RSS、GC、goroutine 峰值和长时间稳定性压测尚未执行。
- 应用内没有鉴权；对外暴露必须由受控网络、反向代理或 API gateway 提供访问控制。
- 应用内没有 admission gate；请求激增可导致连接等待、超时、GC 压力、内存耗尽或进程退出，不承诺无界并发下的延迟与可用性。
- 逐请求 proxy 允许调用方指定网络出口，外部部署必须限制调用方或实施 allowlist。
- 已废弃 GET query 为兼容保留真实副作用，参数可进入 URL 历史或访问日志；不应携带 `AaduaneId` 或代理凭据，新接入应使用 POST JSON。
- Python 业务提交/重放、交互式 CLI、桌面 UI、PyInstaller 和 Swagger UI 不属于 1.0.0 范围；`GET /` 第一方测试页不改变这些排除项。
- Windows EXE 尚未配置 Authenticode 代码签名，Defender/SmartScreen 可能提示未知发布者；SHA-256 不能替代发布者签名。

## 历史证据定位

| Evidence | Finding | Path |
|---|---|---|
| `go.mod:3`；`Dockerfile:3`；`.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `windows-package` | module、容器和 CI 统一固定 Go `1.26.5`。 | source → pinned toolchain → candidate artifacts |
| `pkg/slider/client.go:82`、`:160`、`:188`、`:203`、`:213` | 对外 Client 和完整生命周期已经实现。 | library caller → Solve/Prime/Purge → Close |
| `internal/challenge/solver.go:107`、`:149`、`:289` | 具体 Solver 已串接完整单轮链路。 | request → Device/Init/Vision/PE → one Verify |
| `internal/challenge/solver_test.go` · `TestSolverOfflineCompleteSuccess` / `TestSolverLowConfidenceStopsBeforeVerify` / `TestSolverVerifyNetworkErrorIsSingleAttemptAndSanitized` | 成功、前置失败和网络未知的 Verify 次数已有离线计数证据。 | Mock transport → state gates → counted attempt |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](docs/archive/evidence/validation-2026-08-07.md) | 32 路 Mock 正确性通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛。 | concurrent mock / explicit local sampler → verified compute gate |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165`；`internal/device/online_session_test.go:17` | Device schema 修复只在 Log1 解析对象，Log2/3 接受非对象成功结果；在线 Log1/2/3 约 `0.53s` 通过。 | raw response → action-specific decode → offline regression → authorized probe |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](docs/archive/evidence/validation-2026-08-07.md) | 授权候选 200/32/应用层零重试批次成功 `196/200`，Client 完整链墙钟 P95 `984ms`，两项冻结门槛通过；该 harness 不经过 HTTP Handler，最终 one-shot 加固后未再在线重跑。 | authorized jobs → one Solve each → sanitized aggregate → bounded acceptance |
| `internal/pe/keys.go` · `NewKeyResolverWithCapacity`；`internal/pe/device_runtime.go` · `OpenDevice` / `Close` | 当前每轮立即冷建独立Device/V8，不设置本地live并发槽，结束时关闭会话。 | cold Open/Complete → Close |
| `cmd/server/main.go:36`、`:49`、`:55`、`:60`、`:93`、`:111`、`:147` | 可执行服务、启动/定时清理和优雅关闭已经接线。 | config → Client → HTTP → cleanup |
| `internal/server/server.go` · `checkSolveOrigin` / `decodeQueryRequest` / `requestFromPayload`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` / `TestBrowserOriginBoundaryPreservesLegacyClients` | 已废弃 GET query 的传输兼容、方法参数源隔离和 GET/POST 跨源边界已固定。 | method + path → origin + JSON/query gate → one Solver call |
| `Makefile` · `build-linux`；`.github/workflows/ali-slider-go-ci.yml` · `quality` | Linux AMD64 静态二进制已有本地和 CI 构建路径。 | commit → verified server binary |
| `internal/vision/solver_test.go:260`；`internal/challenge/performance_test.go:65` | `53.10ms/op` 是视觉均值；完整纯计算链 P99 由独立逐次采样得到 `57.05075ms`。 | benchmark mean / sorted samples → separate performance claims |
