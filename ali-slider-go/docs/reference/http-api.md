# HTTP API

> **风险提示**：API 当前没有鉴权。默认只应监听 `127.0.0.1`；将服务显式监听到 `0.0.0.0` 前，必须理解任何可达客户端都能提交挑战和代理地址。已废弃的 GET query 也会发起真实求解，不是无副作用读取；URL 可能进入历史和访问日志。详细边界见 [安全边界](../design/security.md)。

## 接口概览

| 方法 | 路径 | 用途 | 是否调用 Solver |
|---|---|---|---|
| `GET` | `/` | 返回内嵌 API 测试页 | 否；页面只自动检查 health，必须手工提交 |
| `POST` | `/api/slider` | 执行一轮挑战 | 通过浏览器跨源与 HTTP 输入校验后调用一次 |
| `POST` | `/api/bxua` | 生成一次 Baxia / Fireye `bx-ua` | 否；下载当前脚本后本地生成，不发送目标业务请求 |
| `GET` | `/api/slider?...` | 兼容旧 Python query；已废弃 | 通过浏览器跨源与 HTTP 输入校验后调用一次 |
| `GET` | `/health` | 进程级就绪检查 | 否 |
| `GET` | `/openapi.json` | 返回 OpenAPI 3.0.3 JSON | 否 |

只有 `/`、`/api/slider`、`/api/bxua`、`/health` 和 `/openapi.json` 五个路径；Solve 路径同时接受 GET 和 POST，`/api/bxua` 只接受 POST。其他方法或路径返回 `404`。
若极低概率的系统密码学随机源失败，`GET /` 返回 `500 text/plain` 且不调用 Solver；OpenAPI 同时声明该失败边界。

## 合同与兼容策略

当前 HTTP 字段名、别名优先级、状态码和 Go SDK 导出签名保持兼容。新调用方使用 `POST /api/slider`；GET 兼容入口继续保留其原有行为。HTTP 的 `VerifyCode` / `VerifyResult` 历史大小写不会随内部重构改变。

OpenAPI 从 `internal/interfaces/httpapi/openapi.go` 定义，在 Handler 构造时编码并复用。接口增加或修改时，应同步本页与 `internal/interfaces/httpapi` 合同测试；不手工维护第二份 JSON 副本。稳定的 `operationId` 供客户端代码生成使用：

| 操作 | operationId |
|---|---|
| `POST /api/slider` | `solveSlider` |
| `POST /api/bxua` | `generateBXUA` |
| `GET /api/slider` | `solveSliderLegacy` |
| `GET /health` | `getHealth` |
| `GET /openapi.json` | `getOpenAPI` |
| `GET /` | `getTestPage` |

Go SDK 的接入方式见 [README](../../README.md#go-sdk-接入) 和 `go doc ./pkg/slider`。SDK 的 `Result` 包含业务拒绝结果；`error` 表示参数、网络、协议、视觉或内部失败。调用方需要区分这两种情况，不能只凭 HTTP 200 或 `err == nil` 判断验证成功。

## 使用内嵌测试页

服务 ready 后用浏览器打开：

```text
http://127.0.0.1:8000/
```

页面内置于 Go EXE，不加载 CDN、字体、框架或第三方脚本，也不需要 Node.js。它固定调用当前 origin 的 `/health`、`/api/slider` 和 `/api/bxua`，因此换端口后无需配置页面。选择“验证码求解”或“Baxia bx-ua”切换操作；后者的页面 URL 和请求 URL 是计算输入，不改变页面调用的本地 API 地址。
测试页需要支持 `fetch`、`AbortController` 和 `TextEncoder` 的现代 Edge、Chrome 或 Firefox；没有现代浏览器时仍可直接调用 API。

- 页面打开时只执行无副作用的 health 检查，不自动创建挑战。
- 只有手工提交才会发送一次 POST JSON；执行期间按钮和类型选择禁用，防止重复提交或切换。页面不生成旧 GET query，也不自动重试、批量请求或并发压测。
- 空的可选字段不进入 JSON，验证码模式的默认 Scene ID/prefix 由服务启动配置决定；Baxia 模式必须填写 `pageUrl` 和 `requestUrl`，共用代理输入。
- 页面等待上限可设为 `1..305` 秒，并支持手工取消。取消会传播到请求 context，但若 Verify 已发出，上游结果可能未知；不要手工重试结果未知的一轮。
- RPC key、代理、`securityToken`、`certifyId` 和 `bx-ua` 默认遮罩，可勾选“显示敏感字段”查看。请求和响应只保留在当前页面内存，不写 Cookie、URL、localStorage、sessionStorage 或遥测；刷新或“清空”即移除。
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

代理由调用方控制：验证码模式影响本轮全部网络出口，Baxia 模式只影响 AWSC 与 Fireye 公开脚本下载。不要把未经信任的代理参数开放给公网调用方。

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

无痕和图片拼图返回同一组阿里 Verify 字段。调用方可按业务要求自行用 `certifyId`、`sceneId`、成功态 `isSign=true` 和 `securityToken` 组装 `captcha_verify_param`；本 Solver 不预拼接或提交该业务字段，也不主动调用站点短信接口。

业务成功条件为 `VerifyCode == "T001" && VerifyResult && securityToken != ""`。三种验证码类型共用同一结果格式；历史成功率和耗时不属于接口保证，测量范围见 [性能说明](../design/performance.md)。

## Baxia：POST /api/bxua

服务使用与验证码接口相同的监听地址、`--v8-library`、`--timeout` 和代理格式，无需单独启动 `cmd/bxua`。每次请求生成一份公共移动 Chromium 设备画像，下载当前 AWSC 和它选中的 Fireye，在本地 V8 中生成一次 token，然后关闭会话。启动、打开测试页和读取 health 都不会下载这些脚本。

### 请求字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `pageUrl` | string | 是 | SDK 所在页面的完整 HTTP(S) URL，可以包含 `#/...` 路由 |
| `requestUrl` | string | 是 | 实际业务请求的完整 HTTP(S) URL，保留其 query；本接口不会发送该请求 |
| `proxy` | string / null | 否 | 同验证码接口；空或 `null` 时直连，无 scheme 时补 `http://` |
| `Proxy` | string / null | 否 | `proxy` 的兼容别名；两者同时出现时 `proxy` 优先，包括空值 |

URL 去除首尾空白后不能为空，每个最多 8,192 个 UTF-8 字节，必须有主机且不能带用户名/密码。字段名区分大小写，JSON body 最多 65,536 字节，未知字段忽略，URL query 不参与参数合并。空 body、`null`、数组、缺少 URL 或多个 JSON 对象均返回 `400`。不接收客户端指定的 JS 源码、文件路径或 V8 路径；不提供 GET query 入口。

以下示例需要先启动本地服务，提交后只下载公开脚本并本地计算。代理应改为实际可用地址；直连时删除 `proxy` 字段或传空字符串：

```bash
curl --fail-with-body \
  --request POST \
  --header 'Content-Type: application/json' \
  --data '{
    "pageUrl":"https://www.galaxyticketing.com/en/#/login/account?loginBefore=%252FuserCenter%252FaccountList",
    "requestUrl":"https://rest-sig.imaitix.com/api/user/userLogin?_bx-v=2.5.37",
    "proxy":"http://127.0.0.1:7890"
  }' \
  http://127.0.0.1:8000/api/bxua
```

### 成功结果

成功返回 HTTP `200`，字段如下。`ok=true` 表示本地生成成功，不表示登录成功或站点风控接受。

| 字段 | 类型 | 说明 |
|---|---|---|
| `ok` | boolean | 生成成功时为 `true` |
| `bx-ua` | string | 完整 token；业务请求头使用同名 `bx-ua` |
| `uaHeaders` | object | 同一设备画像的 `User-Agent`、`Accept-Language`、`Sec-CH-UA`、`Sec-CH-UA-Mobile`、`Sec-CH-UA-Platform` |
| `version` | integer | 当前 SDK 实测协议版本，不固定为某一版本 |
| `sdkURL` | string | AWSC 实际选中的 Fireye 下载地址 |
| `sdkSha256` / `awscSha256` | string | 本次两份脚本的 SHA-256，用于定位版本变化 |
| `profile` | object | SDK 编码信息：`version`、`prefix`、含 padding 的 65 字符 `alphabet`、`sdkHash`；不是设备画像，也不代表内部密钥 |
| `proxied` | boolean | 本次脚本下载是否配置了代理 |
| `elapsedMs` | integer | 本次服务端处理耗时，单位毫秒 |
| `traceId` | string | 与 `X-Trace-ID` 响应头一致 |

调用方后续发送业务请求时，应同时使用返回的 `bx-ua` 和整组 `uaHeaders`。每次 HTTP 调用都是独立会话，不能假设不同调用属于同一台设备；需要自行指定画像或连续保持 SDK 会话时使用 [Go Baxia 接口](../../README.md#baxia--fireye-的-bx-ua)。该接口不生成 `bx_et`、`bx-umidtoken` 或验证码通过凭据。

### 错误与超时

错误复用 `{ "ok": false, "errorType": "...", "error": "...", "traceId": "..." }` 格式：

| HTTP 状态 | `errorType` | 说明 |
|---|---|---|
| `400` | `ApiRequestError` / `InvalidRequest` | 必填 URL、JSON、代理等请求参数无效 |
| `403` | `ApiOriginError` | 明确的跨源浏览器请求 |
| `500` | `BaxiaSourceError` | AWSC 或 Fireye 下载失败 |
| `500` | `BaxiaUnsupportedSDKError` | 当前 SDK 接口或编码路径无法适配 |
| `500` | `BaxiaRuntimeError` | V8 初始化、执行或 token 校验失败 |
| `500` | `BaxiaCanceled` | 请求取消或执行超时 |
| `500` | `InternalError` | 内部错误；不回显底层错误或 panic 内容 |

总执行受服务 `--timeout` 和客户端连接 context 限制；脚本下载与会话创建阶段另有最多 2 分钟上限，单次初始化最多 30 秒，均不延长外层 deadline。下载或适配失败不会返回旧 token，也不会静默回退到旧 SDK。具体错误消息已脱敏，排查时携带 `traceId`。

## 验证码接口：处理结果与错误

| HTTP 状态 | `errorType` | 语义 | Solver 调用 |
|---|---|---|---|
| `400` | `ApiRequestError` | JSON、query URL encoding/64 KiB、类型、长度、prefix 或 proxy 校验失败 | 不调用 |
| `400` | `InvalidRequest` | Solver 返回稳定的无效请求错误 | 已调用一次 |
| `403` | `ApiOriginError` | 浏览器 `Origin` / `Sec-Fetch-Site` 明确表明跨源 | 不调用 |
| `500` | `ProtocolError`、`NetworkError`、`VisionError`、`InternalError` | Solver 返回分类错误 | 已调用一次 |
| `500` | `UnhandledProtocolError` | 未分类错误或 Solver panic；响应不回显原始错误 | 已调用一次 |
| `404` | 无固定 `errorType` | 未开放的方法或路径 | 不调用 |

Handler 不因本机在途请求数返回 `429` 或 `Retry-After`。每个通过跨源与输入校验的 Solve 都进入应用 service；生产实现逐轮冷建独立 Device/V8 会话。`--max-concurrency` 只控制出站连接和 PE 空闲引擎保留量，不限制 Device 会话或 HTTP 在途请求。

通过跨源和输入校验的请求使用服务 `--timeout`。Handler 从请求 `context.Context` 派生 deadline 并传入 Solver；调用方断开或 deadline 到期会取消该 context，超时返回脱敏 `500 NetworkError`。Solver panic 由 Handler 恢复并返回脱敏 `500 UnhandledProtocolError`，不会回显 panic 内容。自定义 Solver 仍必须主动响应 `context.Context`。

### 响应头

| 响应 | Header | 说明 |
|---|---|---|
| 所有响应 | `Cache-Control: no-store` | 防止包含挑战结果的响应被缓存 |
| `GET /` | `Content-Security-Policy`、`X-Frame-Options`、`Cross-Origin-Resource-Policy`、`Permissions-Policy` | 页面只运行随机 nonce 的内联资源，只连接同源 API，禁止被 frame 嵌入 |
| `GET /` | `Content-Type: text/html; charset=utf-8` | 内嵌测试页 |
| `/api/slider` 和 `/api/bxua` 的 `200/400/403/500` | `X-Trace-ID` | 与响应体 `traceId` 一致 |
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

## 实现与回归入口

HTTP 的输入、输出与 OpenAPI 由 [interfaces/httpapi](../../internal/interfaces/httpapi) 维护。`Handler` 消费应用层请求/结果合同，不依赖公共 SDK；验证码兼容映射位于 [pkg/slider](../../pkg/slider)。Baxia HTTP 由应用层编排，复用 [pkg/baxia](../../pkg/baxia) 对应的底层下载与会话实现。

```bash
go test -count=1 ./internal/interfaces/httpapi ./tests/architecture
```

该命令用本地替身核对字段、别名、状态、跨源边界和公共 SDK 合同，不访问上游。HTTP 协议改动需要同时更新生成式 OpenAPI、本页与相应合同测试。
