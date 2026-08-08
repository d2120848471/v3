# 安全边界

## 1. 默认信任模型

> 服务没有应用内 API 鉴权，也没有基于活动请求数的本地接纳闸门。默认只监听 `127.0.0.1:8000`。显式改为 `0.0.0.0` 时，任何网络可达客户端都可以直接触发 `Solve`、消耗 CPU/内存、Client 连接/预热预算和上游配额，还可以传入代理路由并促使服务对外连接。`GET /api/slider?...` 只是 deprecated 的旧客户端兼容入口，会立即执行与 POST 相同的真实求解，不是安全的读操作。`GET /` 测试页与 API 使用同一信任边界，不提供账号或权限隔离。

服务的 HTTP surface 仍是四个路径：`GET /`、`GET|POST /api/slider`、`GET /health` 和 `GET /openapi.json`。Solve 的两种方法都只声明 `200/400/403/500`；Handler 不主动返回 429。

对外暴露前必须由反向代理/API gateway 提供：

- TLS 和调用者身份验证；
- 按身份/IP 的频率、并发和总量限制；
- 禁止请求/响应正文日志；
- 代理字段禁用或 allowlist；
- 防火墙/安全组的受信来源限制。

Handler 不生成本地 429、`Retry-After`、`TooManyChallenges`、活动数或退避字段。外层网关和上游仍可能返回 429，但这不等于应用内存在资源保护；因此外层鉴权以及按身份/IP 的频率、并发和总量限制是公网或共享网络部署的必要前置条件。

`MaxConcurrency` / `--max-concurrency` 是兼容旧名，只作为 Client 每 route/host 的出站连接与设备预热资源预算；它不限制 HTTP 请求进入。启动器在非回环监听时输出一条纯文本安全警告，但警告不是强制控制。

回环监听只缩小网络暴露，不阻止同机不可信进程访问，也不能把浏览器页面当作身份边界。API 不启用宽松 CORS；Handler 根据 `Origin` / `Sec-Fetch-Site` 拒绝明确的浏览器跨源 POST 和 deprecated GET，但这只是 drive-by 缓解，不是鉴权或完整 CSRF 保护。Go `CrossOriginProtection` 原本会把 GET 视为安全方法，所以 Handler 在同源检查副本中将旧 GET 当作 POST，再保留原请求的 GET/query 语义。明确跨源返回 `403 ApiOriginError` 且不进入 Solver；同源请求、用户在地址栏直接发起的 `Sec-Fetch-Site: none` 以及无浏览器头的旧 API 客户端仍允许。共享机器仍需操作系统用户隔离和外层访问控制。

## 2. 授权与流量边界

仅允许在自有系统或具有明确书面授权的测试环境使用。使用方负责确认 SceneId、prefix、目标、并发、时间窗和总挑战数均在授权内。

2026-08-07 的历史授权候选批次上限是 200 个挑战、并发 32；它恰好执行 200 轮，每个 job 一次 `Client.Solve`、应用层零重试，严格成功 `196/200`。该批不经过 HTTP Handler，因此 200/32 只描述当时 Client harness 的授权与负载，不是当前 Handler 的并发或总量限制。全程只保留脱敏聚合统计，不在文档中保存令牌、挑战标识、图片或上游正文；该批先于传输 one-shot 加固，最终源码未获授权再跑第二批。普通 CI 仍只运行 Mock 和静态 fixture，不访问 Device/Captcha/CDN 目标；同一 `CertifyId` 始终不得重试 Verify。

## 3. 敏感数据分类

| 数据 | 分类 | 允许出现 | 禁止出现 |
|---|---|---|---|
| `securityToken` | 高敏感运行令牌 | API/library 明确结果字段、测试页当前内存 | 普通日志、artifact、错误、String/GoString、浏览器持久化 |
| `CertifyId` | 敏感挑战标识 | API/library 明确结果字段、单轮/测试页当前内存 | 普通日志、artifact metrics、错误、浏览器持久化 |
| 代理用户名/密码 | 高敏感凭据 | 单路由 transport 运行时内存 | map 明文 key、日志、格式化输出、artifact |
| `AaduaneId` / RPC key | 敏感协议材料 | 签名请求内存 | 日志、artifact、String/GoString |
| 失败图像 | 限制级调试证据 | 私有 artifact 目录 | Git、Web root、普通响应 |

`pkg/slider.Request` / `Result`、内部 Solve DTO、Captcha DTO、DevicePoolKey、TransportPool 都有脱敏 `String/GoString`。这只防止常见 `%v/%#v` 误用，不阻止调用方显式访问字段；业务代码仍必须避免正文日志。

### 内嵌测试页

测试页由 `go:embed` 编译进 EXE，不加载第三方脚本、CSS、字体、图片或遥测。页面打开只执行 `/health`，Solve 必须由用户显式提交；执行期间禁用重复提交，并且超时、取消、HTTP/业务失败都不自动重试。

页面安全控制：

- 每次 `GET /` 生成新的 128-bit 密码学随机 CSP nonce；CSP 只允许同源 connect，禁止无 nonce 脚本/样式、外部资源、frame、form action、worker 和 object；
- `Cache-Control: no-store`、`nosniff`、`no-referrer`、`X-Frame-Options: DENY`、同源 CORP/COOP 和最小 `Permissions-Policy`；
- 返回数据只用 `textContent`，不使用 `innerHTML`、`eval` 或动态代码执行；
- `AaduaneId`、proxy、`securityToken`、`certifyId` 默认遮罩；显示明文需要用户显式勾选；
- 不使用 Cookie、URL query/hash、localStorage、sessionStorage、IndexedDB、service worker 或 sendBeacon；刷新/清空即移除当前页面状态。
- Handler 在解码和 Solver 之前拒绝明确跨源的浏览器 POST/deprecated GET；测试页始终使用同源 POST JSON，不生成 legacy query。

这些控制减少页面自身泄漏和 DOM 注入风险，不保护被其他本机客户端直接调用的 `/api/slider`。调用方仍不得截图或把完整请求/响应复制到日志、工单或聊天。

### Deprecated GET query

旧 GET 只用于兼容已存在的 Python 客户端；新接入和内嵌测试页必须使用 POST JSON。GET query 的原始长度上限为 `64 KiB`，支持 `SceneId/sceneId`、`prefix/Prefix`、`AaduaneId/aaduaneId` 和 `proxy/Proxy`。同名参数取最后一个值；canonical 键只要出现就压过 alias，即使 canonical 值为空也不回退到 alias，而是按未传使用默认值。

`server.MaxRequestBytes` 同时定义 POST body 和 raw query 的 `64 KiB` 数据上限；进程入口的 `MaxHeaderBytes` 另外保留 `32 KiB` 给 request line 和普通 header，使恰好 64 KiB 的 query 能经过真实 `net/http.Server` 到达 Handler。这个传输预算不放大 query 可用数据边界，也不是拒绝异常大 header 的替代。

GET 只读 query，POST 只读 JSON body，两者不合并；服务不接受 `application/x-www-form-urlencoded` 或 multipart form body。GET URL 可能进入浏览器历史、代理/网关 access log、监控采集和复制粘贴记录；禁止在 URL 中放入 `AaduaneId` 或含 userinfo 的 proxy。不要把该 URL 写入 HTML 链接、浏览器预取、健康检查、搜索爬虫或 uptime 监控：每次成功路由都会创建真实挑战并消耗上游配额。

## 4. 日志与错误

Handler 的普通日志只记录：

```text
traceId, status, elapsedMs
```

启动日志记录监听地址、每 host 最大连接资源预算、超时及 purge/prime 类别，不记录 token、CertifyId、代理或上游正文。

错误边界：

- API JSON/query 输入错误返回稳定 400 JSON 错误；明确浏览器跨源 POST/deprecated GET 返回脱敏 `403 ApiOriginError`；
- 可识别的协议、网络、视觉和内部错误只返回稳定 Message；
- 未分类错误或 panic 统一为 `UnhandledProtocolError`，不回显 panic/error cause；
- HTTP 2xx 中的非法 JSON/schema 计入 Protocol，不污染“网络失败”统计；
- DNS/TLS/连接/非 2xx/响应读取/超时才是 Network 类。

## 5. Artifact 保护

Store 只在失败、低置信或 Verify 业务拒绝时保存可用图像和脱敏 metrics；成功路径不触盘。metrics 只含：

- UTC 记录时间；
- 稳定 reason/stage；
- confidence、xPos、slidePos；
- 分阶段毫秒耗时。

文件级控制：

- 目录必须是真实目录并拒绝符号链接；Unix 使用时收紧并精确复核为 `0700`，无法收紧则报错；
- Unix 文件以 `0600` + `O_EXCL` 创建；Windows 继承父目录 NTFS ACL 并保留 `O_EXCL`，因为 Go `FileMode` 无法表达 Windows DACL；
- 16 字节密码学随机 ID，文件名为 32 位 hex + 受管后缀；
- 任一写入失败回滚本组已建文件；
- 同一目录的 Save/Purge 在进程内串行配额决策；
- 默认保留 7 天；进程启动清理一次，之后每小时清理；
- 默认硬上限 64 组和 512 MiB，写入前先清过期，再按完整组淘汰最旧记录；
- 超大单组安全拒绝；非受管文件与符号链接不删除。

配额是可用性上限，不是数据脱敏的替代。Artifact 目录不得放在 Web root，需要单独磁盘告警。Windows 便携包必须解压到当前用户的私有可写目录，不得放公共共享盘、多人可写目录或公开同步目录；如需显式组织 DACL，应在部署层配置并验证。

## 6. 代理、SSRF 与路由池

API 支持 `http` / `https` / `socks4` / `socks5` / `socks5h` 代理。这意味着无鉴权客户端可以要求服务连接自选代理地址，可能用于内网探测或资源消耗。应用外 allowlist/禁用是唯一可靠的业务级缓解。

内部硬化：

- 拒绝无 host、非法 scheme/端口、path/query/fragment 和不合法认证形式；
- scheme/hostname 小写、根 path 移除、HTTP 80 / HTTPS 443 归一，防止等价 route 重复占槽；
- route map 使用 canonical URL 的 SHA-256 摘要，不以含密码 URL 作明文 key；
- 直连 route 预留且不淘汰，明确忽略进程 `HTTP_PROXY/HTTPS_PROXY`；
- 路由表最多 32，对非直连 route 使用有界 LRU，淘汰时关闭空闲连接；这是 Client 资源边界，不是 Handler 接纳上限；
- 每 host 最多 32 连接，TLS 最低 1.2，拨号/握手/响应头/空闲均有边界；连接预算不会阻止更多合法 HTTP 请求进入 Solver；
- Client.Close 清空 route map，缩短凭据引用存活时间。

代理凭据仍必须在 transport 运行时内存中存在，SHA-256 map key 不等于内存加密。

## 7. 图片资产边界

Init 只能提供相对路径，不能指定完整 URL。下载器：

- 固定构造 `https://static-captcha.aliyuncs.com/`；
- 拒绝绝对路径、`..` 和越界字符；
- 每次重定向都重新验证 HTTPS、精确 hostname 和无 userinfo；
- 最多 3 次完成重定向；
- 单图默认 8 MiB，可配但硬上限 64 MiB；
- 同时校验 Content-Length 和 `LimitReader(limit+1)`；
- 两图并发下载，首个错误立即取消同伴并 drain 结果；
- 视觉层在完整解码前校验 PNG/IHDR/边长/总像素/shadow alpha。

## 8. 唯一 Verify

完整链的不变量：

1. 每轮 Solver 只创建一个 RPCClient。
2. RPCClient 只允许一次 Init，并记录成功签发的 `CertifyId`。
3. Verify 仅接受该 RPCClient 签发的 ID。
4. Verify 尝试位在任何网络发送之前消耗；请求清空 `GetBody`，阻止 `net/http` 在已发送 body 后透明重放；网络未知也不回滚。
5. 轨迹、PE schema、getter、Device Complete、置信度或 context 门禁失败时，Verify 调用数为零。
6. 业务拒绝是一次已完成 Verify，绝不重试。

当前保证的作用域是单轮生产状态机。上游正常每次 Init 返回新 ID；进程不维护无界的全局历史 ID 数据库。

## 9. 供应链与运行时

- 生产 module 零第三方 Go 依赖。
- `go.mod`、CI 和 Docker 使用 Go 1.26.5；本地 `govulncheck` 在该工具链下未发现可达漏洞。
- Linux AMD64 二进制使用 `CGO_ENABLED=0`、`-trimpath`、stripped 静态构建。
- Windows AMD64 便携版使用 `CGO_ENABLED=0` 的单一 console PE；测试页同样内嵌于 EXE，不依赖 Go、Python、Node、VC++ Runtime、CDN 或第三方 DLL。Windows 原生 CI 负责 test、解压启动和本地 HTTP smoke。
- 官方 GitHub actions 使用完整 commit SHA 固定；普通 CI 权限保持 `contents: read`。Windows ZIP 包含 commit/build 信息和 SHA-256。
- 当前 Windows EXE 没有 Authenticode 代码签名；SmartScreen 可能提示未知发布者。SHA-256 不替代发布者签名，不得要求用户关闭 Defender。
- 容器运行层为 `scratch`，只含 CA 证书、二进制和私有 artifact 目录；使用 UID/GID 65532。
- Linux AMD64 Docker 镜像构建及关闭预热后的非 root `/health` smoke 已验证通过。
- CI 中项目测试和分析设为 `GOPROXY=off`，不访问真实业务目标。工具下载和漏洞库查询仍需要供应链网络；这不是完全 air-gap job。

## 10. 上线前检查清单

- [x] 2026-08-07 历史候选批次的目标、时间窗、并发 32 和 200 挑战总数已授权并执行完毕；该证据仅覆盖 Client harness。
- [ ] 默认回环监听；如需非回环，外层鉴权、TLS、防火墙以及按身份/IP 的频率、并发和总量限制已就绪。
- [ ] 代理字段已禁用、限制调用方或配置 allowlist。
- [ ] 反向代理、APM、客户端日志不收集正文。
- [ ] 测试页只部署在符合本 API 信任模型的网络边界；使用者已知页面显示值可能敏感，未启用浏览器/代理正文采集。
- [ ] Artifact 目录不在 Web root，权限、配额、保留期和磁盘告警已验证。
- [x] 当前源码快照的全量 unit/mock/race/vet/staticcheck/govulncheck/coverage 门禁通过；统一覆盖率重复运行为 `83.1–83.2%`，稳定超过 `>=80%` 门槛，发布 commit 仍须 CI 重跑。
- [x] Linux AMD64 静态构建与容器非 root `/health` smoke 通过。
- [ ] Windows AMD64 原生测试、最终 ZIP smoke、SHA-256 与 Artifact 下载已在发布 commit 的 CI 中通过并归档。
- [x] 纯计算 P99 与 2026-08-07 历史候选批次 200 轮、32 并发、应用层零重试的 Client 完整求解链 P95/成功率报告已生成并达到当时冻结门槛；该批未经过 HTTP Handler，且先于最终 one-shot 加固。
- [ ] 针对实际机器资源的 RSS、GC、goroutine 峰值和长时间稳定性压测，以及生产外层控制的上线评审已完成。

## 11. Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/server/server.go` · `MaxRequestBytes` / `checkSolveOrigin` / `decodeQueryRequest` / `handleSolve`；`cmd/server/main.go` · `maxHeaderBytes`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` / `TestBrowserOriginBoundaryPreservesLegacyClients` / `TestConcurrentRequestsAlwaysEnterSolver`；`cmd/server/main_test.go` · `TestHeaderBudgetAcceptsMaximumLegacyQuery` | POST JSON 与 deprecated GET query 共用参数/Solver 合同但不合并输入；真实 HTTP 入口可承载恰好 64 KiB query；明确跨源和无效输入在 Solver 前拒绝，其他合法请求无本地 admission、429 或退避字段。 | reachable caller → HTTP budget → origin + JSON/query validation → Solve → local and upstream resource consumption |
| `internal/server/testpage.go` · `writeTestPage`；`internal/server/server_test.go` · `TestEmbeddedAPITestPage` | 页面使用随机 nonce、同源 CSP、无持久化/外链/危险 DOM API；根路径 GET 不调用 Solver，手工求解只发送同源 POST。 | browser GET `/` → constrained page memory → explicit same-origin POST |
| `internal/server/server.go` · `logResult` / `solverErrorResponse` | 普通日志只有 trace/status/elapsed，未分类错误不回显 cause。 | request/result → stable metadata log |
| `pkg/slider/types.go` · DTO `String/GoString` | 公开结构的常见格式化不输出 token、ID 或代理凭据。 | DTO → fmt → redacted text |
| `internal/artifact/store.go` · `SaveFailure` / `Purge` | Unix 强制 POSIX 私有 mode；Windows 继承 NTFS ACL；两者共用受管名称、排他创建、整组回滚、保留期和配额。 | platform directory policy → private bounded set → purge/evict |
| `internal/challenge/pool.go` · `canonicalProxyRoute` / `TransportPool.Get` | 路由归一、摘要 key、直连保留和 LRU 限制凭据泄漏与永久耗尽。 | proxy → canonical → SHA-256 key → bounded transport |
| `internal/challenge/assets.go` · `DownloadAssets` | 固定 CDN、逐跳校验、字节上限和首错取消降低 SSRF/内存/尾延迟风险。 | relative path → allowlisted HTTPS → bounded bytes |
| `internal/challenge/rpc.go` · `RPCClient.Init/Verify` | Init 只一次，Verify 绑定该 ID 并在网络前消耗唯一尝试位。 | issued ID → irreversible Verify attempt |
| `.github/workflows/ali-slider-go-ci.yml` | 官方 action 全 SHA 固定；项目测试与 Windows smoke 不访问真实目标，工具与 vuln DB 供应链网络被明确区分。 | pinned actions/tools → offline project checks → verified binary/ZIP |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165` | Device `ResultObject` 保留原始 JSON，只在 Log1 解码配置，避免对 Log2/3 施加错误对象 schema。 | bounded response → action-specific decode → regression proof |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 的历史 Client harness 每个 job 只调用一次 Solve，只输出分类计数和聚合延迟；`196/200` 且 network `0`。它不经过 HTTP Handler；最终源码通过 `GetBody == nil` 离线回归证明 one-shot，但未在线重跑。 | dated authorization → no-application-retry Client jobs → sanitized aggregate → bounded historical finding |
