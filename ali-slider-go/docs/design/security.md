# 安全边界

服务默认监听 `127.0.0.1:8000`，没有应用内鉴权，也没有 HTTP 在途请求或 Device 会话的本地并发闸门。每个合法 Solve 都可以冷建独立 Device/V8 并访问上游；`MaxConcurrency` 只控制出站每 host 连接和 PE 空闲引擎保留量。仅用于自有系统或已获授权的测试环境。

## 网络与浏览器入口

HTTP 只开放 `/`、`/api/slider`、`/health`、`/openapi.json`。Solve 支持 POST JSON 和已废弃的 GET query；两者均有真实副作用。默认回环监听缩小网络暴露，不能阻止同机不可信进程访问。

在共享或外部网络提供服务时，部署层需要提供调用者鉴权、TLS、受信来源限制和经容量验证的频率/并发/总量限制。代理字段应限制调用方或配置 allowlist。不要收集请求/响应正文、完整 Solve query 或代理凭据。外层网关可以返回自身的 429；这不是应用内资源保护，也不能与 Solver 遇到的上游 429 混为一类。

`httpapi.Handler` 使用 `Origin` / `Sec-Fetch-Site` 拒绝明确浏览器跨源的 POST 和旧 GET Solve。旧 GET 在同源检查副本中按有副作用的方法处理，实际解析仍保留 GET/query 语义。拒绝时为 `403 ApiOriginError` 且零 Solver；同源、地址栏 `Sec-Fetch-Site: none` 和没有浏览器来源头的程序客户端仍允许。该保护不是身份认证或完整账号 CSRF 机制。

POST body 与 GET raw query 分别最多 64 KiB，两个来源不合并。入口额外给 request line/header 预留 32 KiB，使合法上限能到达 Handler；这不会扩大 query 上限。旧 GET URL 可能进入历史和访问日志，不应携带 RPC key 或代理账号密码，也不能作为预取、书签、health 或监控地址。完整字段合同见 [HTTP API](../reference/http-api.md)。

## 敏感数据与日志

| 数据 | 必要使用位置 | 普通日志/报告边界 |
|---|---|---|
| `securityToken`、DeviceToken | 本轮内存、明确 API/SDK 结果字段 | 不记录原值 |
| `CertifyId` | 本轮挑战绑定、明确结果字段 | 不写普通日志、artifact metrics 或错误 |
| 代理用户名/密码 | Go Transport 内存 | 不打印 URL/userinfo，不注入 JS |
| `AaduaneId`/RPC key | 签名请求内存 | 不写日志、artifact 或格式化输出 |
| 失败图片 | 私有 artifact 目录 | 不放 Git、Web root 或普通响应 |

公开 `slider.Request`/`Result`、内部 DTO 和 TransportPool 的 `String/GoString` 会脱敏。这只保护常见格式化调用，调用方显式读取字段仍可暴露敏感材料。HTTP 普通日志只记录 traceId、状态和耗时；启动日志记录事件、监听地址和资源配置。

SDK 的 `Error` 与应用 `Failure` 保留稳定类别/阶段；HTTP 不回显底层 cause。未分类错误或 panic 返回脱敏 `UnhandledProtocolError`。DNS/TLS、连接、非 2xx、读取和超时归入网络失败；HTTP 成功状态中的非法 JSON/schema 属于协议失败。

## 内嵌测试页

测试页编译进 Go 主程序，不加载第三方脚本、字体或遥测。打开只检查 health，Solve 必须手工提交；执行期间防重复提交，取消、超时或业务拒绝不触发自动重试。

每次响应使用新的 128-bit 密码学随机 CSP nonce，只允许同源连接；页面响应包含 no-store、nosniff、no-referrer 和禁止 framing 的控制。结果通过 `textContent` 显示，敏感字段默认遮罩，不保存到 Cookie、URL、localStorage、sessionStorage 或其他浏览器持久存储。刷新或清空会移除当前页面状态，但不能撤销已发送的请求。

测试页减少自身泄漏面，不隔离其他可达 API 客户端，也不保护用户主动截图或复制出的完整响应。

## Artifact 存储

`solve.FailureRecorder` 只接收图片与脱敏 `FailureSample`，bootstrap 将它映射到 `artifact.Store`。成功路径不落盘；失败、低置信或 Verify 业务拒绝时保存当轮已取得的图像和 metrics。指标包含 UTC 时间、稳定 reason/stage、置信度、位置与阶段耗时，不含挑战/token。

Store 的文件边界：

- 目录必须是真实目录，拒绝文件系统根目录和符号链接；Unix 收紧并复核 `0700`。
- 文件使用随机受管名称和 `O_EXCL`；Unix 为 `0600`，Windows 继承解压目录 NTFS ACL。
- 任一文件写入失败回滚本组已建文件，同目录的配额决策在进程内串行处理。
- 默认最多 64 组、512 MiB、保留 7 天；写入前清理过期并按组淘汰最旧记录，拒绝过大单组。
- 不删除非受管文件或跟随符号链接。HTTP 宿主在启动时和每小时清理，SDK 宿主自行调度。

保存失败沿用 best-effort 语义，不覆盖原业务结果；清理错误由宿主报告。配额不代替权限和脱敏，目录不应位于 Web root、公共共享盘或公开同步目录。Windows 的 Go FileMode 不表示完整 DACL，必须在私有可写目录运行。

## 代理与图片下载

API 支持 HTTP(S)、SOCKS4、SOCKS5、SOCKS5H。可达调用方能选择代理地址，因此仍可能要求服务连接内网代理或消耗连接资源；业务层面的限制由受控调用方或外层策略负责。

`httpclient.TransportPool` 校验 scheme、host、端口、认证形式与纯代理 origin，规范化等价路由；map 使用规范 URL 的 SHA-256 摘要，不以含密码 URL 作明文 key。直连显式忽略环境 proxy 并保留，代理路由受 LRU 上限约束。淘汰关闭空闲连接，service 关闭时清空路由表；摘要 key 不等于内存加密，运行中的 Transport 仍需保存凭据。

`aliyun` 图片适配器只接受受限相对路径，固定构造 HTTPS 图片 CDN 地址。每次重定向复核精确主机和无 userinfo，最多 3 次重定向；默认单图 8 MiB、硬上限 64 MiB，同时验证 Content-Length 与有界读取。双图并发下载首错取消同伴。domain/vision 在完整解码前验证 PNG、尺寸/像素与 shadow alpha，低置信时应用停止，不发送 Verify。

## 动态脚本与原生运行时

`engine.KeyResolver` 根据 Init 的精确 `StaticPath` 下载和验证当前脚本。路径必须满足固定版本、分片号与摘要格式；SDK/PE 只访问对应官方 HTTPS CDN，默认端口、无 userinfo，并逐跳验证重定向。单公开脚本上限 2 MiB。

Device/V8 每轮保持独立画像和同一 FeiLin 状态，全部网络由 Go host 通过本轮 Transport 执行。JS 不直接获得 socket 或代理环境变量。官方 SDK 桥另允许类型所需的 `x.alicdn.com` 动态资源，但不开放任意主机。

未知或到期的精确 PE 在禁网 V8 context 中采样，以假挑战输入与纯 Go Builder 的 payload、事件和时钟逐字段差分。只有一致的 profile 才在 TTL 内走纯 Go；不兼容时保留禁网 V8 fallback，输出仍验证 session/schema/坐标/getter/事件/逻辑时钟。不能以随机值或近似 payload 掩盖漂移。

SDK 软 TTL 为 5 分钟，PE/profile 硬 TTL 为 30 分钟，后续访问时检查。缓存不保留 DeviceToken、CertifyId、DeviceConfig、轨迹或 data。组件 JS/CSS 的小缓存只属于当前 Device engine，不跨挑战保留会话。

wrapper 对源码、输入、结果和 host JSON 设大小边界，单 Isolate heap 上限 512 MiB，child script 默认 1 秒、最长 10 秒，Go context 取消会请求 V8 终止。Device 会话在本轮成功、失败或取消后关闭。V8 Isolate 与 Go 同进程，不能视为操作系统沙箱；native abort、内存破坏或未终止执行仍可影响整个服务。

生产不启动 Node，也不创建临时 bridge 子进程；可选 Node oracle 仅用于测试。裸机使用低权限服务账号，Docker 使用 UID/GID `65532`。公开脚本、V8/Rust/ICU 和系统动态库仍属于供应链风险面。

## 唯一 Verify

每轮创建一个 Captcha RPCClient，只发送一次真实 Init 并绑定成功签发的 `CertifyId`。Puzzle 的 Verify 尝试位在网络前消耗，POST 禁止 `GetBody` 回卷；网络结果未知也不补发。Device RPC 的一次性动作同样禁止已发送请求的透明 body 重放。

TRACELESS/SLIDING 桥把 SDK Init 绑定到已有响应，仅接受符合顺序的唯一 Verify；success、Verify 响应和场景/挑战/token 绑定关系都需复核。应用对返回的 `Verification` 再检查本轮挑战。Verify 前失败停止，Verify 后合同失败拒绝结果，也不发送第二次 Verify。

保证范围是单轮生产状态机，不是无限的跨进程历史 ID 数据库。调用方与外层系统也不能自动重试结果未知的挑战。

## 验证与残余限制

[开发指南](../guides/development.md) 描述离线、架构、真实 ABI、平台包和安全合同检查。CI 不执行线上挑战，工具下载与漏洞库查询和项目测试网络范围分开。Windows 包检查哈希与文件白名单，但尚未配置 Authenticode 签名；哈希不证明发布者身份。

[2026-08-09 历史性能证据](../archive/evidence/validation-2026-08-09-performance.md) 记录过多个同时存活 VM 的字段合同异常。它属于当时的实验和候选，当前逐轮冷建且没有本地 live 上限，仍需在实际运行形态下验证。历史成功率、离线并发测试或目录依赖检查都不能证明生产容量与长时间稳定性，见 [性能说明](performance.md)。
