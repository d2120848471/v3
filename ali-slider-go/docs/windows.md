# Windows AMD64 便携包

GitHub Actions 在 Linux quality、Darwin race 与 Linux V8 双架构门禁通过后，使用 Windows 2025 runner 原生测试、构建、解压并启动最终包。产物 `ali-slider-go-windows-amd64.zip` 支持 Windows 10 / Windows Server 2016 或更高版本的 AMD64/x64 机器，已内置静态 CRT 构建的 `ali_slider_v8_runtime.dll`，解压即可运行，不需要另装 Go、Rust、Python、Node.js、OpenCV 或 VC++ Runtime。

平台下限依据 Go 1.26 的官方 [Minimum Requirements](https://go.dev/wiki/MinimumRequirements)。

> 本程序是本机控制台 HTTP 服务，不是桌面图形应用，但内置浏览器 API 测试页。当前包只支持 Windows AMD64/x64。默认启动会访问外部 Device RPC 做会话预热；测试页手工求解还会访问 Captcha RPC 和图片 CDN。仅限自有系统或获得明确授权的测试环境。

测试页需要现代 Edge、Chrome 或 Firefox，不支持 Internet Explorer。没有现代浏览器时，Go 服务和 PowerShell/API 仍可使用；包内 V8 DLL 由 Go 服务在进程内加载，不会启动 Node 子进程或打开窗口。

## 下载并启动

1. 打开 GitHub 仓库的 **Actions** 页面。
2. 选择 `ali-slider-go-ci`，打开 `main` 最新一次成功运行。
3. 下载 `ali-slider-go-windows-amd64.zip`。Actions 产物保留 30 天；下载者需要仓库读取权限。
4. 把 ZIP 完整解压到当前用户的私有可写目录。不要直接在压缩软件预览窗口中运行。
5. 双击 `start.bat`。
6. 等待控制台出现 `event=listen status=ready`。

启动后：

| 用途 | 地址 |
|---|---|
| API 测试页 | `http://127.0.0.1:8000/` |
| 健康检查 | `http://127.0.0.1:8000/health` |
| 求解 API | `http://127.0.0.1:8000/api/slider`（POST JSON；deprecated GET query） |
| OpenAPI | `http://127.0.0.1:8000/openapi.json` |

在服务窗口按 `Ctrl+C` 可触发应用的关闭流程。关闭控制台窗口或用任务管理器结束进程属于强制停止，不应视为优雅关闭。

## 包内容

```text
ali-slider-go-windows-amd64.zip
├── ali-slider-go.exe
├── ali_slider_v8_runtime.dll
├── THIRD-PARTY-NOTICES.txt
├── start.bat
├── README-Windows.txt
├── BUILD-INFO.txt
└── SHA256SUMS.txt
```

- `ali-slider-go.exe` 是 `CGO_ENABLED=0` 的 Windows AMD64 console PE；默认触摸轨迹已经嵌入 EXE。
- `ali_slider_v8_runtime.dll` 是 V8 `149.4.0`/ICU 77 的 C ABI wrapper，用于逐挑战设备 SDK/FeiLin 和动态 PE；公开脚本/画像缓存 5 分钟，但 token/data 不跨轮复用。
- `THIRD-PARTY-NOTICES.txt` 包含 rusty_v8、V8、ICU 与对应数据许可说明。
- `start.bat` 先切换到自身目录，再提供回环地址、端口、`var\artifacts` 路径与包内 DLL 的安全默认值；缺少 EXE 或 DLL 都会在启动前停止，异常退出时保留窗口显示错误。
- `README-Windows.txt` 是可脱离仓库阅读的最终用户说明；打包时写为 UTF-8 BOM，便于 Windows Server 2016 旧记事本正确识别中文。
- `BUILD-INFO.txt` 记录完整 commit、ref、Go/Rust/V8 版本、目标平台和 UTC 构建时间，不包含 Secret 或 runner 用户路径。
- `SHA256SUMS.txt` 保存包内文件校验值。

包内没有项目源码、测试 fixture、Git 元数据、日志、凭据或运行 artifact。除 Go 服务外只带对应 V8 DLL 及第三方许可说明；下载后的 ZIP 可以直接转发给使用者，使用者不需要 GitHub 账号。

## 调用与配置

默认启动不需要配置。最直接的测试方式：

1. 打开 `http://127.0.0.1:8000/`；
2. 确认页面右上角显示“服务已就绪”；
3. 参数可全部留空，或按需填写 SceneId、prefix、AaduaneId、proxy；
4. 点击“发送一次求解”；
5. 同时检查 HTTP、业务状态、Trace ID、`ok`、`VerifyCode` 和 `VerifyResult`。

页面打开只检查 health，不自动创建挑战。点击提交才访问真实上游；执行中防双击，不自动重试。RPC key、代理、`securityToken` 和 `certifyId` 默认遮罩，但原始值仍存在当前页面内存中；不要截图、复制到公开日志、工单或聊天。刷新或点击“清空”会移除页面内结果。

页面固定同源 POST JSON 调用 API，不使用 deprecated GET。其他网站的跨源请求不会成功：需要预检的浏览器 fetch 可能先被浏览器拦截；若明确跨源的 POST 或旧 GET 实际到达服务，则返回 `403 ApiOriginError` 且不进入 Solver。用户在地址栏直接打开的 `Sec-Fetch-Site: none` 与无浏览器头的旧客户端仍可用；这不是鉴权。不要为绕过该保护而放宽 CORS。

也可直接使用 PowerShell：

```powershell
$body = @{
  SceneId = "1ug4aptr"
  prefix  = "fsgtmi"
} | ConvertTo-Json

Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8000/api/slider" `
  -ContentType "application/json" `
  -Body $body
```

该请求会访问真实上游。HTTP `200` 仍需检查 `ok`、`VerifyCode` 和 `VerifyResult`。完整字段、状态码和敏感数据边界见 [HTTP API](./api.md)。

### 旧 GET query 兼容

仅当旧客户端无法立即迁移时才使用：

```powershell
Invoke-RestMethod `
  -Method Get `
  -Uri "http://127.0.0.1:8000/api/slider?SceneId=1ug4aptr"
```

这个 GET 会立即创建真实挑战，不是 health 或无副作用读取。raw query 上限为 `64 KiB`；支持 `SceneId/sceneId`、`prefix/Prefix`、`AaduaneId/aaduaneId` 和 `proxy/Proxy`。同名参数取最后值；canonical 键只要出现就压过 alias，空 canonical 值使用默认值而不是 alias。GET 只读 query，POST 只读 JSON body，两者不合并，URL-encoded/multipart form body 不受支持。

发布服务不会在 Handler 之前把该上限截断：`server.MaxRequestBytes` 是 64 KiB，进程的 HTTP `MaxHeaderBytes` 另留 32 KiB 给 request line 和普通 header。CI 使用真实 `net/http.Server` 回归恰好 64 KiB query 可达；超出仍会返回 400。

URL 可能被浏览器历史、代理/网关 access log 和复制记录收集；禁止在 URL 放入 `AaduaneId` 或含 userinfo 的 proxy。不要把旧 GET URL 做成链接、书签、预取、健康检查或监控地址。Solve POST 与 GET 的应用响应均只有 `200/400/403/500`，不会因本地在途数返回 429。

临时改端口时，在解压目录打开 `cmd.exe`：

```bat
start.bat --port=8001
```

此时测试页、健康检查、API 和 OpenAPI 地址中的端口也要改为 `8001`；页面内部使用同源相对路径，无需修改其他配置。最终以 `event=listen` 日志中的实际监听地址为准。`start.bat` 把额外参数放在内置默认参数之后，因此高级用户可以覆盖 flag。完整配置见 [配置参考](./configuration.md)。不要把 `--host` 改为 `0.0.0.0` 后直接对外暴露：服务没有应用内鉴权或本地 admission gate。

## 完整性与 SmartScreen

在 PowerShell 中计算 EXE 哈希：

```powershell
Get-FileHash .\ali-slider-go.exe -Algorithm SHA256
```

结果应与 `SHA256SUMS.txt` 中 `ali-slider-go.exe` 的值一致。CI 还会核对 ZIP SHA-256 与 GitHub artifact digest。

当前流水线没有 Authenticode 代码签名证书，因此 Defender 或 SmartScreen 可能显示“未知发布者”。SHA-256 只证明文件未变化，不证明发布者身份；不要关闭系统防护或绕过组织安全策略。需要降低未知发布者提示时，应另行配置组织持有的代码签名证书和受保护 GitHub Secret。

## Artifact 与 Windows 权限

Unix 构建强制 Artifact 目录/文件为 `0700/0600`。Windows 的 Go `FileMode` 不能表达 NTFS DACL，因此便携版继承解压目录的 ACL，同时仍执行：

- 文件系统根目录拒绝；
- 真实目录与同一文件复核；
- symlink/reparse 异常路径拒绝；
- 随机受管文件名与 `O_EXCL` 创建；
- 完整组回滚、保留期和总量配额。

应解压到当前用户的私有目录，不要放在 Web root、公共共享盘、多人可写目录或公开同步目录。`var\artifacts` 可能包含失败/低置信图片与脱敏指标，不应随普通诊断包转发。

## CI 构建合同

根工作流 `.github/workflows/ali-slider-go-ci.yml` 的 `windows-package` job：

1. 依赖 Linux quality 与 Darwin race 成功；
2. 在 `windows-2025` 原生运行 `go vet` 和全量 `go test`；
3. 使用 Go module 固定的 Go `1.26.5`、`CGO_ENABLED=0` 和 `GOARCH=amd64` 构建，并安装 Rust `1.88.0`；
4. 运行 Rust wrapper 单测，以静态 MSVC CRT 构建 DLL，再用真实 DLL 运行 Go→V8 测试；
5. 把 `ali_slider_v8_runtime.dll` 和 `THIRD-PARTY-NOTICES.txt` 纳入包，生成构建信息、UTF-8 BOM 中文说明、包内 SHA-256 和明确文件白名单；
6. 解压到含中文及空格的临时路径；
7. 通过最终 `start.bat` 启动 EXE，启动时必须先通过 DLL/ABI/V8/ICU 自检，再验证参数转发、内嵌页、`/health`、OpenAPI、跨源和非法输入合同；
8. smoke 强制 `--device-prewarm=0`，不会发送 Device、Init、图片或 Verify 请求；
9. `main` push 和 `workflow_dispatch` 直传单层 ZIP，PR 只验证不上传；官方 actions 使用完整 commit SHA 固定，工作流权限保持 `contents: read`。

本地只构建 EXE：

```bash
make build-windows
file dist/ali-slider-go-windows-amd64.exe
```

macOS/Linux 交叉构建可以证明 EXE/DLL 可生成并检查 PE 导出/依赖，但不能代替 Windows 执行。发布判断以 Windows runner 的原生 Go/Rust/V8 测试和最终 ZIP smoke 为准。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `go.mod:3`；`Makefile` · `build-windows` | Windows EXE 固定 Go 1.26.5、AMD64、CGO 关闭和 stripped 构建。 | source → pinned Go → PE executable |
| `internal/track/track.go:21` | 生产运行所需默认轨迹已嵌入 EXE，不需要外置 fixture。 | embedded asset → single executable |
| `internal/server/testpage.go`、`internal/server/web/test.html` | API 测试页、样式和脚本编译进同一 EXE，只能同源手工 POST，不生成敏感 query，不增加 ZIP 文件或运行时依赖。 | embedded page → browser GET `/` → explicit same-origin POST |
| `internal/server/server.go` · `decodeQueryRequest` / `checkSolveOrigin`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestSolveParameterSourcesStaySeparated` / `TestBrowserOriginBoundaryPreservesLegacyClients` | 旧 GET query 仅做协议兼容，与 POST 分离参数源且共用跨源/Solver 边界。 | legacy URL → query validation → same Solve side effects |
| `internal/artifact/store.go` · `ensureDirectory`；`store_test.go` | Unix 精确 mode 与 Windows ACL 语义分离，其他路径/竞态/配额保护保持。 | platform filesystem → safe directory → bounded artifact |
| `.github/workflows/ali-slider-go-ci.yml` · `windows-package` | 只有跨平台门禁、Windows 原生 Rust/Go→DLL 测试和 package smoke 成功后才上传可分发 ZIP。 | commit → quality/race/Linux V8 → Go PE + V8 DLL/notices → native Windows smoke → ZIP |
| `packaging/windows/start.bat` | 双击入口固定工作目录、检查包内 DLL、提供回环监听与显式 `--v8-library`，并保留 console 诊断和 Ctrl+C。 | extracted package → checked DLL + local server → controlled shutdown |
