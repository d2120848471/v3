# Python 到 Go 迁移指南

> **迁移状态**：Go 主链已落地。2026-08-08 的同机 A/B 发现早期迁移曾丢失两个关键动态边界：旧版在同一挑战内保持 FeiLin VM，并按当轮 `StaticPath` 执行当前 PE；迁移版改成纯 Go Device + 静态 key/近似 PE，最终持续 `F001`。当前实现用同进程 V8 `149.4.0` 恢复动态语义：同一 Device Isolate 跨 Open/Complete，每轮禁网 PE Isolate 执行当前脚本，只缓存公开源码/结构画像 5 分钟，生产不启动 Node。Linux AMD64/ARM64 native/ABI 和 Linux AMD64 公开 Device/PE 组件探针已通过；完整 Solve 成功率/性能尚未重测。旧 Python 源码可从 Git 历史提交 `0509bfd` 恢复，用户指定的动态语义对照提交为 `d92c7d1`。

## 目标与冻结边界

- Go 主进程负责编排、RPC、图片和纯 Go 视觉，不依赖 Python、Node 进程、浏览器、OpenCV、GoCV 或 CGo；通过 `purego` + Rust V8 wrapper 执行设备 SDK/FeiLin 与当前动态 PE。
- reusable Go library 与 HTTP 服务共用同一完整编排。
- HTTP Solve 请求先通过浏览器跨源边界和方法对应的 JSON/query 输入边界，再直接进入 `Solve`；无浏览器来源头的旧 curl/程序客户端保持兼容。应用内不设置在途请求闸门，也不因活动数返回 429。
- 每轮状态隔离；每个 `CertifyId` 最多尝试一次 Verify，网络结果未知也不重试。
- 普通 CI 完全使用 Mock/静态 fixture，不运行真实在线挑战。

明确排除：Python 交互式 CLI、桌面 UI、PyInstaller、vision worker、业务提交/重放和 Swagger UI。已废弃的 `GET /api/slider?...` 只是旧 Python query 的传输兼容层，不是新的求解链；`GET /` 第一方内嵌页仍只手工调用推荐的 POST JSON，它不是桌面 UI 或 Swagger。

## 源码映射

| Python 来源 | Go 目标 | 状态 | 说明 |
|---|---|---|---|
| `config.py`、`entrypoints/options.py` | `internal/config`、`cmd/server` | 已实现 | env/flag/default/validate，并接入 listener 和 Client |
| `device_profile.py` | `internal/device/profile.go` | 已实现 | 画像、浏览器头和可注入随机源 |
| `runtime/device.py` 指纹 | `internal/device/fingerprint.go` | 已实现 | session probe、canvas/GPU/brand、交互事件 |
| `runtime/device.py` RPC/session | `internal/device/session.go`、`rpc.go` | 已实现并在线验证 | Log1/2/3、按 action 解码响应 schema、Init/Verify token、getter/event、完成态与清理 |
| `protocol/signing.py` | `internal/protocol/encoding.go`、`signing.go` | 已实现 | JS/RPC 编码、canonical query、HMAC 签名 |
| `protocol/device_token.py` | `internal/protocol/crypto.go`、`token.go` | 已实现 | AES-CBC、DeviceToken 构造/解析和自检 |
| `protocol/data_codec.py` | `internal/protocol/data.go` | 已实现 | transform、压缩、schema、data Pack/Unpack |
| `protocol/params.py` | `internal/protocol/params.go` | 已实现 | Verify 参数及保留的纯业务参数函数 |
| `protocol/secrets.py` | `internal/protocol/secrets.go` | 已实现 | 前端材料解码及脱敏表示 |
| `challenge/transport.py` | `internal/challenge/transport.go`、`pool.go`、`socks.go` | 已实现 | HTTP(S)/SOCKS、连接池和 route 隔离 |
| `challenge/assets.py` | `internal/challenge/assets.go` | 已实现 | 固定 CDN、双图并发、内存下载和边界 |
| `challenge/track.py` | `internal/track/track.go` | 已实现 | embedded fixture、缩放、抽稀和有界扰动 |
| `vision/gap_solver.py`、`geometry.py` | `internal/vision` | 已实现 | 纯 Go PNG、缺口定位、fallback 和几何 |
| `runtime/pe.py`、历史 `runtime/node_pe.py` | `internal/pe`、`internal/v8runtime`、`native/v8runtime` | 已实现 | 同挑战持久 V8 Device Isolate、逐轮动态 PE Isolate、getter/event/data 自检；公开 SDK/PE 源码和画像按精确 `StaticPath` 缓存 5 分钟 |
| `challenge/session.py` | `internal/challenge/rpc.go`、`solver.go` | 已实现 | Init → Assets → Vision → PE/Device → 唯一 Verify |
| `challenge/device_pool.py` | `internal/challenge/device_pool.go` | 已实现 | 有界 Prime/Lease、完整 key 隔离、过期和补货 |
| `entrypoints/api.py` | `internal/server`、`cmd/server` | 已实现 | HTTP、内嵌测试页、OpenAPI、直接分派 Solve、日志、启动与优雅关闭 |
| `errors.py` | `pkg/slider.Error`、`challenge.Failure` | 已实现 | 稳定错误类别、阶段和脱敏消息 |
| Python 对外 client | `pkg/slider.Client` | 已实现 | `NewClient`、`Solve`、`Prime`、`PurgeArtifacts`、`Close` |
| `challenge/business.py` | 无端到端目标 | 排除 | 1.0.0 不提交或重放业务请求 |

`protocol.BuildBusiness*` 仍作为纯函数和 oracle 兼容性测试存在；这不表示业务提交链路被纳入运行范围。

## 完整 Go 数据流

```text
HTTP / library Request
  → route-isolated Transport
  → V8 Device Isolate（预热 Lease 或冷建 Log1/2/3）
  → InitCaptchaV3
  → KeyResolver（公开 SDK/精确 PE 源码与画像，5 分钟缓存）
  → 双图并发下载
  → vision.Solve + 置信度门禁
  → track.LoadDefault + 当前禁网 V8 PE Isolate + Go 独立复核
  → 同一 Device Isolate Complete + Verify token
  → VerifyCaptchaV3（最多一次）
  → slider.Result / 脱敏分类错误
```

低置信、下载、PE、设备完成态或取消失败均在 Verify 前停止。`RPCClient` 在网络调用前消耗唯一 Verify 尝试位，网络结果未知也不会重发。

## HTTP 兼容合同

### 请求

| 行为 | Go 合同 |
|---|---|
| 浏览器测试入口 | `GET /`；只自动检查 health，手工提交现有 POST，不自动重试 |
| 推荐解题入口 | `POST /api/slider` + JSON object |
| 旧 Python 兼容入口 | `GET /api/slider?...`；OpenAPI 标记 `deprecated`，但仍会发起真实求解 |
| 参数源 | GET 只读 query，POST 只读 JSON body，两者不合并；不支持 form body |
| Scene | `SceneId`，兼容 `sceneId`；规范字段优先，即使它为空值 |
| prefix | `prefix`，兼容 `Prefix`；1–32 ASCII 字母数字 |
| RPC key | `AaduaneId`，兼容 `aaduaneId` |
| proxy | `proxy`，兼容 `Proxy`；无 scheme 时补 `http://` |
| 重复/空 query | 同名键取最后一个值；规范名压过别名；空 `SceneId/prefix` 使用默认值 |
| 输入大小 | JSON body 和 raw query 分别最大 65,536 字节 |
| 未知字段 | JSON 或 query 中的未知键都忽略，保持前向兼容 |
| 非法 query encoding | 返回 `400 ApiRequestError`，不调用 Solver |
| 浏览器跨源 GET/POST | 返回 `403 ApiOriginError`，不调用 Solver；无浏览器来源头客户端允许 |

### 响应

完成态响应保留：`ok`、`securityToken`、`VerifyCode`、`VerifyResult`、`certifyId`、`sceneId`、`proxied`、`elapsedMs`、`timingsMs`、`traceId`。

- Verify 业务未通过仍返回 HTTP `200`；调用方不能只判断状态码。
- 入参错误返回 400，明确浏览器跨源请求返回 403，技术失败返回 500；通过 origin 和输入边界的请求不经过本地并发接纳闸门，直接调用 `Solve`。
- `/api/slider` 响应包含 `X-Trace-ID`；全部响应使用 `Cache-Control: no-store`。
- Handler 不生成本地 429、`Retry-After`、`TooManyChallenges`、活动数或退避字段。外层网关仍可按自身策略返回 429；上游 429 属于上游非 2xx，进入 Solver 的稳定网络错误分类，不作为本地过载响应透传。

`MaxConcurrency` / `--max-concurrency` 是兼容旧名，只作为 Client 每 route/host 的出站连接与设备预热资源预算；它不限制 HTTP 在途请求数，也不决定 Handler 是否接收请求。

当前 HTTP 直达合同已通过 HTTP Mock 和并发 Handler 回归验证。Device Log1/2/3 在线探针与 2026-08-07 的 `Client.Solve` 在线批次分别证明设备链和 Client 链；两者都不能替代 HTTP 传输端到端测量。

## 有意差异

| 差异 | 原因 | 调用方影响 |
|---|---|---|
| 默认 `127.0.0.1:8000` | 应用内无鉴权，缩小暴露面 | 外部访问必须经受控网络/反向代理 |
| 恢复已废弃的 GET query；增加第一方内嵌测试页 | 保留旧 Python 传输兼容；测试页仍只使用 POST，不是 Swagger UI | 旧 GET 调用方可继续工作，新接入应使用 POST JSON |
| Go 标准库编排 + 进程内 V8 Isolate | 消除 Python/OpenCV/浏览器/Node 子进程，同时保留上游动态设备/PE 语义 | Docker/Windows 携带同架构 `.so`/DLL；裸部署需同目录 wrapper 或 `--v8-library` |
| 图片默认在内存 | 降低临时文件竞争和泄漏 | 仅失败/低置信由 artifact 策略落盘 |
| 更严格的 PNG/重定向/大小边界 | 防 SSRF、压缩炸弹和资源耗尽 | 异常旧输入更早失败 |
| prefix 仅 ASCII 字母数字 | 与协议域名边界一致 | 标点/Unicode prefix 被拒绝 |
| 普通日志强制脱敏 | 防止敏感材料落盘 | 用 traceId、稳定阶段和 artifact 排障 |
| proxy route 隔离、哈希 key、LRU proxy 淘汰 | 防跨出口复用、凭据明文驻留 key 和无界池 | proxy churn 会关闭最久未使用的 proxy route；直连不淘汰 |
| 设备预热只命中完整 key | 禁止跨代理、画像和时序复用 mutable session | 逐请求代理通常冷建 |
| 未知 JSON 字段忽略 | 保持前向兼容 | 已知字段类型错误仍为 400 |
| 无本地并发接纳闸门 | 合法请求必须直达 `Solve`，`MaxConcurrency` 仅保留为 Client 资源预算 | 外层必须落实鉴权以及按身份/IP 的频率、并发和总量限制 |

应用内既不实现鉴权，也不提供活动请求数保护；这是冻结范围内的已接受风险，不代表公网无鉴权部署安全。

旧 GET 的副作用和 URL 泄露面是为兼容而保留的风险：不要把 `AaduaneId` 或含 username/password 的代理放入 query，也不要将该 GET 当作可预取、可重试或无副作用的读取。

## Oracle 与回归数据

### 协议

`internal/protocol/testdata/python_oracle.json` 固定 Python 生成的编码、签名、AES、Token、data 和参数。Go 测试读取 fixture，不启动 Python。

### PE

`internal/pe/builder_test.go` 保留 Python data oracle；旧 Node bridge 只作可选 oracle/上下文差异对照。`v8_bundle_test.go`、`v8_context_diff_test.go`、`internal/v8runtime/runtime_test.go` 锁定 bundle、Node 24 上下文兼容、C ABI、host callback、取消、heap/时间上限和 Isolate 生命周期；`device_runtime_test.go` 锁定同 Isolate 两阶段、token/session/action、HTTP/SOCKS 路由和关闭生命周期。`keys_test.go` 另外锁定精确路径缓存、5 分钟 TTL、24 路并发 miss 合并和白名单/体积边界。生产 V8 显式在线组件探针不创建 Captcha Init/Verify；完整验收必须另行明确授权。

### 视觉

`internal/vision/testdata/python-edge-decoy/` 保存正负 PNG/JSON：

- `gap`：`x=241±1`、confidence `>=0.45`、包含 `global-chamfer`。
- `no-gap`：不得高置信接受，也不得包含 `global-chamfer`。

两个子用例当前均 PASS。新的线上失败图只有在授权、脱敏且确认不含 token/CertifyId 后才可进入仓库。

## 构建与部署产物

### Linux 双架构产物

```bash
make build-linux-amd64
make build-linux-arm64
file dist/linux-amd64/ali-slider-go
file dist/linux-amd64/libali_slider_v8_runtime.so
```

完整产物为 `CGO_ENABLED=0` Go launcher + 同架构 V8 `.so` + `THIRD-PARTY-NOTICES.txt`。`purego` 在 Linux 上使用 `libdl.so.2`，所以运行环境需 glibc；不能写成单文件、静态 ELF 或 `scratch` 部署。

### 容器

`Dockerfile` 使用 Go `1.26.5` 构建层、Rust `1.88.0` V8 构建层和 Debian bookworm-slim 运行层，服务以非 root UID/GID `65532` 运行。启动时在 ready 前加载 `.so`、校验 C ABI 并初始化 V8/ICU。容器默认仍监听回环；需要端口映射时显式设置容器内 `0.0.0.0`，并优先只发布到宿主回环。

### 服务生命周期

`cmd/server` 已接线：

- 解析并验证配置；
- 创建 `slider.Client`；
- 在 ready 前校验 wrapper、C ABI 与 V8/ICU；
- 启动前清理过期 artifact；
- 按配置并行 Prime 设备会话；
- 注入完整 Client 到 HTTP Handler；
- 每小时再次执行 artifact 清理；
- 捕获 SIGINT/SIGTERM，HTTP 优雅关闭后等待 Client 活跃任务并释放池。

## 分阶段切换

### 阶段 1：离线等价——已完成

协议、PE、edge-decoy、设备、Captcha、完整 Solver 和 HTTP 使用静态 fixture/Mock 验证；全量 Go 门禁及 Linux AMD64/ARM64 V8 native/ABI 构建已通过。

### 阶段 2：运维预演——已具备，发布前复核

当前覆盖率快照为 `80.4%`，高于 `>=80%` 门槛，`internal/v8runtime` 为 `91.4%`；当前迁移候选已通过 Linux AMD64/ARM64 Rust native 和 Go→V8 ABI 测试、Windows AMD64 DLL 交叉构建/导出依赖检查、同架构 Debian/glibc 包检查、Docker 镜像构建及非 root `/health` smoke。发布 commit 仍应由 CI 重跑覆盖率和全部门禁，尤其是 Windows 原生 DLL 执行/最终 ZIP smoke。

### 阶段 3：授权性能与在线验收——组件通过，完整 Solve 需重测

当前生产 V8 `OpenDevice → Resolve → Build → Complete` 组件探针在 Linux AMD64 最终包中以 `2.78s` 通过，但不创建 Captcha Init/Verify。Mac ARM64 纯 Go 200 样本 `P99=57.05075ms`、2026-08-07 `196/200`/完整链 `P95=984ms` 及 2026-08-08 单挑战 `T001` 都属于旧运行时路径。当前 V8 架构必须在新的明确授权下重做完整成功率、性能与容量报告；HTTP 端到端也尚未单独测量。

### 阶段 4：小流量切换——待验收通过

外部路由只把新请求按固定比例送往 Go。监控成功率、P95/P99、外层网关/上游 429、错误分类、预热命中、内存、goroutine 和 artifact 磁盘。

### 阶段 5：默认切换——待前序门槛通过

只有在线、性能、安全和运维证据全部闭环，才把 Go 设为默认。旧 Python 回滚基线保留在 Git 历史提交 `0509bfd`，当前工作树不维持 Python/Go 双栈。

## 回滚策略

- 通过反向代理或服务发现切换新请求，不共享 Python/Go 的 in-flight 状态。
- 回滚只影响新挑战；已经进入 Verify 的 Go 请求不得交给 Python 重试。
- 保留上一个已验证 Go launcher + 匹配 V8 wrapper、镜像 digest、配置快照和 OpenAPI 快照；敏感配置不入库。
- 触发条件至少包含成功率、P95/P99、500、外层网关/上游 429、RSS、goroutine、预热失败、artifact 磁盘和脱敏告警。
- 旧 Python 源码不在当前工作树；需要 oracle/回滚时从 Git 历史提交 `0509bfd` 临时恢复，不维持长期双栈。

## 切换前清单

- [x] 公共 `slider.Client` 与完整 `challenge.Solver` 已接通全部阶段。
- [x] `cmd/server`、优雅关闭、预热池、连接池和 artifact 定时清理已接线。
- [x] HTTP/OpenAPI、唯一 Verify、错误脱敏和完整 Mock 链测试通过。
- [x] Go `1.26.5` 全量 test/race/vet/staticcheck/govulncheck 通过。
- [x] Linux AMD64/ARM64 V8 Rust native、Go→ABI 测试与双文件包构建通过。
- [x] Windows AMD64 V8 DLL 交叉构建、导出符号和系统 DLL 依赖检查通过。
- [ ] Windows AMD64 原生 Rust/Go→DLL 测试与最终 ZIP smoke 在发布 commit CI 上完成。
- [x] Go CI 工作流和覆盖率门槛已建立。
- [ ] 最终发布 commit 的统一覆盖率报告已归档。
- [x] 纯计算 P99 有可复现报告：200 样本 `P99=57.05075ms`。
- [x] 当前生产 V8 Device/PE 公开组件探针通过；它不创建 Captcha Init/Verify。
- [ ] 当前逐挑战 V8 Device/PE 架构按新授权完成完整 Solve 成功率、P95/P99 和资源报告；2026-08-07 历史 `196/200`、P95 `984ms` 不作为当前结果。
- [ ] 实际机器资源下的 RSS、GC、goroutine 峰值和长时间稳定性压测有可复现报告。
- [ ] 生产外层鉴权、网络、日志、磁盘、回滚与值班流程通过评审。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `pkg/slider/client.go:82`、`:160`、`:188`、`:203`、`:213` | 公共 Client 已实现创建、Solve、Prime、Purge 和并发安全 Close。 | library request → complete solver → result/lifecycle |
| `internal/challenge/solver.go:107`、`:149`、`:289` | 具体 Solver 已串接 Device、Init、Assets、Vision、PE、Device Complete 和 Verify。 | request → one complete Go challenge |
| `internal/challenge/solver_test.go` · `TestSolverOfflineCompleteSuccess` / `TestSolverLowConfidenceStopsBeforeVerify` / `TestSolverVerifyNetworkErrorIsSingleAttemptAndSanitized` | 完整 Mock 链证明成功一次 Verify、低置信零 Verify、网络错误一次尝试。 | fixture transport → state gates → counted Verify |
| `internal/challenge/device_pool.go:22`、`:134`、`:190`、`:299` | 预热池按完整 key 隔离，并提供有界 Prime/Lease/冷建/过期/补货。 | Prime → key match → lease or cold open |
| `cmd/server/main.go:36`、`:49`、`:55`、`:60`、`:93`、`:111`、`:147` | 启动器已接线配置、预热、启动/定时清理和优雅关闭。 | config → Client → HTTP → lifecycle cleanup |
| `internal/server/server.go` · `checkSolveOrigin` / `decodeRequest` / `decodeQueryRequest` / `requestFromPayload` / `handleSolve`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` / `TestBrowserOriginBoundaryPreservesLegacyClients` / `TestConcurrentRequestsAlwaysEnterSolver` | 旧 GET query 的字段、重复键、默认值、64 KiB、非法 encoding 和跨源边界均有回归；通过校验的 GET/POST 直接调用 Solver，没有本地 429。 | method → origin + JSON/query gate → timeout context → Solve exactly once |
| `go.mod`；`native/v8runtime/Cargo.toml`；`Makefile` · `build-linux-amd64` / `build-linux-arm64`；`Dockerfile` | Go `1.26.5` launcher、Rust 1.88/V8 149.4 wrapper 与 Debian/glibc 组成完整容器部署候选。 | Go + Rust source → native/ABI tests → launcher + wrapper → image |
| `.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `linux-runtime` / `windows-package` | Linux AMD64/ARM64 用真实 `.so`，Windows 用真实 DLL，组成跨平台发布门禁。 | change → split-platform Go/Rust/V8 evidence → release gate |
| `internal/vision/solver_test.go:17`；`internal/pe/v8_*_test.go`；`internal/v8runtime/runtime_test.go` | Python/Node oracle 只作静态迁移对照；生产主链在受控 V8 Isolate 执行当前脚本并由 Go 复核。 | fixture + exact StaticPath + challenge input → embedded V8 → Go assertions |
| `internal/pe/v8_runtime_online_test.go` | 生产 `KeyResolver` 在同一 V8 Device Isolate 跨 Open/Complete，并以禁网 PE Isolate 构造 data；完成态 142 字段、4 个请求且不调用 Captcha Init/Verify。 | public SDK/PE + Device RPC → persistent Device Isolate + per-call PE Isolate → validated completion |
| `internal/challenge/performance_test.go:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 200 样本纯计算 `P99=57.05075ms`，满足硬门槛。 | fixture → Vision/Track/PE → nearest-rank P99 |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的历史 Client harness 以 200/32/应用层零重试运行，严格成功 `196/200`、完整链 P95 `984ms`；它不经过 HTTP Handler，不证明当前 HTTP 限流或端到端延迟，最终 one-shot 加固后也未在线重跑。 | dated authorization → one Client.Solve per job → sanitized aggregate → bounded historical finding |
