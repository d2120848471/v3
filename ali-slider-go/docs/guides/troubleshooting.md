# 故障排查

先区分本地启动、HTTP 输入、应用阶段和上游业务结果。运行时使用同进程 V8；每轮 Device 会话冷建并关闭，公开 SDK/PE profile 可缓存。`/health` 不调用上游，正常响应不能证明 Solve 成功率或完整链延迟。

## 先检查本地状态

从 module 根目录运行：

```bash
go version
make fmt-check
make vet
make test
```

这些是离线检查。真实 V8 测试、Darwin race 和固定工具版本见 [开发指南](development.md)。启动命令见 [启动指南](getting-started.md)，出现 `event=listen status=ready` 后可读 health：

```bash
curl --fail --silent --show-error http://127.0.0.1:8000/health
```

浏览器根页只自动检查 health。`GET /api/slider?...` 会发出真实 Solve，不能作为普通诊断或健康检查地址。

记录 commit、Go/OS/arch、HTTP 状态、X-Trace-ID、稳定错误类别/阶段和客户端墙钟即可。不要收集完整请求/响应、token、CertifyId、RPC key、代理凭据或上游原文。

## 启动与关闭

| 现象 | 检查与处理 |
|---|---|
| `配置无效` | 对照 [配置](../reference/configuration.md)；旧 `device-prewarm`/`device-reserve` 只接受 0 |
| `address already in use` | 端口被占用；换端口或停止已确认的旧实例。Unix 可查 `lsof -nP -iTCP:8000 -sTCP:LISTEN`，Windows 可查 `Get-NetTCPConnection -LocalPort 8000` |
| `检查内嵌 V8` 后退出 | 检查 wrapper 路径、系统/架构、C ABI 与 V8/ICU；源码 `go run` 必须显式指定动态库路径，不跳过自检 |
| ready 前等待较长 | bootstrap 已绑定端口，仍可能在本地运行时检查或 artifact 清理；启动不执行 Device Log1/2/3 |
| `event=artifact_purge status=warning` | 检查目录权限/ACL、可写性与磁盘；HTTP 宿主记录错误但保持服务启动 |
| 非回环 `security_warning` | 当前没有鉴权；确认外层访问控制，或恢复回环监听 |
| 关闭等待较长 | service 等待活动 Solve/CheckRuntime/Prime/Purge 返回；`Close` 不主动取消 Solve，需由调用方 context/timeout 收束 |
| `/health` ready 但 Solve 失败 | 按下面的 errorType 与阶段定位，不用 health 结果推断上游状态 |

`cmd/server` 是薄入口，配置、listener、启动自检和清理调度在 `bootstrap/server.Run`。SDK 宿主自行调度 `PurgeArtifacts`，HTTP 宿主在启动和每小时调度。

## HTTP 状态与输入

| 现象 | 含义与处理 |
|---|---|
| 根页打不开 | 核对 ready、实际监听端口与进程；使用根路径 `/` |
| `400 ApiRequestError` | JSON/query 编码、大小、字段类型/长度、prefix 或 proxy 无效；错误发生在 Solver 前 |
| `400 InvalidRequest` | 应用拒绝请求，检查实际传入的 SDK/HTTP 参数 |
| `403 ApiOriginError` | 明确的浏览器跨源请求被拒绝；使用服务自带同源页面，不放宽 CORS |
| HTTP `200`、`ok=false` | 完成态业务拒绝；读取 VerifyCode/VerifyResult，不据此自动重试 |
| `500 NetworkError` | 网络、非 2xx、响应读取或超时；用稳定阶段定位，未知 Verify 结果不重试 |
| `500 VisionError` | PNG、尺寸、识别或低置信；查看受控失败样本与离线视觉回归 |
| `500 ProtocolError` | 上游 schema、动态分片、token/session/getter 或结果绑定不一致；使用对应离线回归 |
| `500 InternalError` | 本地 runtime/host/资源失败；检查本地 ABI 和取消/heap/time 边界 |
| `500 UnhandledProtocolError` | 未分类错误或 panic，响应已脱敏；用 Mock 最小复现，不打印原始 cause |
| 外层返回 `429` | 先确认响应来自网关还是上游；Handler 不按本机在途数生成 429/Retry-After |
| 自定义宿主没有 HTTP 日志 | 检查 `httpapi.New` 的 Logger；默认 bootstrap 已提供安全 logger |

POST 只读 JSON body，GET 只读 query，两者不合并，也不支持 form body。body/raw query 各限 64 KiB；query 同名键取最后值，规范字段只要存在就压过别名，空规范字段按未传处理。完整示例和别名以 [HTTP API](../reference/http-api.md) 为准。

Solve 的 200/400/403/500 响应应带与 body 一致的 X-Trace-ID。HTML/JSON 应有 no-store、nosniff、no-referrer，根页另有随机 nonce CSP；缺失时检查路由、构建及代理改写。

## 应用阶段

SDK 可以从 `*slider.Error` 读取稳定 Stage；HTTP 不公开底层 cause。`timingsMs` 包含固定阶段键，不执行的分支为 0。

| 阶段 | 优先检查 |
|---|---|
| `setup` | 本轮 route、画像、请求与适配器构造 |
| `deviceSession` | wrapper/ABI、系统资源、Device 出口与协议；每轮都承担冷建成本 |
| `init` | 场景、prefix、签名和 Captcha RPC |
| `resolvePEKey` | 精确 StaticPath、公开脚本 allowlist、缓存变化、V8 采样与差分 |
| `downloadAssets` | 相对路径、HTTPS 重定向、体积与双图取消 |
| `vision` | PNG/alpha/尺寸、置信度与 edge-decoy fixture |
| `buildVerifyData` | 轨迹/坐标、DeviceConfig、逻辑时钟、data 自检、V8 fallback |
| `completeDevice` | 同一 Device VM 的 getter、事件、token/session 和最终动作顺序 |
| `traceless`、`sliding` | 官方组件资源、桥接合同、唯一 Init/Verify 与 success 绑定 |
| `verify` | 唯一 Verify 的网络/响应；尝试位消耗后不重试 |
| `clientCleanup` | 本轮 Device Close 与取消竞态，不存在回池/补货 |

对 PE/profile 的缓存 miss，先确认宿主是否错误地逐请求重建 Client；再区分 SDK 字节变化、5 分钟软 TTL、30 分钟硬 TTL 和采样不兼容。不要通过延长缓存或保存挑战级 token/data 来掩盖变化。

## 运行时与离线回归

仅构建 Go 二进制不足以运行动态设备/PE。Linux 使用 `.so` 且需要 glibc，macOS 为 `.dylib`，Windows 为匹配 EXE 架构的 `.dll`。Linux 用 `file` 与 `ldd` 检查自己构建或可信来源的库，Windows 先确认包完整。

当前平台真库测试示例（Linux）：

```bash
CGO_ENABLED=0 \
  ALI_SLIDER_V8_TEST_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.so" \
  go test -count=1 ./internal/platform/v8runtime ./internal/infrastructure/engine
```

局部离线链：

```bash
go test -count=1 \
  ./internal/domain/pe ./internal/domain/vision \
  ./internal/application/solve ./internal/infrastructure/aliyun \
  ./internal/infrastructure/aliyun/device
```

没有提供真库时相关用例会跳过；未执行的平台与 ABI 不能写成通过。普通排障不自动运行在线探针或历史挑战批次。

## 网络、会话与资源

| 现象 | 检查与处理 |
|---|---|
| 代理被拒绝 | 只支持 HTTP(S)/SOCKS4/5/5H；使用纯代理 origin，无业务 path/query/fragment，凭据不输出 |
| socks5/socks5h 行为不同 | 前者本地 DNS，后者代理侧 DNS；按出口要求选择 |
| `transport route capacity exceeded` | 路由容量和不可淘汰直连的边界；正常 proxy churn 应做 LRU 淘汰 |
| 直连受环境代理影响 | 生产直连明确禁用环境 proxy；检查自定义 Transport 是否改写 |
| 调小 MaxConcurrency 后仍有很多 Device 会话 | 它只改变连接和 PE 空闲预算；HTTP/Device/活跃 PE 没有本地上限 |
| 并发升高后字段合同异常或资源增长 | 先记录阶段和资源峰值，由外层降低来流并受控复验；不能用旧预热池样本证明当前容量 |
| `Client.Prime` 不产生流量 | 兼容空操作，返回 nil 不表示 Device 已预热 |
| 结束后 Device 未关闭 | 查 Close/context 与 `clientCleanup`；成功、失败、取消都应释放本轮会话 |
| Log1 通过但 Log2/3 报 ResultObject 错误 | 兼容 Device RPC 只在 Log1 解码对象配置，Log2/3 仅校验成功 Code；确认没有恢复共用对象 schema |

同一轮 Device、Captcha、图片和脚本沿用同一路由；不同路由不共享 TCP/TLS 连接。V8 与 Go 同进程，活跃 Isolate、内存、线程和网络等待均可随来流增长。

## 图片、Artifact 与性能

图片路径或重定向越界、超体积、PNG/alpha 异常、低置信和 PE 自检失败都应停止后续 Verify。不要放宽边界或生成随机 fallback。`Verify already attempted` 表示尝试位已消耗，不允许对原挑战重发。

成功路径不保存 artifact；早期失败可能只有 metrics。缺失文件时检查是否尚未取得图像、目录/ACL、磁盘或 best-effort 写入失败。服务未运行或 SDK 未调度时不会按小时清理；Store 不删除非受管文件和符号链接，不用递归删除替代 Purge。

性能分析先分开视觉、纯计算、Solver total 与 HTTP 端到端墙钟。benchmark 的 ns/op 是均值，RunParallel 反映吞吐，不代表尾延迟；历史分位数也不能代替当前冷建路径。可复现命令和已有测量范围见 [性能说明](../design/performance.md)。

## 构建与平台

Docker 命令必须指定 `-f build/docker/Dockerfile` 并保持 module 根 context；容器内若需端口映射，显式设 `ALI_SLIDER_HOST=0.0.0.0`，宿主优先只发布到回环。运行层为 Debian/glibc，不能按 Alpine/scratch 单文件说明部署。

Windows ZIP 素材已迁到 `build/packaging/windows`，最终包仍把 EXE、DLL、start.bat 和说明置于根目录。没有 ZIP 时检查是否为 PR、前置 job 失败或 artifact 过期；未知发布者与 SHA-256 边界见 [Windows 指南](windows.md)。跨平台编译不能代替 Windows 原生执行。
