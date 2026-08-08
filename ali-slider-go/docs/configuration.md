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
| `MaxConcurrency` | `--max-concurrency` | `ALI_SLIDER_MAX_CONCURRENCY` | `32` | `1..32`；兼容旧名，只控制 Client 每个 route/host 的出站连接数与预热资源预算，不限制 HTTP 在途请求数 |
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
| `DevicePrewarmCapacity` | `--device-prewarm` | `ALI_SLIDER_DEVICE_PREWARM` | `32` | `0..MaxConcurrency`；`0` 显式关闭；启动时预热匹配默认直连配置的一次性 Device Session |

Artifact Store 另有不可放大的安全上限：默认最多 64 组、总计 512 MiB。超限时按受管组的最旧时间淘汰，不删除非受管文件或符号链接。

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

`MaxConcurrency` 是为了保持现有配置兼容而保留的字段名，不是 HTTP admission 开关。它会传入 Client 的 Transport，作为每个 route/host 的出站连接上限；同时也是 `DevicePrewarmCapacity` 的校验上界。

如果调用方降低 `MaxConcurrency`，且没有显式设置 `ALI_SLIDER_DEVICE_PREWARM` 或 `--device-prewarm`，`Parse` 会把默认预热容量自动压到新的资源预算。

例如仅设置：

```bash
export ALI_SLIDER_MAX_CONCURRENCY=8
```

解析结果为 `MaxConcurrency=8`、`DevicePrewarmCapacity=8`。

如果显式设置了预热容量，则不自动改写；超过资源预算会在 `Validate` 阶段报错：

```bash
export ALI_SLIDER_MAX_CONCURRENCY=8
export ALI_SLIDER_DEVICE_PREWARM=16
```

该耦合只限制连接与预热库存，不拒绝合法 HTTP 请求。资源预算较小时，更多并发 Solve 可能在 Transport 获取连接或设备冷建阶段等待，但请求已经进入 Solver。

## 启动与覆盖示例

直接启动默认回环服务：

```bash
go run ./cmd/server
```

浏览器测试页使用当前监听地址的根路径，例如 `http://127.0.0.1:8000/`。它通过相对路径检查同源 `/health`，并以 POST JSON 调用同源 `/api/slider`；修改 `Host`/`Port` 后只需打开新的根地址，不存在独立页面配置。页面等待上限是浏览器单次操作参数，不修改服务端 `Timeout`。

Windows 便携包不需要配置文件。完整解压后双击 `start.bat`；脚本先切换到自身目录，再提供回环地址、端口和包内 Artifact 路径的安全默认值。高级用户可在 `cmd.exe` 追加 flag，后出现的值覆盖脚本默认值：

```bat
start.bat --port=8001 --device-prewarm=16
```

正常便携版保留默认预热容量 32，以降低后续请求时延。CI 的本地启动 smoke 使用 `--device-prewarm=0`，只 GET 测试页/health/OpenAPI，并提交必定在 Solver 前失败的跨源 POST/GET 和非法 JSON/query，不发送真实外部请求。完整交付说明见 [Windows AMD64 便携包](./windows.md)。

环境变量示例：

```bash
export ALI_SLIDER_HOST=127.0.0.1
export ALI_SLIDER_PORT=8000
export ALI_SLIDER_MAX_CONCURRENCY=16
export ALI_SLIDER_TIMEOUT=25s
export ALI_SLIDER_MIN_CONFIDENCE=0.45
export ALI_SLIDER_ARTIFACT_RETENTION=168h
```

对应 flag 等价，且优先级更高：

```text
--host 127.0.0.1
--port 8000
--max-concurrency 16
--timeout 25s
--min-confidence 0.45
--artifact-retention 168h
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

- 将 `MaxConcurrency` 记录为每 route/host 的连接与预热资源预算；不要把它解释为 HTTP 并发上限。
- 将纯计算耗时与外部网络耗时分开记录。
- `Timeout` 是失败边界，不是性能目标；不能通过放大超时来证明 `P95 ≤ 1s`。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/config/config.go:22`、`:125`、`:167` | 单一结构定义服务配置；兼容字段 `MaxConcurrency` 保持 `1..32`，用途明确为 Client 连接与预热资源预算。 | process environment + args → typed resource budget → Validate |
| `internal/config/config.go:145`、`:150`、`:194` | 只在预热未显式配置时随降低的资源预算收敛；显式值仍必须满足 `0..MaxConcurrency`。 | max-concurrency override → implicit prewarm clamp |
| `cmd/server/main.go` · `clientOptions` / `run` / `maxHeaderBytes`；`cmd/server/main_test.go` · `TestHeaderBudgetAcceptsMaximumLegacyQuery` | 完整配置映射到 Client 和 Handler，显式零值不会被二次默认化吞掉；真实 HTTP 入口可承载恰好 64 KiB raw query。 | Config + request limits → HTTP/Client options → validated runtime |
| `internal/server/server.go` · `Handler` / `checkSolveOrigin` / `decodeQueryRequest` / `requestFromPayload` / `handleSolve`；`internal/server/openapi.go` · `legacyQueryParameters` / `solveResponses` | POST JSON 和 deprecated GET query 使用共享字段合同、分离参数源；明确浏览器跨源在 Solver 前返回 403，其他合法请求直接进入 Solver，两种方法只声明 200/400/403/500。 | method/source → origin + JSON/query gate → timeout context → Solve |
| `internal/artifact/store.go` · `SaveFailure` / `Purge` | 保留期、组数和总字节硬配额同时执行。 | failure set → purge expired → evict oldest → O_EXCL write |
