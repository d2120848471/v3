# 配置说明

> **当前状态**：`cmd/server` 已将 `internal/config.Parse` 接入公共 Client、HTTP Handler、设备预热、artifact 清理与优雅关闭。本页参数是当前可执行服务的真实合同。HTTP surface 仍只有 `/`、`/api/slider`、`/health`、`/openapi.json` 四个路径；Solve 同时接受 POST JSON 和 deprecated GET query。Handler 不设置本地并发准入或主动 429：每个通过浏览器跨源和 JSON/query/字段校验的 Solve 请求都会进入 `Solve`。

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
| `MaxConcurrency` | `--max-concurrency` | `ALI_SLIDER_MAX_CONCURRENCY` | `32` | `1..32`；兼容旧名，只控制 Client 每个 route/host 的出站连接数与 PE 资源预算，不限制 HTTP 在途请求数 |
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
| `DevicePrewarmCapacity` | `--device-prewarm` | `ALI_SLIDER_DEVICE_PREWARM` | `4` | `0..min(MaxConcurrency,4)`；`0` 显式关闭；启动时为默认直连配置建立独立画像的 Device slot |
| `DeviceSessionReserve` | `--device-reserve` | `ALI_SLIDER_DEVICE_RESERVE` | `0` | `0..4`；非0时要求prewarm非0，且两者之和不超过4；默认关闭 |
| `V8RuntimeLibrary` | `--v8-library` | `ALI_SLIDER_V8_LIBRARY` | 可执行文件同目录的平台默认文件 | 1–4096 个有效 UTF-8 字节且不含 NUL；Linux 通常为 `libali_slider_v8_runtime.so`，Windows 为 `ali_slider_v8_runtime.dll` |

Artifact Store 另有不可放大的安全上限：默认最多 64 组、总计 512 MiB。超限时按受管组的最旧时间淘汰，不删除非受管文件或符号链接。

`V8RuntimeLibrary` 只选择 wrapper，不控制缓存周期。KeyResolver 每 5 分钟重下公开 SDK 并做字节比对：SDK 未变时延长所有已验证精确 `StaticPath` 的软 TTL，SDK 变更时立即清空画像；无论 SDK 是否改变，每个 PE 分片最多 30 分钟必须重下并重做一次 V8 差分。

未知/到期分片会先使用当前 SDK + 精确 PE 在隔离 V8 context 中生成假挑战 data，再与纯 Go Builder 解包后逐字段差分。只有 payload、轨迹事件和 Complete 延迟都一致时，该精确分片才在 TTL 内走纯 Go；不一致或采样失败时保留原 V8 Build。因此 key/schema 不是全局写死，也不缓存挑战级 `CertifyId`、DeviceToken、轨迹或 `data`。

每轮挑战仍新建或租用短龄 V8 Device 会话，并在 Log1/Log2/Log3 到 Complete 期间保持同一 FeiLin 状态。每个 live slot 有独立画像；当轮 RPC headers、图片、PE 和 Device VM 使用租到会话的实际画像。消费后只复用外层 Isolate，下一轮仍重建独立浏览器 context 和新 session/token。默认直连池最多同时保留4个live slot；满池时等待已有槽位 recycle，不额外冷建同池会话。代理或非默认 prefix 不跨池复用，改走本轮冷会话。预热设备会话默认最多空闲20秒且一次性消费。Docker和Windows便携包已携带对应动态库；裸二进制部署需将wrapper放在可执行文件同目录，或使用绝对路径显式配置。

## 逐请求 Solve 参数

新调用应使用 `POST /api/slider` 的 JSON object；`GET /api/slider?...` 仅为兼容旧 Python 客户端而保留，OpenAPI 标记为 deprecated。内嵌测试页始终发送同源 POST JSON。

| 标准字段 | 兼容别名 | 空值结果 | 边界 |
|---|---|---|---|
| `SceneId` | `sceneId` | 使用服务 `SceneID` 默认值 | 最多 64 个 Unicode 字符 |
| `prefix` | `Prefix` | 使用服务 `Prefix` 默认值 | 1–32 个 ASCII 字母数字 |
| `AaduaneId` | `aaduaneId` | 不做逐请求 RPC key 覆盖 | 最多 128 个 Unicode 字符 |
| `proxy` | `Proxy` | 直连 | 受支持的代理 origin |

POST JSON body 与 GET 的 raw query 分别有 `64 KiB` 上限。两种输入使用同一字段规则：同时出现 canonical 和 alias 时，canonical 只要存在就优先；POST JSON 的 canonical 为 `null`，或任一方式的 canonical 为空串/纯空白时，也不会回退到 alias，而是按上表使用默认值。GET 中同一精确键重复时取最后一个值，未知字段忽略；`?SceneId=null` 的 `null` 是普通字符串，不是 JSON null。

上限在真实 HTTP 入口也可达：`server.MaxRequestBytes = 64 KiB`，cmd 的 `maxHeaderBytes = server.MaxRequestBytes + 32 KiB`，额外预算只用于 request line 和普通 header。恰好 64 KiB raw query 不会在进入 Handler 前被 `net/http.Server` 拒绝，超过 64 KiB 仍由 Handler 返回 400。

参数源不合并：GET 只读 query，POST 只读 JSON body，POST URL 的 query 不会填充 body，GET body 也不参与。Solve 不支持 `application/x-www-form-urlencoded` 或 multipart form body。两种方法的应用响应都只有 `200/400/403/500`，不会因本地在途数返回 429。

旧 GET 会执行真实求解，不能用作 health、可预取链接或缓存读取。URL 可能进入历史、access log 和复制记录；禁止在 URL 放入 `AaduaneId` 或含 userinfo 的 proxy。若旧客户端只需覆盖非敏感场景，最小示例为：

```text
http://127.0.0.1:8000/api/slider?SceneId=1ug4aptr
```

### 兼容资源预算与设备预热收敛

`MaxConcurrency` 是为了保持现有配置兼容而保留的字段名，不是 HTTP admission 开关。它会传入 Client 的 Transport，作为每个 route/host 的出站连接上限，同时决定 PE runtime 资源预算。`DevicePrewarmCapacity` 还受独立池库存硬上限4约束。`DeviceSessionReserve` 只在有租约时投机补货，但与prewarm之和也不能超过4；实测reserve会增加争用，所以默认为0。

如果调用方降低 `MaxConcurrency`，且没有显式设置 `ALI_SLIDER_DEVICE_PREWARM` 或 `--device-prewarm`，`Parse` 会把默认预热容量自动压到新的资源预算。

例如仅设置：

```bash
export ALI_SLIDER_MAX_CONCURRENCY=8
```

解析结果为 `MaxConcurrency=8`、`DevicePrewarmCapacity=4`。

如果显式设置了预热容量，则不自动改写；超过资源预算会在 `Validate` 阶段报错：

```bash
export ALI_SLIDER_MAX_CONCURRENCY=8
export ALI_SLIDER_DEVICE_PREWARM=5
```

该耦合只限制连接/PE预算与预热库存，不拒绝合法HTTP请求。资源预算较小时，更多默认直连并发Solve可能在Transport或4槽Device池处等待，但请求已经进入Solver。

## 启动与覆盖示例

直接启动默认回环服务：

```bash
go run ./cmd/server
```

浏览器测试页使用当前监听地址的根路径，例如 `http://127.0.0.1:8000/`。它通过相对路径检查同源 `/health`，并以 POST JSON 调用同源 `/api/slider`；修改 `Host`/`Port` 后只需打开新的根地址，不存在独立页面配置。页面等待上限是浏览器单次操作参数，不修改服务端 `Timeout`。

Windows 便携包不需要配置文件。完整解压后双击 `start.bat`；脚本先切换到自身目录，再提供回环地址、端口、包内 Artifact 路径和包内 `ali_slider_v8_runtime.dll` 的安全默认值。缺少 DLL 时脚本会在启动前停止。高级用户可在 `cmd.exe` 追加 flag，后出现的值覆盖脚本默认值：

```bat
start.bat --port=8001 --device-prewarm=4
```

正常便携版保留默认预热容量4；默认直连公开组件A/B已证明5个对齐存活的Device VM会出现字段合同错误，因此不能提高该池上限。CI的本地启动smoke使用 `--device-prewarm=0`，只GET测试页/health/OpenAPI，并提交必定在Solver前失败的跨源POST/GET和非法JSON/query，不发送真实外部请求。完整交付说明见 [Windows AMD64 便携包](./windows.md)。

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

- 将 `MaxConcurrency` 记录为每 route/host 的连接与PE资源预算；默认Device预热池库存另有硬上限4。不要把任何一个解释为HTTP admission上限。
- 将纯计算耗时与外部网络耗时分开记录。
- `Timeout` 是失败边界，不是性能目标；不能通过放大超时来证明 `P95 ≤ 1s`。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/config/config.go` · `MaxDevicePrewarmCapacity` / `Validate` | `MaxConcurrency` 保持 `1..32` 作为连接/PE预算；Device prewarm默认和硬上限为4，且prewarm+reserve不超过4。 | process environment + args → typed resource budgets → Validate |
| `internal/config/config.go` · `Parse` | 只在预热未显式配置时随更小的 MaxConcurrency 收敛；显式值仍必须满足 `0..min(MaxConcurrency,4)`。 | max-concurrency override → implicit prewarm clamp |
| `cmd/server/main.go` · `clientOptions` / `run` / `maxHeaderBytes`；`cmd/server/main_test.go` · `TestHeaderBudgetAcceptsMaximumLegacyQuery` | 完整配置映射到 Client 和 Handler，显式零值不会被二次默认化吞掉；真实 HTTP 入口可承载恰好 64 KiB raw query。 | Config + request limits → HTTP/Client options → validated runtime |
| `internal/config/config.go` · `V8RuntimeLibrary`；`pkg/slider/client.go` · `CheckRuntime` | 动态库路径按 flag/env/default 解析；服务在预热和 ready 之前校验文件、C ABI 与 V8/ICU 初始化。 | config → purego load → ABI/version check → ready |
| `internal/server/server.go` · `Handler` / `checkSolveOrigin` / `decodeQueryRequest` / `requestFromPayload` / `handleSolve`；`internal/server/openapi.go` · `legacyQueryParameters` / `solveResponses` | POST JSON 和 deprecated GET query 使用共享字段合同、分离参数源；明确浏览器跨源在 Solver 前返回 403，其他合法请求直接进入 Solver，两种方法只声明 200/400/403/500。 | method/source → origin + JSON/query gate → timeout context → Solve |
| `internal/artifact/store.go` · `SaveFailure` / `Purge` | 保留期、组数和总字节硬配额同时执行。 | failure set → purge expired → evict oldest → O_EXCL write |
