# 排障手册

> **当前边界**：服务、完整 Solver、公共 Client、预热池和定时清理已经可运行。纯计算 `P99=57.05075ms`、2026-08-07 历史授权候选批次 `196/200` 和 Client 完整求解链墙钟 `P95=984ms` 已达到当时冻结门槛。1 秒只约束 P95；该批 `P99=1018ms`、`max=1555ms`，且是未经过 HTTP Handler 的 Client harness。该批先于最终传输 one-shot 加固，最终源码未获授权再跑第二批。离线 6,400 次 Solve 压力已通过；生产形态的长稳资源压测仍无结论。排障过程中不得擅自重跑或扩大在线流量，也不得重试同一 `CertifyId`。

## 快速分流

从 `ali-slider-go` 目录确认工具链和离线状态：

```bash
export CGO_ENABLED=0
go version
make fmt-check
make vet
make test
make race
```

`make race` 与完整 `make check` 的严格无 CGo race 只在 Darwin 可运行；Linux 使用 `make check-linux`，并由根 CI 的 macOS job补齐 race。Go 1.26 的 Linux race runtime 强制要求 CGo，不要临时放宽后把结果记为本项目纯 Go 门禁。

启动本地服务但关闭设备预热，避免仅做 health 冒烟时产生设备外部请求：

```bash
go run ./cmd/server \
  --host=127.0.0.1 \
  --port=8000 \
  --device-prewarm=0
```

另一个终端：

```bash
curl --fail --silent http://127.0.0.1:8000/health
```

处理问题时记录 commit、Go 版本、`X-Trace-ID`、HTTP 状态、客户端墙钟和脱敏后的错误类别/阶段。不要收集请求/响应正文、token、`CertifyId`、RPC key、代理密码或上游原始错误。

## 启动与生命周期

| 现象 | 直接证据 | 常见原因 | 处理 |
|---|---|---|---|
| `配置无效` | 进程退出并给出配置边界 | host/port、Client 连接/预热资源预算、timeout、confidence、artifact 或预热值越界 | 对照 [配置说明](./configuration.md) 修正 flag/env；不要绕过 Validate |
| `HTTP 服务退出: bind... address already in use` | 启动器退出 | 端口被占用 | `lsof -nP -iTCP:8000 -sTCP:LISTEN` 定位；换端口或按运维流程停止旧实例 |
| `event=device_prewarm status=warning` | 进程继续启动 | 设备 RPC 超时、部分 Prime 失败或配置/出口问题 | 请求会冷建兜底；检查网络和 device 日志阶段，不把 warning 当预热成功 |
| 服务 ready 前较慢 | 端口已绑定，但尚未出现 `event=listen status=ready` | 默认预热会在开始 `Serve` 前执行 Log1/2/3 | 开发 health 冒烟用 `--device-prewarm=0`；生产记录 Prime 耗时 |
| `event=artifact_purge status=warning` | 启动或每小时出现 | artifact 目录权限、磁盘或读取失败 | 检查目录、UID、配额；清理失败不应阻止服务，但必须告警 |
| 非回环绑定出现 security warning | `unauthenticated=true` | 服务监听 `0.0.0.0` 或非回环 IP | 确认外层鉴权、防火墙、TLS 和来源限制；否则改回回环 |
| SIGTERM 后关闭慢 | `event=shutdown status=starting` 后等待 | 活跃 Solve/Prime/Purge 尚未结束 | 等待总 timeout；检查上游取消传播和长尾阶段，不要强制重复 Verify |
| `/health` ready 但 solve 失败 | health 200、solve 4xx/5xx | health 只验证 HTTP handler，不探测全部上游 | 按 errorType/traceId 和阶段排查；不要把 health 当在线成功率证明 |

启动器会在创建 Client 后立即清理一次 artifact，随后每小时清理，并在 SIGINT/SIGTERM 时执行 HTTP 优雅关闭和 `Client.Close`。

`MaxConcurrency` / `--max-concurrency` 只控制 Client 每 route/host 的出站连接与预热资源预算，不是 HTTP Handler 的并发闸门。服务没有本地活动请求数保护；外部部署必须由网关落实鉴权以及按身份/IP 的频率、并发和总量限制。

## HTTP 排障矩阵

| 现象 | 含义 | 处理 |
|---|---|---|
| `GET /api/slider` 返回 404 | 合同只允许 POST solve | 改为 `POST`；不要恢复 GET 副作用接口 |
| `400 ApiRequestError` | JSON、类型、长度、prefix 或 proxy 在 Solver 前失败 | 从 `{}` 开始逐项加入字段；请求体不超过 64 KiB |
| `400 InvalidRequest` | 公共 Client/Solver 认为请求不合法 | 检查 SceneId、prefix、RPC key 和 proxy 长度 |
| 外层网关直接返回 429 | 网关的身份/IP 频率、并发或总量策略生效 | 按该网关合同处理；不要把它归因于应用内活动数闸门 |
| `500 NetworkError` 且上游记录为 429 | 上游把本轮外部请求作为非 2xx 拒绝 | 用 traceId 与上游受控指标定位；Handler 不透传上游 429 或退避头，同一 `CertifyId` 不得重试 Verify |
| HTTP 200 但 `ok=false` | 协议完成，Verify 业务拒绝 | 读取 `VerifyCode`、`VerifyResult`；不要当技术成功或自动重试 |
| `500 NetworkError` | 设备、Init、下载、Complete 或 Verify 网络阶段失败 | 用 traceId 与受控指标定位；同一 CertifyId 不得重试 Verify |
| `500 VisionError` | PNG/识别失败或低置信 | 检查脱敏 artifact、置信度、坐标和 fixture 回归 |
| `500 ProtocolError` | PE、getter、token 或上游 schema 合同失败 | 运行 protocol/PE/device/challenge 测试；禁止输出敏感原文 |
| `500 UnhandledProtocolError` | 未分类 error 或 panic | 视为缺陷；用本地 Mock 最小复现并映射为稳定脱敏错误 |
| HTTP 无普通日志 | 自定义宿主给 `server.New` 的 Logger 为 nil | cmd/server 已注入 stdout logger；library 宿主应显式注入安全 Logger |

所有应用生成的 HTTP JSON 响应应有 `Cache-Control: no-store`；`/api/slider` 的 200/400/500 应包含与 body 一致的 `X-Trace-ID`。Handler 不生成本地 429、`Retry-After`、`TooManyChallenges`、活动数或退避字段；外层网关响应遵循网关自己的合同。

## Solver 阶段定位

library 错误的 `Stage` 和成功结果 `timingsMs` 使用稳定阶段名：

| 阶段 | 常见问题 | 检查 |
|---|---|---|
| `setup` | proxy route、画像或 client 构造 | proxy 格式、route 容量、prefix |
| `deviceSession` | 预热 Lease 或冷建 Log1/2/3 | pool key、会话年龄、设备 endpoint/时钟 |
| `init` | InitCaptchaV3 | 签名、SceneId/prefix、出口 |
| `downloadAssets` | CDN/重定向/大小 | 相对路径、HTTPS allowlist、Content-Length |
| `vision` | PNG 或低置信 | alpha、尺寸、候选、edge-decoy |
| `buildVerifyData` | 轨迹/PE/逻辑时钟 | Track、ExpectedXPos、Pack/Unpack |
| `completeDevice` | getter/event 或最终 Log2 | Session 状态、event、token 刷新 |
| `verify` | 唯一 Verify 请求/响应 | 网络未知也不得重试同一挑战 |
| `clientCleanup` | release/关闭/补货 | pool Lease 所有权、Close 竞态 |

HTTP 错误响应不会回显底层 cause。只有受控 library 调用方可以读取脱敏 `slider.Error.Stage`；不要为排障把完整 error chain 或 Result 写入公共日志。

## Proxy 与网络

| 现象/错误 | 原因 | 处理 |
|---|---|---|
| proxy scheme 被拒绝 | 使用 ftp 或未支持协议 | 仅使用 `http/https/socks4/socks5/socks5h` |
| proxy 含 path/query/fragment | URL 不是纯代理 origin | 使用 `scheme://[userinfo@]host:port`，不要打印 userinfo |
| SOCKS4 失败 | password 不支持或目标只有 IPv6 | SOCKS4 只用可选 username；需要远端 DNS 用 socks5h |
| SOCKS5 认证失败 | user/password 缺失、越界或代理拒绝 | 修正凭据；日志与工单不得包含 password |
| socks5 与 socks5h 结果不同 | socks5 本地 DNS，socks5h 代理侧 DNS | 按出口要求选择，不混淆报告 |
| `transport route capacity exceeded` | 容量已满且没有可淘汰的 proxy route，常见于容量 1 被不可淘汰的直连占用 | 合理配置 route 容量；普通 proxy churn 会 LRU 淘汰最旧 proxy，不会淘汰直连 |
| 直连受环境代理影响 | 宿主或自定义 Transport 改写 | 项目直连显式 `Proxy:nil`；确认没有替换生产 Transport |
| 外部阶段慢 | DNS/TLS/代理/CDN/上游长尾 | 比较阶段耗时；不要只放大总 timeout |

同一轮设备、Init、图片和 Verify 使用同一 transport。不同 route 不共享 TCP/TLS 连接。

## 设备预热池

| 现象 | 原因 | 处理 |
|---|---|---|
| 已 Prime 但请求仍冷建 | pool key 与请求不完全一致 | 检查 prefix、route、profile ID、timeout 和设备时序范围 |
| 使用逐请求 proxy 时未命中直连池 | route 不同，按设计隔离 | 为安全保留冷建；不得跨 proxy 复用会话 |
| 约 20 秒后首次请求变慢 | idle 会话超过默认最大年龄 | 记录过期/冷建；通过 library 配置评估合理年龄，不无限延长 |
| 消费后后台出现设备请求 | release 后有界异步补货 | 正常；监控补货失败，`ready+pending+leased` 不超过容量 |
| Prime 部分失败 | opener/网络失败 | 成功库存保留；可在授权环境再次 Prime，其他请求冷建 |
| Close 与 Lease 并发 | 关停过程取消任务并清理 ownership | 应由 `Client.Close` 统一关闭；不要绕过 release |

`Client.Prime` 会发送真实设备 Log1/2/3。只有授权环境才可调用；纯本地 health 冒烟设置预热容量为 0。

Device RPC 的成功响应 schema 按 action 处理：Log1 必须给出对象形式的 `ResultObject.DeviceConfig`；Log2/Log3 只要求成功 Code，`ResultObject` 可以是非对象。若 Log1 通过而 Log2/Log3 报 `invalid ResultObject`，优先确认没有回退到“所有 action 共用对象 schema”的旧实现。当前独立在线 Log1/2/3 探针约 `0.53s` 通过。

## 图片、视觉、PE 与 Verify

| 现象/错误 | 原因 | 处理 |
|---|---|---|
| unsafe relative asset path | 绝对路径、`..` 或非法字符 | 拒绝挑战；不要放宽为任意 URL |
| redirect left HTTPS allowlist | 重定向离开固定 CDN 或含 userinfo | 拒绝；检查代理/上游 |
| asset exceeds byte limit | 响应超过单图边界 | 核实样本；安全评审后才调整配置 |
| invalid PNG / dimensions | 内容损坏、非 PNG、边长或像素过大 | 检查 CDN/代理；保持资源门禁 |
| shadow 无 alpha/空形状 | 拼图图错误 | 不 Verify；纳入授权负例回归 |
| 两图尺寸不匹配 | 跨请求错配或缓存污染 | 检查单轮状态隔离；禁止复用 mutable 图片 |
| 低置信 | 无缺口、诱饵或算法不确定 | 不 Verify；查看脱敏 artifact，先复现 oracle |
| PE xPos mismatch | 轨迹换算与视觉坐标相差超过 1 px | 检查尺寸、最后 touchmove 和坐标；不 Verify |
| invalid PE logical clock | Init、first-touch、轨迹和 now 不一致 | 核对毫秒单位/主机时钟；不要绕过新鲜度 |
| PE self-check mismatch | Pack/Unpack 或 schema 内部回归 | 停止并运行 protocol/PE oracle |
| Verify already attempted | 同一 RPCClient 已尝试 Verify | 创建新挑战；禁止对原 CertifyId 再发 |

离线复核：

```bash
go test -count=1 -race \
  ./internal/device \
  ./internal/pe \
  ./internal/challenge \
  ./internal/vision
```

## Artifact 与定时清理

| 现象 | 原因 | 处理 |
|---|---|---|
| 成功请求没有 artifact | 成功路径按设计不落图 | 正常，不要为调试保存成功 token/正文 |
| 低置信/业务拒绝没有完整三件套 | 图片尚未取得、目录权限或 SaveFailure best-effort 失败 | 检查阶段和目录；早期失败可能只有 metrics |
| 目录创建/写入失败 | UID、父目录、只读文件系统或磁盘配额 | 目录应可由服务 UID 写入，权限保持 `0700/0600` |
| 文件超过 7 天仍存在 | server 未持续运行、purge warning 或使用 library 未调度 | cmd/server 启动时和每小时清理；library 调用方需调 `PurgeArtifacts` |
| 清理数量小于预期 | 名称不受管、symlink/目录或尚未过期 | 这是保护行为；不要扩大删除范围 |
| 保存半组文件 | 中途写失败 | Store 回滚本组已创建文件；检查磁盘和权限 |

`Purge` 只删除符合受管命名的过期普通文件，不跟随符号链接。不要用递归删除命令代替它。

## 性能排障

| 现象 | 误区 | 正确路径 |
|---|---|---|
| 视觉均值约 `53.10ms/op`，纯计算 P99 为 `57.05075ms` | 混淆测量范围或把均值当分位数 | 分别记录视觉 benchmark 均值和 `Vision → Track → PE/Data` 逐次 P99 |
| 完整请求超过 1 秒 | 先改视觉或扩大 timeout | 用 `timingsMs` 分离 device/init/download/vision/PE/verify |
| 并发升高后时延、RSS、goroutine 或连接等待上升 | 误以为 Handler 会用本地 429 自动保护进程 | 用 `timingsMs`、RSS、GC、goroutine、连接池和上游指标定位瓶颈；在外层网关设置经容量验证的频率、并发和总量限制 |
| 2026-08-07 的 32 并发历史短批次和离线 RSS 筛查通过 | 把 6.16 秒 Client harness 或含 Go 驱动的 Mock maxRSS 外推成生产长稳容量 | 按实际机器资源另测生产 RSS、GC、goroutine 峰值、持续吞吐和长时间稳定性 |
| 热态与冷态差异大 | 忽略预热 key/年龄 | 分开报告 Prime、命中、过期、冷建和 proxy |
| 并行视觉快但在线慢 | 把算法吞吐当完整链 | 分别报告视觉、纯计算、Solver、HTTP 和在线 |

当前纯计算 `P99=57.05075ms`；2026-08-07 的历史授权 Client harness 严格成功 `196/200`，完整求解链墙钟 `P95=984ms`，达到当时两项冻结目标。该批 `P99=1018ms`，不得声称 P99 小于 1 秒、HTTP 端到端已测或 200/32 是当前 Handler 限制；也不得把它写成 one-shot 加固后最终源码的第二份在线报告。完整方法见[性能测试与容量口径](./performance.md)和[脱敏验证证据](./evidence/validation-2026-08-07.md)。

## 构建、CI 与 Docker

| 现象 | 原因 | 处理 |
|---|---|---|
| module 要求 Go 1.26.5 | 本机旧工具链且无法自动获取 | 安装 Go 1.26.5；CI 使用 `GOTOOLCHAIN=local` 和精确版本 |
| `staticcheck` 不在 PATH | 本机未安装组织工具 | 使用 CI 固定 `v0.7.0` 或按组织流程安装；项目验证记录已通过 |
| `govulncheck` 不在 PATH/查询失败 | 工具或漏洞库网络不可用 | 使用 CI 固定 `v1.1.4`；区分扫描基础设施失败与发现漏洞 |
| PR 没有 Go CI | 变更路径未命中或工作流未触发 | 检查 `.github/workflows/ali-slider-go-ci.yml` paths；可 workflow_dispatch |
| 覆盖率变化 | commit/命令/缓存不同 | 使用 `-count=1` 和单一 coverprofile，记录 commit；当前基线 `83.0%` |
| `go build ./...` 没有目标文件 | 该命令只验证包 | 使用 `make build-linux` 生成 `dist/ali-slider-go-linux-amd64` |
| Linux 二进制不是 static | 未设置 CGO 或构建参数改变 | 使用 Makefile/CI 命令，并用 `file` 断言 `statically linked` |
| 容器端口不可达 | 应用默认监听容器回环 | 容器内设 `ALI_SLIDER_HOST=0.0.0.0`，宿主只发布到 `127.0.0.1` |
| 无法在容器内 exec shell/curl | runtime 是 scratch | 从宿主做 health；不要为调试把生产镜像改成 root/full OS |
| artifact 重启后丢失 | 容器未挂持久卷 | 将 `/app/var/artifacts` 挂受控卷并保持 UID 65532 权限 |

已验证的离线门禁包括 Go `1.26.5` 全量 test/race/vet/staticcheck/govulncheck、Linux AMD64 静态构建及 Docker 非 root `/health` smoke。在线验收不属于 CI；2026-08-07 的历史 200 轮 Client 批次已经完成，普通排障不得自动重跑。

## 安全诊断包

允许收集：

- commit、Go/OS/arch、配置字段名和脱敏值；
- traceId、HTTP 状态、稳定错误类别/阶段和聚合耗时；
- test/race/staticcheck/govulncheck/build 的 PASS/FAIL 摘要；
- 经授权、已脱敏的图片和算法指标。

禁止收集：

- `securityToken`、DeviceToken、`CertifyId`、RPC secret；
- proxy username/password；
- 原始 request/response body、完整 URL query；
- 未经授权的真实挑战图片或批量在线流量。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `cmd/server/main.go:36`、`:49`、`:55`、`:60`、`:93`、`:111`、`:147` | 启动器已实现配置、Prime、启动/每小时清理和优雅关闭。 | config → Client → HTTP → lifecycle |
| `internal/server/server.go` · `handleSolve`；`internal/server/server_test.go` · `TestConcurrentRequestsAlwaysEnterSolver` | 合法 POST 经解码后均直接进入 Solver；Handler 不按活动数拒绝，也不生成本地 429 或退避字段。 | method/body → validation → timeout context → Solve or stable error |
| `internal/challenge/solver.go:107`、`:149`、`:289` | 完整 Solver 已按阶段执行并只在前置门禁后 Verify。 | setup → device/init/assets/vision/PE → Verify |
| `internal/challenge/solver_test.go:268`、`:286` | 低置信零 Verify、网络未知单次 Verify 有计数测试。 | failure branch → counted transport calls |
| `internal/challenge/device_pool.go:190`、`:299` | 预热池按完整 key Lease，Close 有界清理后台任务。 | Prime/Lease → release/refill → Close |
| `pkg/slider/client.go:203`、`:213` | library 暴露 Purge，并在 Close 前等待活跃 Solve/Prime/Purge。 | caller lifecycle → safe resource cleanup |
| `internal/artifact/store.go:121`；`cmd/server/main.go:147` | Store 只清理受管过期文件；服务已按小时调度。 | failure artifact → retention → scheduled Purge |
| `.github/workflows/ali-slider-go-ci.yml:32`、`:62`、`:70`、`:76`、`:94`、`:97`、`:107`、`:115`、`:131` | Linux quality 与 Darwin race 两个 CI job 均固定 `CGO_ENABLED=0`；静态 Linux 构建门禁已经存在。 | source change → split-platform gates → verified binary |
| `Dockerfile:19`、`:26` | 运行镜像为 scratch、非 root，因此无 shell且需要正确卷权限。 | static binary → minimal runtime |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165` | Device schema 只在 Log1 解码对象，Log2/3 接受非对象成功结果。 | raw response → action gate → stable session |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的历史 Client harness 以 200/32/应用层零重试运行并达到当时成功率与完整链 P95 门槛；不经过 HTTP Handler，不能作为当前 HTTP 限流证据，最终 one-shot 加固后也未在线重跑。 | dated authorization → aggregate Client metrics → bounded historical conclusion |
