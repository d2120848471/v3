# HTTP API

> **风险提示**：API 当前没有鉴权。默认只应监听 `127.0.0.1`；将服务显式监听到 `0.0.0.0` 前，必须理解任何可达客户端都能提交挑战和代理地址。已废弃的 GET query 也会发起真实求解，不是无副作用读取；URL 可能进入历史和访问日志。详细边界见 [security.md](./security.md)。

## 接口概览

| 方法 | 路径 | 用途 | 是否调用 Solver |
|---|---|---|---|
| `GET` | `/` | 返回内嵌 API 测试页 | 否；页面只自动检查 health，必须手工提交 |
| `POST` | `/api/slider` | 执行一轮挑战 | 通过浏览器跨源与 HTTP 输入校验后调用一次 |
| `GET` | `/api/slider?...` | 兼容旧 Python query；已废弃 | 通过浏览器跨源与 HTTP 输入校验后调用一次 |
| `GET` | `/health` | 进程级就绪检查 | 否 |
| `GET` | `/openapi.json` | 返回 OpenAPI 3.0.3 JSON | 否 |

只有 `/`、`/api/slider`、`/health` 和 `/openapi.json` 四个路径；Solve 路径同时接受 GET 和 POST。其他方法或路径返回 `404`。
若极低概率的系统密码学随机源失败，`GET /` 返回 `500 text/plain` 且不调用 Solver；OpenAPI 同时声明该失败边界。

## 使用内嵌测试页

服务 ready 后用浏览器打开：

```text
http://127.0.0.1:8000/
```

页面内置于 Go EXE，不加载 CDN、字体、框架或第三方脚本，也不需要 Node.js。它固定调用当前 origin 的 `/health` 和 `/api/slider`，不能修改目标 URL，因此换端口后无需配置页面。
测试页需要支持 `fetch`、`AbortController` 和 `TextEncoder` 的现代 Edge、Chrome 或 Firefox；没有现代浏览器时仍可直接调用 API。

- 页面打开时只执行无副作用的 health 检查，不自动创建挑战。
- 只有点击“发送一次求解”才会提交一个合法 POST JSON；执行期间按钮禁用，防止双击。页面不生成旧 GET query，也不自动重试、批量请求或并发压测。
- 空字段不进入 JSON，实际默认 Scene ID/prefix 由服务启动配置决定。
- 页面等待上限可设为 `1..305` 秒，并支持手工取消。取消会传播到请求 context，但若 Verify 已发出，上游结果可能未知；不要手工重试结果未知的一轮。
- RPC key、代理、`securityToken` 和 `certifyId` 默认遮罩。请求和响应只保留在当前页面内存，不写 Cookie、URL、localStorage、sessionStorage 或遥测；刷新或“清空”即移除。
- 页面用 `textContent` 显示响应；CSP 只允许同源连接，并禁止外部资源、frame、form action、worker 和不带随机 nonce 的脚本/样式。

测试页是本机调试入口，不是鉴权边界。Handler 会根据浏览器 `Origin` / `Sec-Fetch-Site` 拒绝明确的跨源 GET/POST Solve，但这不是账号、权限或完整 CSRF 身份机制；不要把监听地址改成 `0.0.0.0` 后直接暴露到公网。无浏览器来源头的 curl/程序客户端仍允许；POST 还保持空 body 和无 Content-Type 兼容性。

## 推荐：POST JSON

```http
POST /api/slider HTTP/1.1
Content-Type: application/json
```

请求体可以为空或为 JSON object。空、`null` 或只含空白的可选字符串按未传处理；未知字段被忽略。请求体总大小不得超过 65,536 字节。POST 只读 JSON body，会忽略 URL query；不支持 `application/x-www-form-urlencoded` 或 multipart form body。

### 请求字段

| 规范字段 | 兼容别名 | POST JSON 类型 / GET query | 长度/格式 | 默认值 |
|---|---|---|---|---|
| `SceneId` | `sceneId` | string 或 `null` / URL-decoded string | 去除首尾空白后最多 64 个 Unicode 字符 | 服务配置的 `SceneID`，默认 `1ug4aptr` |
| `prefix` | `Prefix` | string 或 `null` / URL-decoded string | 1–32 个 ASCII 字母或数字；空值使用默认值 | 服务配置的 `Prefix`，默认 `fsgtmi` |
| `AaduaneId` | `aaduaneId` | string 或 `null` / URL-decoded string | 去除首尾空白后最多 128 个 Unicode 字符 | 空，由 Solver 使用自身默认 RPC key id |
| `proxy` | `Proxy` | string 或 `null` / URL-decoded string | 见“代理格式” | 空，表示直连 |

当规范字段和别名同时出现时，规范字段优先，即使其值是 `null` 或空字符串。例如同时传入 `SceneId: null` 和 `sceneId: "x"` 时，服务使用默认 Scene ID，而不是 `x`。

### 代理格式

HTTP 层接受以下 scheme：

- `http`
- `https`
- `socks4`
- `socks5`
- `socks5h`

省略 scheme 时自动补为 `http://`。代理必须包含主机；显式端口必须位于 `1..65535`。传输层只接受不含业务 path、query 和 fragment 的绝对代理 URL，因此推荐形式为：

```text
http://127.0.0.1:7890
socks5h://proxy.example.test:1080
```

代理由调用方控制且会影响本轮全部网络出口。不要把未经信任的代理参数开放给公网调用方。

### curl 示例

使用默认场景并直连：

```bash
curl --fail-with-body \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{}' \
  http://127.0.0.1:8000/api/slider
```

覆盖场景参数：

```bash
curl --fail-with-body \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{"SceneId":"1ug4aptr","prefix":"fsgtmi"}' \
  http://127.0.0.1:8000/api/slider
```

上述命令会调用真实 Solver。只允许在自有或有书面授权的测试环境中执行；普通 CI 应使用 Mock Solver。

## 旧版兼容：GET query（已废弃）

GET query 仅用于不能立即迁移的旧 Python 调用方。新接入必须优先使用 POST JSON。

```bash
curl --fail-with-body --get \
  --data-urlencode 'SceneId=1ug4aptr' \
  --data-urlencode 'prefix=fsgtmi' \
  http://127.0.0.1:8000/api/slider
```

兼容规则：

- 只识别表中 8 个精确、区分大小写的名称；未知 query 键被忽略。
- 同名 query 键重复时取最后一个值。规范名和别名同时出现时，规范名始终优先，即使它为空值。
- 空 query 值按未传处理：`SceneId/prefix` 回退服务默认值，`AaduaneId/proxy` 保持空。query 中的文字 `null` 是普通字符串，不是 JSON `null`。
- 整个 raw query 最大 65,536 字节；超限或非法 URL encoding 返回 `400 ApiRequestError`，不调用 Solver。
- GET 只读 query 并忽略 body；POST 只读 JSON body 并忽略 query。两个来源永不合并。

GET 是真实有副作用的 Solve，不是健康检查。即使响应头禁止缓存，URL 仍可能进入浏览器历史、反向代理、API gateway 或访问日志。不要把 `AaduaneId` 或含 username/password 的 proxy 放入 query；存在敏感参数时只使用 POST JSON。

## 读取结果

一次完整协议往返始终使用 `200`，即使 Verify 业务结果未通过。调用方必须读取 `ok`、`VerifyCode` 和 `VerifyResult`，不能只判断 HTTP 状态码。

```json
{
  "ok": false,
  "securityToken": "",
  "VerifyCode": "F015",
  "VerifyResult": false,
  "certifyId": "***",
  "sceneId": "1ug4aptr",
  "proxied": false,
  "elapsedMs": 842,
  "timingsMs": {
    "total": 842
  },
  "traceId": "9a06ac1a6f13"
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `ok` | boolean | Solver 的业务成功判断；业务失败时为 `false` |
| `securityToken` | string | Verify 成功产生的敏感令牌；可能为空，不得写入日志 |
| `VerifyCode` | string | 上游 Verify 结果码，例如成功码 `T001` |
| `VerifyResult` | boolean | 上游 Verify 布尔结果 |
| `certifyId` | string | 本轮敏感挑战标识；不得写入日志或失败样本指标 |
| `sceneId` | string | 本轮实际 Scene ID |
| `proxied` | boolean | 本轮是否使用代理 |
| `elapsedMs` | integer | Solver 报告的完整耗时毫秒数 |
| `timingsMs` | object | 稳定阶段名到非负毫秒数的映射 |
| `traceId` | string | 服务生成的 12 位小写十六进制追踪 ID |

`pkg/slider.Client` 已接通完整 Go 编排和进程内 V8 Device/动态 PE 运行时，不启动 Node 子进程。`ok=true` 的判定是 `VerifyCode == "T001" && VerifyResult && securityToken != ""`。2026-08-08 以前的单挑战 `T001` 和 2026-08-07 `196/200`、Client `P95=984ms` 都属于旧运行时路径，不得外推到当前架构。当前 V8 生产组件探针只验证 Device/PE 执行，不调用 Captcha Init/Verify，因此不构成完整成功率或 P95 报告。

两次记录都直接验证公共 Client，不是 HTTP 传输基准。历史批次的 `P99=1018ms`，不能声称 P99 或每个响应都小于 1 秒；当前单次 smoke 更不能用来计算分位数。聚合结果不包含令牌、挑战标识、图片或上游正文。

## 处理结果与错误

| HTTP 状态 | `errorType` | 语义 | Solver 调用 |
|---|---|---|---|
| `400` | `ApiRequestError` | JSON、query URL encoding/64 KiB、类型、长度、prefix 或 proxy 校验失败 | 不调用 |
| `400` | `InvalidRequest` | Solver 返回稳定的无效请求错误 | 已调用一次 |
| `403` | `ApiOriginError` | 浏览器 `Origin` / `Sec-Fetch-Site` 明确表明跨源 | 不调用 |
| `500` | `ProtocolError`、`NetworkError`、`VisionError`、`InternalError` | Solver 返回分类错误 | 已调用一次 |
| `500` | `UnhandledProtocolError` | 未分类错误或 Solver panic；响应不回显原始错误 | 已调用一次 |
| `404` | 无固定 `errorType` | 未开放的方法或路径 | 不调用 |

HTTP 层不设置本地 admission gate、不维护并发槽，也不因本机在途请求数主动返回 `429` 或 `Retry-After`。每个通过浏览器跨源与输入校验的 GET/POST Solve 请求都直接进入 Solver；`--max-concurrency` 仅是 Client 每个 route/host 的出站连接与设备预热资源预算，不是 HTTP 并发上限。上游、连接池或机器资源仍可能自然等待或以技术错误结束。

通过跨源和输入校验的请求使用服务 `--timeout`。Handler 从请求 `context.Context` 派生 deadline 并传入 Solver；调用方断开或 deadline 到期会取消该 context，超时返回脱敏 `500 NetworkError`。Solver panic 由 Handler 恢复并返回脱敏 `500 UnhandledProtocolError`，不会回显 panic 内容。自定义 Solver 仍必须主动响应 `context.Context`。

### 响应头

| 响应 | Header | 说明 |
|---|---|---|
| 所有响应 | `Cache-Control: no-store` | 防止包含挑战结果的响应被缓存 |
| `GET /` | `Content-Security-Policy`、`X-Frame-Options`、`Cross-Origin-Resource-Policy`、`Permissions-Policy` | 页面只运行随机 nonce 的内联资源，只连接同源 API，禁止被 frame 嵌入 |
| `GET /` | `Content-Type: text/html; charset=utf-8` | 内嵌测试页 |
| `/api/slider` 的 `200/400/403/500` | `X-Trace-ID` | 与响应体 `traceId` 一致 |
| JSON 响应 | `Content-Type: application/json; charset=utf-8` | UTF-8 JSON |
| HTML 与 JSON | `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer` | 禁止 MIME 猜测且不发送 referrer |

## 健康检查与 OpenAPI

健康检查：

```bash
curl --fail http://127.0.0.1:8000/health
```

```json
{"ok":true,"status":"ready"}
```

该检查只证明 HTTP handler 和本地进程可以响应，不会对每次 health 调用重新探测外部服务，也不代表在线成功率或 P95 达标。

下载 OpenAPI：

```bash
curl --fail --output openapi.json http://127.0.0.1:8000/openapi.json
```

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `internal/server/server.go` · `TestPagePath` / `SolvePath` / `HealthPath` / `OpenAPIPath` / `ServeHTTP`；`internal/server/testpage.go` · `writeTestPage` | HTTP 层只开放四个路径；Solve 支持推荐 POST 和已废弃 GET，页面本身仍只手工发送 POST。 | method + path → HTML/JSON handler 或 404 |
| `internal/server/server.go` · `decodeRequest` / `decodeQueryRequest` / `requestFromPayload` / `optionalAliasedText`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` | JSON body 和 raw query 各限 64 KiB；GET/POST 参数源分离，query 同名取最后值，规范名优先，非法 encoding 在 Solver 前返回 400。 | method → JSON body 或 query → alias resolution → `slider.Request` |
| `internal/server/server.go` · `normalizeProxy` | HTTP 层校验代理 scheme、主机和端口，并为无 scheme 输入补 `http://`。 | proxy text → `url.Parse` → normalized proxy |
| `internal/server/server.go` · `browserOriginProtection` / `checkSolveOrigin` / `handleSolve` / `callSolver` / `solverErrorResponse`；`internal/server/server_test.go` · `TestBrowserOriginBoundaryPreservesLegacyClients` | GET 有真实副作用，因此与 POST 一样经过跨源检查；无浏览器来源头客户端允许，合法请求在 deadline context 中调用一次 Solver。 | origin gate → method-specific parse → timeout context → Solve ×1 → result/error mapping |
| `internal/config/config.go` · `MaxConcurrency`；`cmd/server/main.go` · `clientOptions`；`pkg/slider/client.go` · `NewTransportPool` | 兼容配置名 `MaxConcurrency` 只进入 Client 的每 route/host 连接与预热资源边界，不进入 HTTP Handler。 | config → ClientOptions → transport/prewarm budget；valid GET/POST Solve → Solver |
| `pkg/slider/types.go:22` · `Result`；`:45` · `ErrorKind` | 成功响应字段与稳定错误类别由 library 合同定义。 | Solver output → server response DTO → JSON |
| `internal/server/openapi.go` · `openAPIDocument` / `legacyQueryParameters` / `solveResponses`；`internal/server/server_test.go` · `TestEmbeddedAPITestPage` / `TestOnlyFrozenRoutesAreExposed` | 运行时 OpenAPI 描述 POST JSON、已废弃 GET 的 8 个 query 名称、完整 200 字段及 400/403/500 合同；测试页仍只手工发送 POST。 | GET `/` / `/openapi.json` → embedded page / generated contract → same-origin manual POST |
| `internal/device/rpc.go:119`、`:130` | Device 顶层 Code 统一校验，只有 Log1 解码对象内的 DeviceConfig；Log2/3 接受非对象结果。 | upstream JSON → action-specific schema → session result |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 候选 200/32/应用层零重试批次严格成功 `196/200`，Client 完整链墙钟 P95 `984ms`；最终 one-shot 加固后未在线重跑。 | public Client → one Solve per job → aggregate Client contract evidence；不经过 HTTP Handler |
