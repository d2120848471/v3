# HTTP API

> **风险提示**：API 当前没有鉴权。默认只应监听 `127.0.0.1`；将服务显式监听到 `0.0.0.0` 前，必须理解任何可达客户端都能提交挑战和代理地址。详细边界见 [security.md](./security.md)。

## 接口概览

| 方法 | 路径 | 用途 | 是否调用 Solver |
|---|---|---|---|
| `POST` | `/api/slider` | 执行一轮挑战 | 通过 HTTP 输入校验后调用一次 |
| `GET` | `/health` | 进程级就绪检查 | 否 |
| `GET` | `/openapi.json` | 返回 OpenAPI 3.0.3 JSON | 否 |

其他方法或路径返回 `404`。特别地，`GET /api/slider` 不执行挑战。

## 提交一轮挑战

```http
POST /api/slider HTTP/1.1
Content-Type: application/json
```

请求体可以为空或为 JSON object。空、`null` 或只含空白的可选字符串按未传处理；未知字段被忽略。请求体总大小不得超过 65,536 字节。

### 请求字段

| 规范字段 | 兼容别名 | 类型 | 长度/格式 | 默认值 |
|---|---|---|---|---|
| `SceneId` | `sceneId` | string 或 `null` | 去除首尾空白后最多 64 个 Unicode 字符 | 服务配置的 `SceneID`，默认 `1ug4aptr` |
| `prefix` | `Prefix` | string 或 `null` | 1–32 个 ASCII 字母或数字；空值使用默认值 | 服务配置的 `Prefix`，默认 `fsgtmi` |
| `AaduaneId` | `aaduaneId` | string 或 `null` | 去除首尾空白后最多 128 个 Unicode 字符 | 空，由 Solver 使用自身默认 RPC key id |
| `proxy` | `Proxy` | string 或 `null` | 见“代理格式” | 空，表示直连 |

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

`pkg/slider.Client` 已接通完整纯 Go Solver。`ok=true` 的判定是 `VerifyCode == "T001" && VerifyResult && securityToken != ""`。2026-08-07 唯一授权候选报告恰好执行 200 个新挑战、并发 32、每个 job 一次 Solve、应用层零重试：严格成功 `196`、业务失败 `3`、`VisionError=1`、网络错误 `0`，成功率 `98%`。Client 完整链墙钟 `P50=816ms`、`P95=984ms`、`P99=1018ms`、`max=1555ms`，成功样本墙钟 `P95=989ms`，批次约 `6.16s`。该批先于最终传输 one-shot 加固，最终源码未获授权再跑第二批。

该报告验证公共 Client 与完整 Solver 的在线合同，不是 HTTP 传输基准。成功率和完整链 P95 达到冻结门槛；`P99=1018ms`，因此不能声称 P99 或每个响应都小于 1 秒。聚合结果不包含令牌、挑战标识、图片或上游正文。

## 处理结果与错误

| HTTP 状态 | `errorType` | 语义 | Solver 调用 |
|---|---|---|---|
| `400` | `ApiRequestError` | JSON、类型、长度、prefix 或 proxy 校验失败 | 不调用 |
| `400` | `InvalidRequest` | Solver 返回稳定的无效请求错误 | 已调用一次 |
| `500` | `ProtocolError`、`NetworkError`、`VisionError`、`InternalError` | Solver 返回分类错误 | 已调用一次 |
| `500` | `UnhandledProtocolError` | 未分类错误或 Solver panic；响应不回显原始错误 | 已调用一次 |
| `404` | 无固定 `errorType` | 未开放的方法或路径 | 不调用 |

HTTP 层不设置本地 admission gate、不维护并发槽，也不因本机在途请求数主动返回 `429` 或 `Retry-After`。每个通过输入校验的 `POST /api/slider` 都直接进入 Solver；`--max-concurrency` 仅是 Client 每个 route/host 的出站连接与设备预热资源预算，不是 HTTP 并发上限。上游、连接池或机器资源仍可能自然等待或以技术错误结束。

通过输入校验的请求使用服务 `--timeout`。Handler 从请求 `context.Context` 派生 deadline 并传入 Solver；调用方断开或 deadline 到期会取消该 context，超时返回脱敏 `500 NetworkError`。Solver panic 由 Handler 恢复并返回脱敏 `500 UnhandledProtocolError`，不会回显 panic 内容。自定义 Solver 仍必须主动响应 `context.Context`。

### 响应头

| 响应 | Header | 说明 |
|---|---|---|
| 所有响应 | `Cache-Control: no-store` | 防止包含挑战结果的响应被缓存 |
| `/api/slider` 的 `200/400/500` | `X-Trace-ID` | 与响应体 `traceId` 一致 |
| JSON 响应 | `Content-Type: application/json; charset=utf-8` | UTF-8 JSON |

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
| `internal/server/server.go:25` · `SolvePath`、`HealthPath`、`OpenAPIPath`；`:87` · `ServeHTTP` | HTTP 层只开放三个冻结入口；GET solve 走 404。 | method + path → route switch → handler 或 404 |
| `internal/server/server.go:169` · `decodeRequest`；`:237` · `optionalAliasedText` | 请求体限制为 64 KiB；规范字段优先，未知字段忽略，字符串先 trim 再校验。 | body → JSON object → alias resolution → `slider.Request` |
| `internal/server/server.go:268` · `normalizeProxy` | HTTP 层校验代理 scheme、主机和端口，并为无 scheme 输入补 `http://`。 | proxy text → `url.Parse` → normalized proxy |
| `internal/server/server.go:105` · `handleSolve`；`:135` · `callSolver`；`:145` · `solverErrorResponse` | HTTP 输入合法后不经过本地 admission，直接在请求派生的 deadline context 中调用一次 Solver；panic 被恢复，业务完成态仍为 200。 | parse → timeout context → Solve ×1 → result/error mapping |
| `internal/config/config.go:26` · `MaxConcurrency`；`cmd/server/main.go:124` · `clientOptions`；`pkg/slider/client.go:91` · `NewTransportPool` | 兼容配置名 `MaxConcurrency` 只进入 Client 的每 route/host 连接与预热资源边界，不进入 HTTP Handler。 | config → ClientOptions → transport/prewarm budget；valid POST → Solver |
| `pkg/slider/types.go:22` · `Result`；`:45` · `ErrorKind` | 成功响应字段与稳定错误类别由 library 合同定义。 | Solver output → server response DTO → JSON |
| `internal/server/openapi.go:3` · `openAPIDocument`；`:20` · `responses` | 运行时 OpenAPI 描述请求别名、完整 200 字段以及 400/500 错误合同，不声明本地 429。 | GET `/openapi.json` → generated document → JSON |
| `internal/device/rpc.go:119`、`:130` | Device 顶层 Code 统一校验，只有 Log1 解码对象内的 DeviceConfig；Log2/3 接受非对象结果。 | upstream JSON → action-specific schema → session result |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 2026-08-07 候选 200/32/应用层零重试批次严格成功 `196/200`，Client 完整链墙钟 P95 `984ms`；最终 one-shot 加固后未在线重跑。 | public Client → one Solve per job → aggregate Client contract evidence；不经过 HTTP Handler |
