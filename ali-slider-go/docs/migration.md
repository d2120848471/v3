# Python 到 Go 迁移指南

> **迁移状态**：纯 Go 代码链已经完整落地。Go `1.26.5` 离线门禁、Linux AMD64 静态构建、Docker 非 root `/health` smoke、Device Log1/2/3 在线探针和 Mac ARM64 纯计算 P99 均已通过；统一覆盖率重复运行为 `83.1–83.2%`，稳定高于 `>=80%` 门槛。2026-08-07 的历史授权候选批次恰好 200 轮、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`，Client 完整求解链墙钟 `P95=984ms`，达到成功率与 P95 两项冻结门槛。该批是未经过 HTTP Handler 的 Client harness 历史证据，不构成当前 HTTP 并发或总量限制；它先于传输 one-shot 加固，最终源码未获授权再跑第二批。旧 Python 源码已从当前工作树删除，需要审计或回滚时从 Git 历史提交 `0509bfd` 恢复；默认流量切换仍需生产安全、运维和长时间资源稳定性评审。

## 目标与冻结边界

- 单一纯 Go 进程完成一轮挑战，不依赖 Python、Node.js、浏览器、OpenCV、GoCV、CGo、动态库或子进程。
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
| `runtime/pe.py` | `internal/pe` | 已实现 | 轨迹字段、坐标、逻辑时钟、getter plan、data 自检 |
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
  → Device Session（预热 Lease 或冷建 Log1/2/3）
  → InitCaptchaV3
  → 双图并发下载
  → vision.Solve + 置信度门禁
  → track.LoadDefault + pe.Builder
  → Device Complete + Verify token
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
| 纯 Go 标准库 | 消除 Python/OpenCV/浏览器 worker | 不再安装 wheels、Node 或动态库 |
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

`internal/pe/builder_test.go` 解包 Python data，对照 payload JSON、arg、坐标、TrackList、逻辑时钟和 getter 元数据；允许 zlib 实现产生不同压缩字节，但解包语义必须完全一致。

### 视觉

`internal/vision/testdata/python-edge-decoy/` 保存正负 PNG/JSON：

- `gap`：`x=241±1`、confidence `>=0.45`、包含 `global-chamfer`。
- `no-gap`：不得高置信接受，也不得包含 `global-chamfer`。

两个子用例当前均 PASS。新的线上失败图只有在授权、脱敏且确认不含 token/CertifyId 后才可进入仓库。

## 构建与部署产物

### 静态二进制

```bash
make build-linux
file dist/ali-slider-go-linux-amd64
```

Go `1.26.5`、`CGO_ENABLED=0`、Linux AMD64 静态构建已验证通过。

### 容器

`Dockerfile` 使用 Go `1.26.5` Alpine 构建层、`scratch` 运行层、非 root UID/GID `65532` 和静态二进制。Linux AMD64 镜像构建和关闭预热后的非 root `/health` smoke 已通过。容器默认仍监听回环；需要端口映射时显式设置容器内 `0.0.0.0`，并优先只发布到宿主回环。

### 服务生命周期

`cmd/server` 已接线：

- 解析并验证配置；
- 创建 `slider.Client`；
- 启动前清理过期 artifact；
- 按配置并行 Prime 设备会话；
- 注入完整 Client 到 HTTP Handler；
- 每小时再次执行 artifact 清理；
- 捕获 SIGINT/SIGTERM，HTTP 优雅关闭后等待 Client 活跃任务并释放池。

## 分阶段切换

### 阶段 1：离线等价——已完成

协议、PE、edge-decoy、设备、Captcha、完整 Solver 和 HTTP 使用静态 fixture/Mock 验证；全量 test/race/vet/staticcheck/govulncheck 和静态构建已通过。

### 阶段 2：运维预演——已具备，发布前复核

当前源码快照的统一覆盖率重复运行为 `83.1–83.2%`，稳定高于 `>=80%` 门槛；Linux AMD64 静态二进制、Docker 镜像构建及非 root `/health` smoke 已通过。发布 commit 仍应由 CI 重跑覆盖率和门禁，并复核启动、预热降级、定时清理、信号关闭、磁盘权限和日志脱敏。

### 阶段 3：授权性能与在线验收——2026-08-07 历史门槛已完成

Mac ARM64 纯计算 200 样本 `P50=50.577458ms`、`P95=55.711708ms`、`P99=57.05075ms`、`max=60.337542ms`，满足 `P99<=100ms`。2026-08-07 的历史授权 Client harness 恰好执行 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试：严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`；Client 完整求解链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，总批次约 `6.16s`。成功率和 Client 完整链 P95 达到当时门槛；P99 不属于 1 秒目标，HTTP 端到端尚未单独测量，200/32 也不构成当前 Handler 限制。该批先于最终 one-shot 加固；完整时序边界见 [脱敏验证证据](./evidence/validation-2026-08-07.md)。

### 阶段 4：小流量切换——待验收通过

外部路由只把新请求按固定比例送往 Go。监控成功率、P95/P99、外层网关/上游 429、错误分类、预热命中、内存、goroutine 和 artifact 磁盘。

### 阶段 5：默认切换——待前序门槛通过

只有在线、性能、安全和运维证据全部闭环，才把 Go 设为默认。旧 Python 回滚基线保留在 Git 历史提交 `0509bfd`，当前工作树不维持 Python/Go 双栈。

## 回滚策略

- 通过反向代理或服务发现切换新请求，不共享 Python/Go 的 in-flight 状态。
- 回滚只影响新挑战；已经进入 Verify 的 Go 请求不得交给 Python 重试。
- 保留上一个已验证 Go 静态二进制、镜像 digest、配置快照和 OpenAPI 快照；敏感配置不入库。
- 触发条件至少包含成功率、P95/P99、500、外层网关/上游 429、RSS、goroutine、预热失败、artifact 磁盘和脱敏告警。
- 旧 Python 源码不在当前工作树；需要 oracle/回滚时从 Git 历史提交 `0509bfd` 临时恢复，不维持长期双栈。

## 切换前清单

- [x] 公共 `slider.Client` 与完整 `challenge.Solver` 已接通全部阶段。
- [x] `cmd/server`、优雅关闭、预热池、连接池和 artifact 定时清理已接线。
- [x] HTTP/OpenAPI、唯一 Verify、错误脱敏和完整 Mock 链测试通过。
- [x] Go `1.26.5` 全量 test/race/vet/staticcheck/govulncheck 通过。
- [x] Linux AMD64 静态二进制构建通过。
- [x] Go CI 工作流和覆盖率门槛已建立。
- [ ] 最终发布 commit 的统一覆盖率报告已归档。
- [x] 纯计算 P99 有可复现报告：200 样本 `P99=57.05075ms`。
- [x] 2026-08-07 历史授权 Client harness 恰好 200 轮、并发 32、每 job 一次 Solve、应用层零重试，严格成功 `196/200`，Client 完整求解链墙钟 `P95=984ms`；最终 one-shot 加固后未在线重跑。
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
| `go.mod:3`；`Makefile` · `build-linux`；`Dockerfile:3`、`:19` | Go `1.26.5`、静态二进制和 scratch 镜像均已有生产构建路径。 | source → binary/image → deployment candidate |
| `.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `windows-package` | Linux quality、Darwin race 和 Windows package job 都在 `CGO_ENABLED=0` 下建立纯 Go 质量与发布门禁。 | change → split-platform CI evidence → release gate |
| `internal/vision/solver_test.go:17`；`internal/pe/builder_test.go:96` | Python oracle 仅作为静态迁移证据，Go 测试/runtime 不依赖 Python。 | fixture → Go assertion → pure Go runtime |
| `internal/device/rpc.go:119`、`:130`；`internal/device/online_session_test.go:17` | Device 响应按 action 解码；在线 Log1/2/3 探针约 `0.53s` 通过。 | raw response → schema gate → authorized device probe |
| `internal/challenge/performance_test.go:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 200 样本纯计算 `P99=57.05075ms`，满足硬门槛。 | fixture → Vision/Track/PE → nearest-rank P99 |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的历史 Client harness 以 200/32/应用层零重试运行，严格成功 `196/200`、完整链 P95 `984ms`；它不经过 HTTP Handler，不证明当前 HTTP 限流或端到端延迟，最终 one-shot 加固后也未在线重跑。 | dated authorization → one Client.Solve per job → sanitized aggregate → bounded historical finding |
