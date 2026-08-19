# 排障手册

> **当前边界**：生产Device和精确PE oracle使用Go进程内V8 `149.4.0`，不启动Node；PE只在当前V8与纯Go完整差分一致后才在TTL内走纯算，其余保留V8 fallback。启动器不预热Device；每个合法Solve立即冷建独立Device/V8并在结束时关闭，本地不限制live数量，公开SDK/PE缓存仍复用。2026-08-09历史预热池候选真实50次为 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`，且当时5个对齐存活VM出现过字段合同异常；当前取消保护后该异常是已知风险，不能外推历史性能或正确性。资源峰值和长稳压测仍无结论。排障时不得擅自扩大在线流量或重试同一 `CertifyId`。

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

启动本地服务。当前启动过程本身不会产生Device外部请求；保留 `--device-prewarm=0` 只用于验证旧脚本兼容：

```bash
export ALI_SLIDER_V8_LIBRARY=/absolute/path/libali_slider_v8_runtime.so
go run ./cmd/server \
  --host=127.0.0.1 \
  --port=8000 \
  --device-prewarm=0
```

另一个终端：

```bash
curl --fail --silent http://127.0.0.1:8000/health
```

浏览器测试页位于 `http://127.0.0.1:8000/`。页面打开只检查 health；不要在普通排障或 CI 中点击发送真实求解。

`GET /api/slider?...` 已恢复为 deprecated 旧客户端兼容入口，但它会立即执行真实 Solve，不是 health 或只读调试地址。新调用和内嵌页始终使用 POST JSON。

处理问题时记录 commit、Go 版本、`X-Trace-ID`、HTTP 状态、客户端墙钟和脱敏后的错误类别/阶段。不要收集请求/响应正文、token、`CertifyId`、RPC key、代理密码或上游原始错误。

## 启动与生命周期

| 现象 | 直接证据 | 常见原因 | 处理 |
|---|---|---|---|
| `配置无效` | 进程退出并给出配置边界 | host/port、连接/PE预算、timeout、artifact，或旧prewarm/reserve参数非0 | 对照[配置说明](./configuration.md)修正flag/env；`--device-prewarm`与`--device-reserve`只能为0 |
| `HTTP 服务退出: bind... address already in use` | 启动器退出 | 端口被占用 | Unix 用 `lsof -nP -iTCP:8000 -sTCP:LISTEN`；Windows 用 `Get-NetTCPConnection -LocalPort 8000`；关闭旧实例或换端口 |
| `检查内嵌 V8` 后进程退出 | 未出现 ready | wrapper 文件缺失、架构/格式错误、C ABI 不匹配或 V8/ICU 初始化失败 | 检查 `--v8-library`/`ALI_SLIDER_V8_LIBRARY`、`file`；Linux 再用 `ldd`，Windows 确认 EXE 与 DLL 同为 AMD64；不绕过启动自检 |
| 服务 ready 前较慢 | 端口已绑定，但尚未出现 `event=listen status=ready` | V8 wrapper/ABI/ICU自检或artifact启动清理较慢 | 检查runtime自检与purge日志；当前启动器不调用Prime，也不执行Device Log1/2/3 |
| `event=artifact_purge status=warning` | 启动或每小时出现 | artifact 目录权限/ACL、磁盘或读取失败 | Unix 检查 UID/mode；Windows 检查解压目录可写性和 NTFS ACL；清理失败不应阻止服务，但必须告警 |
| 非回环绑定出现 security warning | `unauthenticated=true` | 服务监听 `0.0.0.0` 或非回环 IP | 确认外层鉴权、防火墙、TLS 和来源限制；否则改回回环 |
| SIGTERM 后关闭慢 | `event=shutdown status=starting` 后等待 | 活跃 Solve/Purge 尚未结束，或某轮Device/V8仍在关闭 | 等待总 timeout；检查上游取消传播、Device Close和长尾阶段，不要强制重复 Verify |
| `/health` ready 但 solve 失败 | health 200、solve 4xx/5xx | health 只验证 HTTP handler，不探测全部上游 | 按 errorType/traceId 和阶段排查；不要把 health 当在线成功率证明 |

启动器会在创建 Client 后立即清理一次 artifact，随后每小时清理，并在 SIGINT/SIGTERM 时执行 HTTP 优雅关闭和 `Client.Close`。

`MaxConcurrency` / `--max-concurrency`只控制Client每route/host的出站连接与PE资源预算，不是Device live或HTTP Handler并发闸门。每个合法请求进入Solver后立即冷建独立Device/V8。外部部署仍必须由网关落实鉴权、频率、并发和总量限制。

## HTTP 排障矩阵

| 现象 | 含义 | 处理 |
|---|---|---|
| 测试页无法打开 | 服务尚未 ready、端口错误或浏览器访问了旧实例 | 先检查 `event=listen status=ready` 和 `/health`；使用启动日志中的实际端口打开根路径 `/` |
| 测试页可打开，但显示 health 异常 | 页面 HTML 已返回，`/health` fetch 失败或不是 ready | 检查浏览器开发者工具的本机请求、端口和服务进程；不要因此重复提交 Solve |
| 旧 `GET /api/slider?...` 返回 `400 ApiRequestError` | query 不是合法 URL encoding、原始 query 超过 64 KiB，或字段/prefix/proxy 校验失败 | 先用 `?SceneId=1ug4aptr` 排查；正确 percent-encode 参数。不得在 URL 中放 `AaduaneId` 或含 userinfo 的 proxy |
| `POST` form body 返回 `400 ApiRequestError` | Solve 不支持 URL-encoded/multipart form body | 使用 `Content-Type: application/json` 和单一 JSON object；不要把 POST query 当作 body 补充 |
| `400 ApiRequestError` | POST JSON 或 GET query 的格式、类型、长度、prefix 或 proxy 在 Solver 前失败 | POST 从 `{}` 开始逐项加入字段；JSON body 和 GET raw query 分别不超过 64 KiB |
| `400 InvalidRequest` | 公共 Client/Solver 认为请求不合法 | 检查 SceneId、prefix、RPC key 和 proxy 长度 |
| `403 ApiOriginError` | 浏览器标记 POST 或 deprecated GET 来自明确跨源 origin/site | 从服务自带根页同源 POST，或使用受信的无浏览器头 API 客户端；不要放宽 CORS/绕过保护 |
| 外层网关直接返回 429 | 网关的身份/IP 频率、并发或总量策略生效 | 按该网关合同处理；不要把它归因于应用内活动数闸门 |
| `500 NetworkError` 且上游记录为 429 | 上游把本轮外部请求作为非 2xx 拒绝 | 用 traceId 与上游受控指标定位；Handler 不透传上游 429 或退避头，同一 `CertifyId` 不得重试 Verify |
| HTTP 200 但 `ok=false` | 协议完成，Verify 业务拒绝 | 读取 `VerifyCode`、`VerifyResult`；不要当技术成功或自动重试 |
| `500 NetworkError` | 设备、Init、动态 PE 公开脚本下载、图片、Complete 或 Verify 网络阶段失败 | 用 traceId/Stage 与受控指标定位；同一 CertifyId 不得重试 Verify |
| `500 VisionError` | PNG/识别失败或低置信 | 检查脱敏 artifact、置信度、坐标和 fixture 回归 |
| `500 ProtocolError` | 动态 PE 路径/脚本/算法不受支持，或 PE、getter、token、上游 schema 合同失败 | 先看 Stage；`resolvePEKey` 运行公开分片探针，其他阶段运行 protocol/PE/device/challenge 测试；禁止输出敏感原文 |
| `500 InternalError` 且 Stage=`deviceSession`/`resolvePEKey`/`buildVerifyData`/`completeDevice` | V8 执行超时/heap 上限、host 输入输出不合法，或 library 直接使用时未先自检 | 运行 `Client.CheckRuntime`和真实 wrapper ABI 测试；根据 Stage 检查上限/脚本合同；不要对同一挑战重试 |
| `500 UnhandledProtocolError` | 未分类 error 或 panic | 视为缺陷；用本地 Mock 最小复现并映射为稳定脱敏错误 |
| HTTP 无普通日志 | 自定义宿主给 `server.New` 的 Logger 为 nil | cmd/server 已注入 stdout logger；library 宿主应显式注入安全 Logger |

所有应用生成的 HTTP JSON 响应应有 `Cache-Control: no-store`；`/api/slider` 的 200/400/403/500 应包含与 body 一致的 `X-Trace-ID`。测试页响应还应有 HTML Content-Type、随机 nonce CSP、`nosniff`、`no-referrer` 和防 framing 头；缺失时视为构建/路由合同回归。Handler 不生成本地 429、`Retry-After`、`TooManyChallenges`、活动数或退避字段；403 仅是跨源浏览器安全边界，不是 admission。外层网关响应遵循网关自己的合同。

GET 与 POST 的参数源严格分离：GET 只读 query，POST 只读 JSON body，POST URL 上的 query 不会填充 body，GET body 也不会参与。GET 同名参数取最后值；`SceneId/prefix/AaduaneId/proxy` canonical 键只要出现就压过对应 alias，空 canonical 值会使用默认值而不是 alias。

## Solver 阶段定位

library 错误的 `Stage` 和成功结果 `timingsMs` 使用稳定阶段名：

| 阶段 | 常见问题 | 检查 |
|---|---|---|
| `setup` | proxy route、画像或 client 构造 | proxy 格式、route 容量、prefix |
| `deviceSession` | 冷建本轮独立V8/FeiLin Isolate并执行Log1/2/3；不含本地live槽等待 | wrapper/ABI、V8/系统资源、设备endpoint/时钟、代理 |
| `init` | InitCaptchaV3 | 签名、SceneId/prefix、出口 |
| `resolvePEKey` | 精确 `StaticPath` 的公开 SDK/PE 下载、V8 画像采样 + 纯 Go 完整差分，或缓存命中 | V8 wrapper、CDN allowlist、代理路由；SDK 软 TTL 5 分钟，PE 硬 TTL 30 分钟；有效缓存命中应接近 0 ms |
| `downloadAssets` | CDN/重定向/大小 | 相对路径、HTTPS allowlist、Content-Length |
| `vision` | PNG 或低置信 | alpha、尺寸、候选、edge-decoy |
| `buildVerifyData` | 已验证分片用纯 Go Builder，其余在禁网 V8 Isolate 执行；两者都独立复核 | Track、ExpectedXPos、DeviceConfig、getter、Pack/Unpack、逻辑时钟；V8 fallback 再检查 timeout/heap |
| `completeDevice` | 在同一FeiLin VM回放PE events/getter并执行最终Log2 | VM生命周期、事件顺序、getter、DeviceToken；若并发出现137/138字段或sequence错误，参照2026-08-09的5+ live历史异常，降低外层并发复验 |
| `verify` | 唯一 Verify 请求/响应 | 网络未知也不得重试同一挑战 |
| `clientCleanup` | 关闭本轮Device/V8 | session Close、V8/子进程清理和取消竞态；当前不补货、不回池 |

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

## 逐请求Device会话

| 现象 | 原因 | 处理 |
|---|---|---|
| 每次请求的 `deviceSession` 都包含初始化 | 当前设计每轮都冷建Device/V8，不存在预热命中 | 按预期记录完整Log1/2/3；不要尝试恢复会话复用 |
| 并发升高后 `deviceSession` 变慢或出现137/138字段、sequence错误 | 本地不限制Device/V8 live数量；CPU/内存/线程和上游压力直接增长，且历史5+ live曾出现字段合同异常 | 先在外层网关降低并发并受控复验；调整 `MaxConcurrency` 只改变连接/PE预算，不会限制Device会话数 |
| 调低 `MaxConcurrency` 后仍同时冷建大量Device会话 | 该字段不再是Device live并发限制 | 在外层网关实施经过容量验证的并发/速率限制；不要提高prewarm/reserve，它们只接受0 |
| 请求完成后仍持续占用V8/子进程资源 | Device session未走到Close，或V8关闭阻塞 | 查看 `clientCleanup`、context取消与线程采样；成功、失败和取消都必须Close |
| 调用 `Client.Prime` 没有Device流量 | `Prime`只保留为源码兼容空操作 | 正常；不要把Prime返回nil当作热态已建立 |
| `--device-prewarm`/`--device-reserve` 非0启动失败 | 预热/复用已移除，旧参数只兼容0 | 删除旧非0配置，或显式设置两者为0 |

公开SDK、精确PE源码和已验证结构画像仍由进程级KeyResolver按TTL复用；这类缓存命中不会复用DeviceToken、`CertifyId`、DeviceConfig、轨迹或Device/V8会话。

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
| `dynamic PE runtime failed` | 首次画像或 fallback 所需 V8 wrapper/ABI 不可用、Isolate 超时/heap 超限或输出不合法 | 用 `Client.CheckRuntime`、`ALI_SLIDER_V8_TEST_LIBRARY=... go test ./internal/v8runtime ./internal/pe` 和显式组件探针检查；不得因快路存在就省略 `.so`/DLL |
| `unsupported dynamic PE script` | StaticPath 格式、公开脚本结构、候选唯一性或 Go 独立复核失败 | 不随机兜底、不 Verify；保存路径和稳定错误类别，不保存真实 CertifyId/正文，更新 bridge 前先做离线逆向验证 |
| 同一路径每轮都重采画像 | Client/KeyResolver 被逐请求重建、SDK 字节变化、PE 硬 TTL 到期，或前次采样失败 | 复用一个 `slider.Client`；用 `resolvePEKey` 耗时和脱敏 profile 计数区分 miss；不要延长硬 TTL 或缓存挑战级 token/data |
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
| 目录创建/写入失败 | UID、父目录、Windows ACL、只读文件系统或磁盘配额 | Unix 保持 `0700/0600`；Windows 解压到当前用户私有可写目录并检查继承 ACL |
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
| 同一进程前后耗时差异大 | 把公开SDK/PE缓存命中误认为Device预热 | Device始终逐轮冷建；分开报告Device冷建、同host连接等待和 `resolvePEKey` 缓存命中/刷新 |
| 并行视觉快但在线慢 | 把算法吞吐当完整链 | 分别报告视觉、纯计算、Solver、HTTP 和在线 |
| 历史前25次与后25次不同 | 把后半窗口写成全部50次结果，或把旧预热池结果外推到当前实现 | 历史全批mean为 `2562ms`，不是后25的 `2232ms`；当前逐请求冷建路径必须重新采样，并检查Device冷建、同host连接等待、分片数和 `resolvePEKey` |

当前纯计算 `P99=57.05075ms`；2026-08-07 的历史授权 Client harness 严格成功 `196/200`，完整求解链墙钟 `P95=984ms`，达到当时两项冻结目标。该批 `P99=1018ms`，不得声称 P99 小于 1 秒、HTTP 端到端已测或 200/32 是当前 Handler 限制；也不得把它写成 one-shot 加固后最终源码的第二份在线报告。完整方法见[性能测试与容量口径](./performance.md)和[脱敏验证证据](./evidence/validation-2026-08-07.md)。

## 构建、CI 与 Docker

| 现象 | 原因 | 处理 |
|---|---|---|
| module 要求 Go 1.26.6 | 本机旧工具链且无法自动获取 | 安装 Go 1.26.6；CI 使用 `GOTOOLCHAIN=local` 和精确版本 |
| `staticcheck` 不在 PATH | 本机未安装组织工具 | 使用 CI 固定 `v0.7.0` 或按组织流程安装；项目验证记录已通过 |
| `govulncheck` 不在 PATH/查询失败 | 工具或漏洞库网络不可用 | 使用 CI 固定 `v1.1.4`；区分扫描基础设施失败与发现漏洞 |
| PR 没有 Go CI | 变更路径未命中或工作流未触发 | 检查 `.github/workflows/ali-slider-go-ci.yml` paths；可 workflow_dispatch |
| 覆盖率变化 | 并发分支命中、commit/命令/缓存不同 | 使用 `-count=1` 和单一 coverprofile，记录 commit；当前统一结果为 `80.4%`，超过 `>=80%` 门槛 |
| `go build ./...` 没有目标文件 | 该命令只验证包 | 使用 `make build-linux` 生成 `dist/ali-slider-go-linux-amd64` |
| Linux launcher 显示依赖 `libdl.so.2`/glibc | `purego` 通过动态加载器调用 V8 wrapper | 正常；完整部署是 Debian/glibc + launcher + 同架构 `.so`，不是单静态 ELF |
| 裸 Linux 服务启动时报 V8 library 不可用 | 只部署 Go launcher，或 `.so` 架构/版本不匹配 | 将同产物的 `libali_slider_v8_runtime.so` 放在 launcher 同目录，或设置 `ALI_SLIDER_V8_LIBRARY` 绝对路径；用 `file`/`ldd` 检查 |
| Actions 没有 Windows ZIP | quality/race/Windows job 失败、运行来自 PR 或 artifact 已过期 | 打开同一 run 的 `windows-package`；只在 main push/手动运行上传，保留 30 天 |
| Windows 显示未知发布者 | EXE 未做 Authenticode 商业签名 | 从可信仓库下载并核对 SHA-256；不要关闭 Defender 或绕过组织策略 |
| Windows 提示应用无法运行 | 系统低于 Windows 10 / Windows Server 2016、使用 32 位 Windows 或非原生架构 | 当前包要求 Windows 10 / Windows Server 2016 或更高版本的 AMD64/x64；不要把它标成所有 Windows 通用包 |
| 双击后只有控制台，没有桌面窗口 | 程序是控制台 HTTP 服务；测试 UI 在浏览器中 | 完整解压后运行 `start.bat`，等待 console 的 ready 日志，再手工打开 `http://127.0.0.1:8000/`；换端口时同步修改 URL |
| 容器端口不可达 | 应用默认监听容器回环 | 容器内设 `ALI_SLIDER_HOST=0.0.0.0`，宿主只发布到 `127.0.0.1` |
| 仍按旧说明假设容器是 `scratch`/Alpine | 当前镜像是 Debian bookworm-slim，以提供 wrapper 所需 glibc | 继续从宿主做 health；镜像扫描需覆盖 Debian/glibc、V8、ICU 和 Rust wrapper |
| artifact 重启后丢失 | 容器未挂持久卷 | 将 `/app/var/artifacts` 挂受控卷并保持 UID 65532 权限 |

已验证的候选门禁包括 Go `1.26.5` 全量 test/race/vet、Linux AMD64/ARM64 Rust native + Go→V8 ABI 测试、包构建及 Docker 非 root `/health` smoke；Windows 原生执行仍以发布 commit CI 为准。在线验收不属于 CI；普通排障不得自动重跑历史挑战批次。

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
| `cmd/server/main.go` · `run` | 启动器已实现配置、runtime自检、启动/每小时清理和优雅关闭；不调用Prime。 | config → Client/runtime check → HTTP → lifecycle |
| `internal/server/server.go` · `checkSolveOrigin` / `decodeQueryRequest` / `handleSolve`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` / `TestBrowserOriginBoundaryPreservesLegacyClients` / `TestConcurrentRequestsAlwaysEnterSolver` | POST JSON 与 deprecated GET query 分源解析；无效输入和明确跨源在 Solver 前拒绝，其他合法请求不按活动数拒绝。 | method/source → origin + JSON/query validation → timeout context → Solve or stable error |
| `internal/server/testpage.go` · `writeTestPage`；`internal/server/server_test.go` · `TestEmbeddedAPITestPage` | 根路径只允许 GET，返回带严格页面安全头的内嵌测试台且不触发 Solver。 | start → browser GET `/` → health / explicit solve |
| `internal/challenge/solver.go:107`、`:149`、`:289` | 完整 Solver 已按阶段执行并只在前置门禁后 Verify。 | setup → device/init/assets/vision/PE → Verify |
| `internal/challenge/solver_test.go` · `TestSolverLowConfidenceStopsBeforeVerify` / `TestSolverVerifyNetworkErrorIsSingleAttemptAndSanitized` | 低置信零 Verify、网络未知单次 Verify 有计数测试。 | failure branch → counted transport calls |
| `internal/pe/keys.go` · `NewKeyResolverWithCapacity`；`internal/pe/device_runtime.go` · `OpenDevice` / `Close` | 每个合法Solve立即冷建/关闭Device会话，本地没有live并发槽；公开SDK/PE缓存不含挑战态。 | Solve → Open → Close |
| `pkg/slider/client.go` · `Prime` / `PurgeArtifacts` / `Close` | `Prime`是兼容空操作；library暴露Purge，并在Close前等待活跃Solve/Prime/Purge。 | caller lifecycle → safe resource cleanup |
| `internal/artifact/store.go:121`；`cmd/server/main.go:147` | Store 只清理受管过期文件；服务已按小时调度。 | failure artifact → retention → scheduled Purge |
| `.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `windows-package` | Linux quality、Darwin race 与 Windows 原生 package 都固定 `CGO_ENABLED=0`；只有全部通过才上传便携 ZIP。 | source change → cross-platform gates → verified ZIP |
| `Dockerfile`；`internal/pe/v8_runtime.go`；`internal/v8runtime/runtime.go`；`native/v8runtime/src/lib.rs` | 运行镜像固定携带同架构 V8 wrapper，服务在 ready 前校验 ABI/V8/ICU 并保持非 root；生产不依赖 Node/PATH。 | Go launcher + V8 wrapper → startup check → bounded script cache + per-challenge Isolates |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165` | Device schema 只在 Log1 解码对象，Log2/3 接受非对象成功结果。 | raw response → action gate → stable session |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的历史 Client harness 以 200/32/应用层零重试运行并达到当时成功率与完整链 P95 门槛；不经过 HTTP Handler，不能作为当前 HTTP 限流证据，最终 one-shot 加固后也未在线重跑。 | dated authorization → aggregate Client metrics → bounded historical conclusion |
