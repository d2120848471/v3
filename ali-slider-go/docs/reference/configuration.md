# 配置说明

配置由 [bootstrap/config](../../internal/bootstrap/config) 解析，再由 `bootstrap/server.Run` 映射到共享应用 service 与 HTTP Handler。`cmd/server` 只提供进程入口。参数、默认值及 HTTP/SDK 兼容行为保持不变。

## 配置优先级

配置按以下顺序覆盖，左侧优先级最高：

```text
命令行 flag > ALI_SLIDER_* 环境变量 > 内置默认值
```

环境变量的首尾空白会被去除；空字符串视为未设置。Go duration 使用 `time.ParseDuration` 语法，例如 `750ms`、`25s`、`2m`、`168h`。

## 完整配置表

| Config 字段 | 命令行 flag | 环境变量 | 默认值 | 约束与用途 |
|---|---|---|---|---|
| `Host` | `--host` | `ALI_SLIDER_HOST` | `127.0.0.1` | 必须是 IP 地址或 `localhost`；`0.0.0.0` 会监听全部 IPv4 网卡 |
| `Port` | `--port` | `ALI_SLIDER_PORT` | `8000` | `1..65535` |
| `MaxConcurrency` | `--max-concurrency` | `ALI_SLIDER_MAX_CONCURRENCY` | `32` | `1..32`；兼容旧名，控制各路由 Transport 的每 host 连接上限与 PE 空闲引擎保留量；不限制活跃 PE、Device 会话或 HTTP 在途请求数 |
| `Timeout` | `--timeout` | `ALI_SLIDER_TIMEOUT` | `25s` | 大于 0 且不超过 `5m`；作为单轮总超时合同 |
| `MinimumConfidence` | `--min-confidence` | `ALI_SLIDER_MIN_CONFIDENCE` | `0.45` | `0..1`；具体 Solver 必须在低于阈值时停止且不 Verify |
| `SceneID` | `--scene-id` | `ALI_SLIDER_SCENE_ID` | `1ug4aptr` | 非空，最多 64 个 Unicode 字符；逐请求 `SceneId` 可覆盖 |
| `Prefix` | `--prefix` | `ALI_SLIDER_PREFIX` | `fsgtmi` | 1–32 个 ASCII 字母数字；逐请求 `prefix` 可覆盖 |
| `GatherCostMin` | `--gather-cost-min` | `ALI_SLIDER_GATHER_COST_MIN` | `180` | 必须 `≥ 0` 且不大于 `GatherCostMax`，单位 ms |
| `GatherCostMax` | `--gather-cost-max` | `ALI_SLIDER_GATHER_COST_MAX` | `260` | 必须不小于 `GatherCostMin`，单位 ms |
| `FirstTouchAgeMin` | `--first-touch-age-min` | `ALI_SLIDER_FIRST_TOUCH_AGE_MIN` | `650` | 必须 `≥ 1` 且不大于 `FirstTouchAgeMax`，单位 ms |
| `FirstTouchAgeMax` | `--first-touch-age-max` | `ALI_SLIDER_FIRST_TOUCH_AGE_MAX` | `850` | 必须不小于 `FirstTouchAgeMin`，单位 ms |
| `ArtifactDir` | `--artifact-dir` | `ALI_SLIDER_ARTIFACT_DIR` | `var/artifacts` | 非空；用于脱敏失败/低置信样本 |
| `ArtifactRetention` | `--artifact-retention` | `ALI_SLIDER_ARTIFACT_RETENTION` | `168h`（7 天） | 必须大于 0；启动时清理一次，运行时每小时清理 |
| `AssetMaxBytes` | `--asset-max-bytes` | `ALI_SLIDER_ASSET_MAX_BYTES` | `8388608`（8 MiB） | `1..67108864`；单张下载和视觉输入字节上限 |
| `AssetMaxDimension` | `--asset-max-dimension` | `ALI_SLIDER_ASSET_MAX_DIMENSION` | `16384` | `1..16384`；视觉 PNG 宽高上限 |
| `DevicePrewarmCapacity` | `--device-prewarm` | `ALI_SLIDER_DEVICE_PREWARM` | `0` | 升级兼容参数，唯一有效值为 `0`；非 0 值启动失败，生产路径不会预热或复用 Device 会话 |
| `DeviceSessionReserve` | `--device-reserve` | `ALI_SLIDER_DEVICE_RESERVE` | `0` | 升级兼容参数，唯一有效值为 `0`；非 0 值启动失败，不存在备用会话库存 |
| `V8RuntimeLibrary` | `--v8-library` | `ALI_SLIDER_V8_LIBRARY` | 可执行文件同目录的平台默认文件 | 1–4096 个有效 UTF-8 字节且不含 NUL；Linux 通常为 `libali_slider_v8_runtime.so`，Windows 为 `ali_slider_v8_runtime.dll` |

Artifact Store 另有不可放大的安全上限：默认最多 64 组、总计 512 MiB。超限时按受管组的最旧时间淘汰，不删除非受管文件或符号链接。

`V8RuntimeLibrary` 只选择 wrapper，不控制缓存周期。`engine.KeyResolver` 在后续访问时按 5 分钟软 TTL 复核公开 SDK 并做字节比对：SDK 未变时延长所有已验证精确 `StaticPath` 的软 TTL，SDK 变更时立即清空画像；无论 SDK 是否改变，每个 PE 分片最多 30 分钟必须重下并重做一次 V8 差分。

未知/到期分片会先使用当前 SDK + 精确 PE 在隔离 V8 context 中生成假挑战 data，再与纯 Go Builder 解包后逐字段差分。只有 payload、轨迹事件和 Complete 延迟都一致时，该精确分片才在 TTL 内走纯 Go；不一致或采样失败时保留原 V8 Build。因此 key/schema 不是全局写死，也不缓存挑战级 `CertifyId`、DeviceToken、轨迹或 `data`。

每轮挑战都立即冷建独立 V8 Device 会话，并在 Log1/Log2/Log3 到 Complete 期间保持同一 FeiLin 状态；当轮 RPC headers、图片、PE 和 Device VM 使用该轮独立画像。Solve 成功、失败或取消后都会关闭该 Device/V8 会话，不保留 ready、reserve、recycle 或 idle 会话。进程级 KeyResolver 仍复用不含挑战态的公开 SDK、精确 PE 源码和已验证结构画像。本地不设置 `OpenDevice` 到 `Close` 的 live 会话容量或等待槽；Docker和Windows便携包已携带对应动态库，裸二进制部署需将wrapper放在可执行文件同目录，或使用绝对路径显式配置。

## 逐请求 Solve 参数

新调用应使用 `POST /api/slider` 的 JSON object；`GET /api/slider?...` 仅为兼容旧 Python 客户端而保留，OpenAPI 标记为 deprecated。内嵌测试页始终发送同源 POST JSON。

| 标准字段 | 兼容别名 | 空值结果 | 边界 |
|---|---|---|---|
| `SceneId` | `sceneId` | 使用服务 `SceneID` 默认值 | 最多 64 个 Unicode 字符 |
| `prefix` | `Prefix` | 使用服务 `Prefix` 默认值 | 1–32 个 ASCII 字母数字 |
| `AaduaneId` | `aaduaneId` | 不做逐请求 RPC key 覆盖 | 最多 128 个 Unicode 字符 |
| `proxy` | `Proxy` | 直连 | 受支持的代理 origin |

POST JSON body 与 GET 的 raw query 分别有 `64 KiB` 上限。两种输入使用同一字段规则：同时出现 canonical 和 alias 时，canonical 只要存在就优先；POST JSON 的 canonical 为 `null`，或任一方式的 canonical 为空串/纯空白时，也不会回退到 alias，而是按上表使用默认值。GET 中同一精确键重复时取最后一个值，未知字段忽略；`?SceneId=null` 的 `null` 是普通字符串，不是 JSON null。

上限在真实 HTTP 入口也可达：`httpapi.MaxRequestBytes = 64 KiB`，bootstrap/server 的 HTTP header 预算另加 32 KiB，额外预算只用于 request line 和普通 header。恰好 64 KiB raw query 不会在进入 Handler 前被 `net/http.Server` 拒绝，超过 64 KiB 仍由 Handler 返回 400。

参数源不合并：GET 只读 query，POST 只读 JSON body，POST URL 的 query 不会填充 body，GET body 也不参与。Solve 不支持 `application/x-www-form-urlencoded` 或 multipart form body。两种方法的应用响应都只有 `200/400/403/500`，不会因本地在途数返回 429。

旧 GET 会执行真实求解，不能用作 health、可预取链接或缓存读取。URL 可能进入历史、access log 和复制记录；禁止在 URL 放入 `AaduaneId` 或含 userinfo 的 proxy。若旧客户端只需覆盖非敏感场景，最小示例为：

```text
http://127.0.0.1:8000/api/slider?SceneId=1ug4aptr
```

### 兼容资源预算

`MaxConcurrency` 保留旧配置名称，传给各路由 Transport 的 `MaxConnsPerHost`，并限定 PE 任务完成后可保留的空闲 Isolate 数量。没有可用空闲 PE 引擎时会新建引擎，不等待该保留额度。每个合法 Solve 都独立冷建 Device 会话，直到 `Close` 结束；HTTP 在途请求、Device 会话和活跃 PE 调用均不受此值限制。

例如设置：

```bash
export ALI_SLIDER_MAX_CONCURRENCY=8
```

解析结果为 `MaxConcurrency=8`，表示每个出站 Transport 的每 host 连接上限为 8，KeyResolver 最多保留 8 个空闲 PE 引擎；活跃 PE 调用与 Device 会话没有本地容量上限。

旧启动脚本可继续显式传入零值：

```bash
export ALI_SLIDER_DEVICE_PREWARM=0
export ALI_SLIDER_DEVICE_RESERVE=0
```

任一旧参数为非零值都会在 `Validate` 阶段报错，不会恢复旧会话池。当前没有 Device 会话容量或等待槽，合法 HTTP 请求直接进入 Solver。

## 启动与覆盖示例

在 module 根目录指定当前平台 wrapper 后启动默认回环服务（下例为 Linux）：

```bash
CGO_ENABLED=0 \
  ALI_SLIDER_V8_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.so" \
  go run ./cmd/server
```

浏览器测试页使用当前监听地址的根路径，例如 `http://127.0.0.1:8000/`。它通过相对路径检查同源 `/health`，并以 POST JSON 调用同源 `/api/slider`；修改 `Host`/`Port` 后只需打开新的根地址，不存在独立页面配置。页面等待上限是浏览器单次操作参数，不修改服务端 `Timeout`。

Windows 便携包不需要配置文件。完整解压后双击 `start.bat`；脚本先切换到自身目录，再提供回环地址、端口、包内 Artifact 路径和包内 `ali_slider_v8_runtime.dll` 的安全默认值。缺少 DLL 时脚本会在启动前停止。高级用户可在 `cmd.exe` 追加 flag，后出现的值覆盖脚本默认值：

```bat
start.bat --port=8001
```

正常便携版不做 Device 预热；每次手工 Solve 都冷建并关闭独立 Device/V8 会话。CI 的本地启动 smoke 可继续传兼容值 `--device-prewarm=0`，只 GET 测试页/health/OpenAPI，并提交必定在 Solver 前失败的跨源 POST/GET 和非法 JSON/query，不发送真实外部请求。完整交付说明见 [Windows AMD64 便携包](../guides/windows.md)。

环境变量示例：

```bash
export ALI_SLIDER_HOST=127.0.0.1
export ALI_SLIDER_PORT=8000
export ALI_SLIDER_MAX_CONCURRENCY=16
export ALI_SLIDER_TIMEOUT=25s
export ALI_SLIDER_MIN_CONFIDENCE=0.45
export ALI_SLIDER_ARTIFACT_RETENTION=168h
export ALI_SLIDER_V8_LIBRARY=/opt/ali-slider/libali_slider_v8_runtime.so
```

对应 flag 等价，且优先级更高：

```text
--host 127.0.0.1
--port 8000
--max-concurrency 16
--timeout 25s
--min-confidence 0.45
--artifact-retention 168h
--v8-library /opt/ali-slider/libali_slider_v8_runtime.so
```

## 配置决策建议

### 本机开发

- 保持 `Host=127.0.0.1`。
- 使用 Mock Solver 跑 HTTP 和并发测试。
- 不在普通测试中发送真实 Init 或 Verify。

### 容器或反向代理

- 只有在网络策略已限制来源时才设置 `Host=0.0.0.0`。
- 在服务外层部署鉴权、TLS、访问控制和请求速率限制。
- 外层反向代理、负载均衡或 API 网关仍可在请求到达本进程前返回 429；这是外层策略，不是本服务的本地 admission。
- 第三方上游也可能在请求进入 Solve 后返回 429；Solver 会按上游错误处理，不能把它统计为本地 HTTP 主动限流。
- 不要把代理参数、Solve query 或响应体写入访问日志；网关应优先禁用 deprecated GET。

### 性能验收

- 将 `MaxConcurrency` 记录为出站 Transport 每 host 连接上限与 PE 空闲引擎保留量；它不是 Device、活跃 PE 或 HTTP 请求上限。
- 将纯计算耗时与外部网络耗时分开记录。
- `Timeout` 是失败边界，不是性能目标；不能通过放大超时来证明 `P95 ≤ 1s`。

## 配置与生命周期边界

服务配置、应用选项与 SDK 选项是不同边界：`bootstrap/config.Config` 包含监听地址与清理调度配置；应用 service 管理共享资源的生命周期；`slider.ClientOptions` 保留公开字段并映射到生产组装。内部类型不承诺稳定，公共字段以 [SDK 源码](../../pkg/slider/options.go) 为准。

`NewClient` 与 HTTP 启动器都通过 `bootstrap.NewService` 创建同类应用 service。HTTP 启动器会调用本地运行时检查并调度清理；SDK 宿主需自行调用 `CheckRuntime` 与 `PurgeArtifacts`。具体所有权见 [架构](../design/architecture.md#跨请求生命周期)。
