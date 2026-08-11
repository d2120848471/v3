# 测试与质量门禁

> **当前结论**：生产运行时已从Node子进程迁到同进程V8 `149.4.0`。Device使用动态V8；PE按精确 `StaticPath`先做当前V8/纯Go完整差分，兼容才开启纯Go快路，其余保留V8 fallback。Linux AMD64/ARM64已通过Rust wrapper和Go→V8 ABI测试；Windows AMD64 DLL已交叉构建。最终安全候选真实50次为 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`，分类错误0；成功率不低于基线，但mean约1秒未通过。

## 测试原则

- 普通开发测试与 CI 的项目代码执行必须离线，不访问真实验证码、设备 RPC、图片 CDN或第三方代理。
- Python 只用于迁移阶段生成确定性 oracle。Go 测试只读取已提交的 JSON/PNG fixture，运行时不启动 Python。
- 正确性优先于性能；oracle、负例、唯一 Verify 或竞态测试失败时，性能结果无效。
- 在线验收必须有明确授权、由人工显式发起，并与普通 CI 隔离。
- 同一个 `CertifyId` 最多尝试一次 Verify；网络结果未知也视为已经消耗尝试位。
- Deprecated `GET /api/slider?...` 是具有真实上游副作用的兼容入口；普通测试只用 Mock Solver，不得用浏览器、爬虫或健康检查访问真实 GET Solve。
- 覆盖率必须从同一源码快照、同一份全项目 `coverprofile` 计算，不能拼接不同时间点的包级数字。

## 分层测试模型

| 层级 | 目的 | 主要入口 | 网络策略 | 当前状态 |
|---|---|---|---|---|
| L0 单元 | 编码、加密、配置、设备、动态 PE/runtime、脚本缓存、视觉、轨迹和边界分支 | 各包 `*_test.go` | 禁止真实外网；V8 host 网络用替身，Node 仅在可选历史 oracle/上下文差异测试中出现 | 通过 |
| L1 跨语言 oracle | 锁定 Python 与 Go 的协议和困难视觉语义 | `protocol/testdata`、`vision/testdata`、PE oracle | 静态 fixture | 通过 |
| L2 组件集成 | 验证设备 RPC schema、Captcha RPC、代理、下载、预热池及清理 | `internal/device`、`internal/challenge`、`internal/artifact` | Fake `RoundTripper` | 通过 |
| L3 完整离线链 | 验证 Device → Init → 类型自动分流；Puzzle 进入 PE/Assets/Vision/Complete/Verify，TRACELESS/SLIDING 进入 SDK completion/Verify | `solver_test.go`、`device_runtime_test.go`、`sdk_device_bridge_test.go` | 单一 Mock transport / fake PE runtime / Node bridge fixture | 通过 |
| L4 服务合同 | HTTP 四路径、POST JSON + deprecated GET query、内嵌页/CSP、别名/空值/参数源、状态码、OpenAPI、64 并发直通、无本地主动 429、日志脱敏、启动配置 | `server_test.go`、`cmd/server/main_test.go` | Mock Solver/占用端口 | 通过 |
| L5 工程门禁 | fmt、vet、test、race、coverage、staticcheck、govulncheck、V8 native/ABI 测试与便携包 | `Makefile`、Docker BuildKit、CI | Linux AMD64/ARM64 实际 V8 `.so`；Windows AMD64 原生 DLL；工具/依赖获取可联网 | 本地候选已验证；发布 commit 由 CI 重跑 |
| L6 授权在线 | 生产 V8 Device、精确 PE 差分探针；受控挑战 acceptance | `v8_runtime_online_test.go`、`online_acceptance_test.go`、其他 `online` build tag 测试 | 双重显式开关/授权；普通 CI 永不执行 | 当前V8/纯Go差分为0；最终50次 `47/50`、mean `2562ms`；4-slot/10-job组件通过 |

## Go 版本与快速验证

模块、CI 和 Docker 构建层统一固定 Go `1.26.5`；V8 wrapper 构建层固定 Rust `1.88.0`、`v8=149.4.0`、ICU data `0.77.0`。以下命令从 `ali-slider-go` 目录执行：

```bash
export CGO_ENABLED=0
go version
make fmt-check
make vet
make test
make race
make staticcheck
make vuln
make cover
make native-test
make build-linux
make build-linux-amd64
make build-linux-arm64
make build-windows
```

`make check` 会串行执行除 `cover` 外的全部质量门禁：

```bash
make check
```

Go 1.26 的 Linux race runtime 会强制要求 CGo，与本项目冻结的无 CGo 测试边界冲突。因此 `make race` / `make check` 的严格 race 部分在 Darwin 执行；Linux 本地改用 `make check-linux`。根 CI 的 Linux job 负责其余纯 Go 门禁，独立 macOS job 在 `CGO_ENABLED=0` 下执行全量 race。不得在 Linux 上临时改成 `CGO_ENABLED=1` 后把结果记为本项目的严格纯 Go race 门禁。

`staticcheck` 和 `govulncheck` 不写入生产 `go.mod`，由开发或 CI 环境提供。当前验证记录中两项均已通过；若本机命令不存在，应使用 CI 固定版本或按组织工具供应流程安装，不能跳过后仍声称门禁通过。

## 覆盖率门槛

全项目统一测量：

```bash
coverage_file="$(mktemp)"
trap 'rm -f "$coverage_file"' EXIT
go test -count=1 -coverprofile="$coverage_file" ./...
go tool cover -func="$coverage_file" | tail -n 1
```

核心包复核：

```bash
go test -count=1 -cover ./internal/protocol
go test -count=1 -cover ./internal/vision
go test -count=1 -cover ./internal/pe
```

当前统一记录：

| 指标 | 门槛 | 已验证结果 | 判定 |
|---|---:|---:|---|
| 全项目语句覆盖率 | `>= 80%` | `80.4%` | 通过；`internal/v8runtime=91.4%`，发布 commit 仍由 CI 重跑 |
| `internal/protocol` | `>= 90%` | `91.4%` | 通过；包含 fuzz seeds |
| `internal/vision` | `>= 90%` | `91.9%` | 通过 |
| `internal/pe` | 纳入全项目门禁 | `80.9%` | 通过 |

覆盖率不是线上成功率，也不证明尾延迟。迁移期间源码仍可能变化，因此 1.0.0 发布候选必须由 CI 对最终 commit 重新生成覆盖率证据。

## Fuzz seeds 与短跑

普通 `go test` 会执行 4 个 fuzz target 的静态 seeds：协议解包、密码/令牌解码、JS/RPC 编码和 PNG/视觉入口。所有任意输入均有 `64 KiB` 的 fuzz worker 边界，生产解压仍另有 `4 MiB` 上限。最终快照还对四个 target 各做了显式短跑，均 PASS。

```bash
go test -run '^$' -fuzz '^FuzzUnpackData$' -fuzztime=1s ./internal/protocol
go test -run '^$' -fuzz '^FuzzProtocolDecoders$' -fuzztime=1s ./internal/protocol
go test -run '^$' -fuzz '^FuzzPercentAndFormEncoding$' -fuzztime=1s ./internal/protocol
go test -run '^$' -fuzz '^FuzzPNGPairSeeds$' -fuzztime=1s ./internal/vision
```

## Python oracle 对照

### 协议与 PE

```bash
go test -count=1 ./internal/protocol \
  -run 'PythonOracle|Oracle'

go test -count=1 ./internal/pe \
  -run '^TestBuildMatchesPythonOracleSemantics$'
```

协议 fixture 锁定 JS/RPC 编码、签名、AES、DeviceToken、data codec 和参数。PE 测试对 Python data 解包后比较完整 payload、坐标、逻辑时钟、arg、TrackList 和 getter 元数据；Go 与 Python 的 zlib 字节流不必逐字节相同，但解包语义必须一致。

### 困难 edge-decoy 视觉

```bash
go test -count=1 -v ./internal/vision \
  -run '^TestPythonOracleEdgeDecoyFixtures$'
```

当前 `gap` 和 `no-gap` 两个子用例均为 `PASS`：

- `gap`：`x=241±1`、置信度至少 `0.45`，并包含 `global-chamfer`。
- `no-gap`：置信度低于门槛，且不得误触发 `global-chamfer`。

该命令只读取静态 PNG/JSON，不调用 Python 解释器。

## Device schema 与在线探针

Device RPC 的 `ResultObject` 在真实服务上并非所有 action 都使用同一 JSON 类型。当前实现先保留原始 JSON，只在 Log1 成功响应中解码对象内的 `DeviceConfig`；Log2/Log3 仅校验成功 Code，允许非对象结果。离线回归显式让 Log1 返回对象、Log2/Log3 返回字符串，防止再次把合法成功响应误判为 schema 错误。

独立在线探针只执行 Device Log1/2/3，不调用 Captcha Init/Verify。它同时要求 `online` build tag 和显式环境开关，普通 test/CI 不会访问真实目标。当前授权探针通过，action 数为 3、指纹字段数为 111，墙钟约 `0.53s`。该结果只证明设备初始化链，不代表完整验证码成功率。

## 动态 PE 与持久 V8 Device Isolate 组件探针

生产探针直接构造 `NewKeyResolver(libraryPath)`，请求公开 SDK/Device/PE，验证同一外层 V8 Device Isolate 两轮 recycle 后 context/session/token 仍独立，并在单独禁网 PE context 中强制 V8 输出、再与纯 Go 逐字段差分。它使用 dummy `CertifyId` 和静态图片名，不创建 Captcha Init/Verify：

```bash
ALI_SLIDER_V8_DEVICE_ONLINE=1 \
ALI_SLIDER_V8_TEST_LIBRARY=/absolute/path/libali_slider_v8_runtime.so \
CGO_ENABLED=0 go test -count=1 -tags online \
  -run '^TestOnlineV8DeviceRuntime$' -v ./internal/pe
```

Linux AMD64 最终 Debian/glibc 包中已验证：`tokenLength=1616`、`dataLength=1164`、Complete 后 142 个指纹字段、4 个请求，测试用时 `2.78s`。这只证明生产 V8 Device/PE 组件合同，不是验证码成功率。

并发组件探针按默认直连生产模型创建最多4个独立画像池slot，以Complete→Recycle接力服务10个job；同样不创建Captcha Init/Verify：

```bash
ALI_SLIDER_V8_DEVICE_CONCURRENT_ONLINE=1 \
ALI_SLIDER_V8_DEVICE_CONCURRENCY=10 \
ALI_SLIDER_V8_TEST_LIBRARY=/absolute/path/libali_slider_v8_runtime.so \
CGO_ENABLED=0 go test -count=1 -tags online \
  -run '^TestOnlineV8DeviceRuntimeConcurrentPrime$' -v ./internal/pe
```

Linux AMD64实测10/10满足142字段、4请求合同，组件wall为9.27s。前置默认直连A/B已证明5个同时存活的Device VM出现138字段，10-live还会触发request sequence错误；因此该测试锁定4槽池模型，不提供提高预热库存上限的开关。

`keys_online_test.go` 和 `device_runtime_online_test.go` 仍保留旧 Node 路径，仅用于人工回归 oracle，不由生产构造器、普通测试或发布包调用。普通 test/CI 不设置任何 online 开关，因此不会访问 CDN/Device。

## 完整 Solver 离线集成

```bash
go test -count=1 -v ./internal/challenge \
  -run '^TestSolver'
```

完整链 Mock 测试已经证明：

- 成功路径执行一次 Init、一次 Verify，设备 action 顺序为 `Log1,Log2,Log3,Log2`。
- 成功必须得到 `T001`、`VerifyResult=true`、非空 token 和完整阶段耗时。
- Verify 业务拒绝返回完成态结果，并写入脱敏失败样本。
- 低置信在 Verify 前停止，Verify 计数为零。
- Verify 网络错误只尝试一次，错误和 artifact 不泄漏代理凭据或敏感正文。
- 无效或已取消请求在任何网络 action 前停止。

这证明的是确定性 Mock 链路和状态机，不是第三方真实服务成功率。

## 并发、预热池与 HTTP

```bash
go test -count=1 -race ./...
go test -count=1 -v ./internal/challenge \
  -run 'DeviceSessionPool|Solver'
go test -count=1 -v ./internal/server
```

持续门禁包括：

- 视觉 16 路并发结果完全一致。
- 完整 Solver 的 32 路并发 Mock 正确性测试通过；热构建缓存下五次包测试用时 `0.166–0.193s`、maxRSS `155.2–177.6MiB`。
- 最终源码的 `200×32=6,400` 次完整离线 Solve 以 package `31.996s`、命令 wall `32.27s`、maxRSS `242.2MiB` 通过。这仅是含 Go 驱动进程的短时筛查，不是生产内存预算或长稳证明。
- 设备预热池的 `ready + pending + leased` 不超过配置容量，key 不同则冷建，不跨代理或画像复用。
- 池处理过期、取消、失败、异步补货、并发 Lease/Close 和幂等 release。
- `TestConcurrentRequestsAlwaysEnterSolver` 同时发出 64 个合法 HTTP 请求，在 Mock Solver 阻塞期间确认 `64/64` 均已进入；释放后全部返回 HTTP 200，且没有 `Retry-After`。
- `TestEmbeddedAPITestPage` / `TestEmbeddedAPITestPageNonceFailure` 验证 `GET /` 返回内嵌 HTML 且不调用 Solver；POST/HEAD 根路径仍为 404；连续响应使用不同 128-bit nonce，CSP 无 unsafe 指令，页面无外链、浏览器持久化或危险 DOM API；随机源失败时返回脱敏 500 且零 Solver。
- `TestLegacyGETQueryCompatibility` 锁定 GET query `64 KiB` 边界下的旧别名、同名参数最后值、canonical 压过 alias、空 canonical 抑制 alias 并回落默认值，以及未知字段忽略。
- `TestLegacyGETQueryValidationNeverCallsSolver` 验证非法 URL encoding、超长 query/字段、prefix 和 proxy 错误统一返回 `400 ApiRequestError`，且零 Solver。
- `TestHeaderBudgetAcceptsMaximumLegacyQuery` 从真实 `net/http.Server` 层验证 `maxHeaderBytes = server.MaxRequestBytes + 32 KiB`：64 KiB raw query 之外仍有 request line/普通 header 预算，精确上限的 URL 不会在到达 Handler 前被拒绝。
- `TestSolveParameterSourcesStaySeparated` 验证 GET 只读 query、POST 只读 JSON body，两者不合并，并且 URL-encoded/multipart form body 不是支持的 Solve 输入。
- `TestBrowserOriginBoundaryPreservesLegacyClients` 验证 Go 标准库边界对 POST 和 deprecated GET 都拒绝明确浏览器跨源请求且零 Solver；同源、`Sec-Fetch-Site: none` 和无浏览器头的旧客户端仍通过。
- `TestOnlyFrozenRoutesAreExposed` 验证仍只有四个路径，Solve 同时声明 POST 与 deprecated GET，两种方法都只有 200/400/403/500；文档不含 429、`Retry-After`、`TooManyChallenges` 等旧本地过载字段。
- JSON/query/字段校验失败不调用 Client；每个合法 Solve 请求都进入 Solver，业务失败仍返回 HTTP `200`。
- 外层反向代理、负载均衡或 API 网关可能在请求到达进程前返回 429；第三方上游也可能在 Solve 内返回 429。两者必须分开归因，不能写成本服务的本地 admission 429。
- Solver error/panic、普通日志和 artifact 均不泄漏 token、`CertifyId`、代理凭据或原始正文。
- `slider.Client.Close` 等待活跃 Solve/Prime/Purge，再关闭预热会话和空闲连接。

64并发测试是服务合同回归，不是生产容量证明。Handler不设置HTTP在途硬上限；`MaxConcurrency`只限制每route/host连接与PE资源，默认直连Device池满4槽会让更多Solve等待而非返回429。生产来流超过下游消化速度时，goroutine、等待中的Solve、内存和超时仍可增长，详见[性能测试与容量口径](./performance.md#mock-solverhttp-分层容量方法)。

## Linux 双架构构建

完整可运行产物必须同时包含 Go launcher、同架构 V8 wrapper 和许可说明：

```bash
make build-linux-amd64
make build-linux-arm64

file dist/linux-amd64/ali-slider-go
file dist/linux-amd64/libali_slider_v8_runtime.so
```

BuildKit 的 `native-test` 会执行 Rust 测试，`v8-test` 会用当前平台真实 `.so` 执行 Go→C ABI→V8 测试。`CGO_ENABLED=0` 的 Go launcher 依然由 `purego` 使用 `libdl.so.2`，最终镜像为 Debian/glibc。不得将它描述为 `scratch`、单文件或静态 ELF 部署。

## Windows 原生便携包

本地交叉构建命令：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w -buildid=' \
  -o dist/ali-slider-go-windows-amd64.exe ./cmd/server

file dist/ali-slider-go-windows-amd64.exe
```

交叉构建只证明 PE 可以生成。根 CI 的 `windows-package` job 必须在 `windows-2025` 原生完成：

- 全量 `go vet` 与 `go test -count=1 ./...`，包括 Windows Artifact 保存/Purge；
- `CGO_ENABLED=0`、AMD64 console PE 构建；
- 固定 Rust `1.88.0`，原生执行 wrapper Rust 单测，构建 `ali_slider_v8_runtime.dll`；
- 用真实 DLL 执行 `internal/v8runtime` 和 `internal/pe` 的 Go→V8 测试；
- 最终 ZIP 七文件白名单与 EXE、DLL、`THIRD-PARTY-NOTICES.txt` 等全部文件的 SHA-256；
- 解压到含中文和空格的路径后，通过最终 `start.bat` 启动 EXE 并验证参数转发；
- 精确检查内嵌 `GET /` 的 HTML marker、同源 POST API 路径、CSP/no-store/nosniff/no-referrer、`/health`、OpenAPI 3.0.3、deprecated GET 合同与非法 JSON/query `400`；
- 检查启动日志没有 `artifact_purge status=warning`；
- smoke 设置 `--device-prewarm=0`；只 GET 页面/health/OpenAPI，并发送在 Solver 前返回 403 的跨源 POST/deprecated GET 以及返回 400 的非法 JSON/query。跨源 GET 同时带非法 prefix，POST 同时使用非 object `[]`；即使 origin gate 意外回归，后续输入门禁也只能返回 400，不会进入真实 Solver。

PR 执行完整 Windows 验证但不上传。`main` push 和 `workflow_dispatch` 在 Linux quality、Darwin race 与 Windows package 全部成功后，上传保留 30 天的单层 `ali-slider-go-windows-amd64.zip`。下载后的 ZIP 可直接转发，接收者不需要 GitHub 账号。详细合同见 [Windows AMD64 便携包](./windows.md)。

## Docker 功能冒烟

Docker 镜像使用 Go `1.26.5` 构建层、Rust `1.88.0` V8 构建层、Debian bookworm-slim 运行层和非 root UID/GID `65532`。HTTP 服务在 ready 前加载 `.so`、校验 C ABI 并初始化 V8/ICU；以下 smoke 关闭设备预热，不会访问外部 Device/PE：

```bash
docker build --platform linux/amd64 -t ali-slider-go:test .

docker run --detach --rm \
  --name ali-slider-go-smoke \
  --env ALI_SLIDER_HOST=0.0.0.0 \
  --env ALI_SLIDER_DEVICE_PREWARM=0 \
  --publish 127.0.0.1:8000:8000 \
  ali-slider-go:test

curl --fail --silent http://127.0.0.1:8000/health
docker stop ali-slider-go-smoke
```

健康检查仍从宿主执行，避免把调试工具当作应用合同。镜像漏洞扫描与基础镜像更新必须覆盖 Debian/glibc、Rust wrapper、V8 和 ICU。Docker 构建可能需要拉取基础镜像及 Rust crates/V8 预编译库；它不属于“项目测试代码离线执行”的网络范围。

## 离线 CI

根仓库工作流 `.github/workflows/ali-slider-go-ci.yml` 已实现，固定：

- Go `1.26.5`，`GOTOOLCHAIN=local`；
- `staticcheck@v0.7.0`、`govulncheck@v1.1.4`；
- Go/Rust 锁定依赖下载与 `go mod verify` / `cargo --locked`；
- gofmt、vet、staticcheck、unit、integration、race；
- 全项目 `>=80%`、protocol/vision 各 `>=90%`；
- govulncheck；
- `CGO_ENABLED=0` Linux AMD64/ARM64 launcher、真实 `.so` native/ABI 测试及 Debian 镜像 smoke；
- Windows 2025 原生 Go/Rust/真实 DLL 测试、PE/DLL 构建、解压 smoke、文件白名单、SHA-256 和单层 ZIP artifact。

工具下载和漏洞数据库查询可以联网，但测试步骤设置 `GOPROXY=off`，且不配置真实目标凭据，不访问验证码、设备 RPC、CDN或代理。工作流由相关路径的 pull request/push 触发，也可 `workflow_dispatch` 手动运行。

## 当前修复快照功能 smoke

2026-08-08 在确认历史 `d92c7d1` 同机可成功、迁移版持续 `F001` 后，对当前 Device/PE 修复快照只执行 1 个新挑战、并发 1、一次 `Client.Solve`、应用层零重试：

```text
attempts=1 concurrency=1 success=1 businessFailure=0
errorsByKind={} wallP50Ms=1579 wallP95Ms=1579 wallP99Ms=1579 wallMaxMs=1579
```

结果满足严格成功定义：`VerifyCode=T001`、`VerifyResult=true`、token 非空。该样本只证明当前同 VM Device + 原生动态 PE 合同恢复；样本数为 1，不能计算或宣称当前成功率、P95/P99 门槛，且不替代后续正式授权批次。

## 2026-08-11 TRACELESS 无痕组件 smoke

普通离线门禁包含 SDK bridge 的 DOM 原型、`NodeList`、完成消息、`x.alicdn.com` 动态资源白名单和错误脱敏合同：

```bash
go test -count=1 ./internal/pe -run 'TestNodeSDKBridgeTracelessContracts|TestDeviceRuntimeAcceptsBoundTracelessStage'
```

显式在线 smoke 只访问阿里公开 Device、Captcha Init/Verify 和动态资源端点；不调用 PixCake 或其他站点的短信 API，也不记录挑战 ID、token 或业务提交参数：

```bash
env \
  ALI_SLIDER_NODE_TRACELESS_ONLINE=1 \
  ALI_SLIDER_ONLINE_SCENE_ID=wa3238du \
  ALI_SLIDER_ONLINE_PREFIX=1ohgtl \
  go test -tags=online ./internal/pe \
  -run 'TestOnlineNodeTracelessRuntime$' -count=1 -v
```

2026-08-11 的单次结果为 `code=T001 result=true requests=7`。该证据证明当前 PixCake 场景的 TRACELESS 组件合同可完成，不代表短信已发送、完整站点业务已成功，也不能外推成功率或延迟分位数。生产 `slider.Client` 使用同进程 V8 路径；本机缺少可加载的 Darwin V8 wrapper，因此本轮在线 smoke 验证的是同源 JS bridge 的 Node 测试路径，V8 宿主/消息分流只由离线测试覆盖。

## 2026-08-11 SLIDING 拖动组件 smoke

普通离线门禁覆盖 Init 类型自动分流、无双图合同、`418×48`/`370px` 轨迹、CSSStyleDeclaration 空值、元素事件与冒泡、`insertAdjacentHTML`、官方 success Base64、唯一 Init/Verify 及敏感错误脱敏：

```bash
node --check internal/pe/runtime/sdk_device_bridge.mjs
go test -count=1 ./internal/pe ./internal/challenge
```

显式在线 smoke 只访问阿里公开 Device、Captcha Init/Verify、动态资源和采集端点；不访问 DJI 登录或短信接口，也不记录挑战 ID、token 或 success 参数：

```bash
env \
  ALI_SLIDER_NODE_SLIDING_ONLINE=1 \
  ALI_SLIDER_ONLINE_SCENE_ID=159tlu75 \
  ALI_SLIDER_ONLINE_PREFIX=1ulc59 \
  go test -timeout 35s -tags=online ./internal/pe \
  -run 'TestOnlineNodeSlidingRuntime$' -count=1 -v
```

2026-08-11 的最终干净脱敏快照输出为 `SLIDING completed: code=T001 result=true requests=7`，用例约 `5.58s`。`requests=7` 是 bridge 记录数，包含 Device、被本地绑定的 SDK Init、动态资源和真实 Verify，不等于 7 次网络 Verify；运行时严格确认 `InitCaptchaV3` 与 `VerifyCaptchaV3` 记录各出现一次且顺序正确，SDK Init 不产生第二次真实网络 Init。该最终快照的单次结果只证明当前 `SceneId=159tlu75`、`prefix=1ulc59` 的组件合同可以完成；开发期 first-divergence 探针也不构成成功率、性能或容量报告。生产 `slider.Client` 使用同进程 V8；本机没有可加载的 Darwin V8 wrapper，因此在线部分验证 Node bridge，V8 completion 分流和 Go stage 由离线测试覆盖。

## 2026-08-07 历史授权候选批次结果

历史候选批次已在明确授权下恰好执行一次，参数和结果如下；它先于本次 Device/PE 动态边界修复，不得外推到当前架构：

| 指标 | 冻结门槛 | 已验证结果 | 判定 |
|---|---:|---:|---|
| 新挑战数 | 恰好 `200` | `200` | 通过 |
| 同时并发 | `32` | `32` | 通过 |
| 应用重试 | `0` | `0` | 通过 |
| 严格成功 | `>=190/200` | `196/200`（`98%`） | 通过 |
| 业务失败 | 分类记录 | `3` | 已计入总数 |
| 技术错误 | 分类记录 | `VisionError=1`、network `0` | 已计入总数 |
| Client 完整求解链墙钟 P50 | 报告 | `816ms` | 记录 |
| Client 完整求解链墙钟 P95 | `<=1000ms` | `984ms` | 通过 |
| Client 完整求解链墙钟 P99 | 报告，不设 1 秒门槛 | `1018ms` | 记录 |
| Client 完整求解链墙钟 max | 报告 | `1555ms` | 记录 |
| 成功样本墙钟 P95 | 报告 | `989ms` | 记录 |
| 批次总墙钟 | 报告 | 约 `6.16s` | 记录 |

严格成功仍定义为 `VerifyCode == "T001"`、`VerifyResult == true` 且 token 非空。业务失败是已完成的 Verify，不计成功；`VisionError` 是 Verify 前技术失败；网络错误为零。每个 worker 对一个新挑战只调用一次 `Solve`，没有重试分支，网络结果未知也不会换挑战重放。

该批次达到 `>=190/200` 和 Client 完整求解链 `P95<=1000ms`。不得把它改写成 HTTP 端到端 P95 或 `P99<=1s`：它直接调用 `Client.Solve`，实测 P99 为 `1018ms`，最大值为 `1555ms`。该约 6.16 秒短批次没有测量生产形态的 RSS、GC、goroutine 峰值或长时间资源稳定性。

上表是 2026-08-07 的历史证据：`200 / 32 / 0` 不代表当前 HTTP 服务存在 32 路 admission，也不能自动成为后续批次的授权范围。后续若因发布候选变化需要重新验收，必须满足：

1. 总请求数和并发度不得超过当次明确授权；每轮使用新的 `CertifyId`，不重试同一挑战。
2. 并发逐级升高；外层 429、第三方上游 429、网络错误、低置信和业务失败分别计入报告。本服务 Handler 不主动返回 admission 429。
3. 仅当 `VerifyCode == "T001"`、`VerifyResult == true` 且 `securityToken` 非空时计为成功。
4. 在执行前冻结样本数、成功率和延迟门槛；不能用额外挑战替换失败样本。若复用历史门槛，则为 `>=95%`，即 200 样本至少 `190/200`。
5. 如使用当前 harness，以同一批 `Client.Solve` 墙钟计算 P95，目标 `<=1000 ms`；HTTP 端到端必须另用负载发生器测量。
6. 纯本地计算另采足够多的离线逐次样本计算 P99，硬门槛 `<=100 ms`；平均 `ns/op` 不能代替 P99。

后续任何新批次的正式报告必须至少保存 commit、Go 版本、机器规格、授权范围、开始/结束时间、并发度、总数、严格成功数、失败分类、完整链 P50/P95/P99、纯计算 P50/P95/P99 和阶段聚合。不得保存 token、`CertifyId`、代理凭据或上游原始正文。当前唯一授权候选批次没有保留精确起止时间，也没有采集阶段聚合；该缺口已在[脱敏验证证据](./evidence/validation-2026-08-07.md)中显式登记，不能事后补造，因此当前报告不得宣称完全满足本条后续模板。

## Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| `go.mod:3`；`Dockerfile:3`；`.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `windows-package` | module、Docker 构建层和 CI 统一固定 Go `1.26.5`。 | source → pinned toolchain → reproducible gates |
| `.github/workflows/ali-slider-go-ci.yml` · `quality` / `race` / `linux-runtime` / `windows-package` | Linux AMD64/ARM64 用真实 `.so`，Windows 用真实 DLL；只有平台 native/ABI/test/smoke 通过才上传产物。 | PR/push → cross-platform Go/Rust/V8 gates → verified artifacts |
| `internal/protocol/protocol_test.go:47`、`:125`、`:211` | 协议关键输出由静态 Python oracle 锁定。 | Python fixture → Go primitives → equality |
| `internal/vision/solver_test.go:17` | edge-decoy 正负 fixture 已通过跨语言静态对照。 | PNG fixture → Go vision → accept/reject assertion |
| `internal/pe/builder_test.go`、`keys_test.go`、`runtime_test.go`、`v8_runtime_test.go`、`device_runtime_test.go` | PE 保留 Python oracle；SDK 5 分钟软 TTL、PE 30 分钟硬 TTL、分片 singleflight、V8/纯 Go 差分、路径/下载边界、Device recycle 和关闭生命周期均由测试锁定。 | profile + exact StaticPath + current V8 oracle → verified pure-Go or V8 fallback → independently checked output |
| `internal/challenge/solver_test.go` · `TestSolverOfflineCompleteSuccess` / `TestSolverUsesResolvedDynamicPEKey` / `TestSolverClassifiesPEKeyFailuresBeforeAssetsAndVerify` | 完整离线链覆盖动态 runtime 注入、稳定错误分类和 Verify 前停止。 | Device → Init → PE profile → Vision/native PE → guarded Verify |
| `internal/challenge/device_pool_test.go:138`、`:177`、`:930` | 预热池覆盖有界并行、key 隔离及 Lease/Close 竞态。 | Prime/Lease → bounded ownership → Close |
| `internal/server/server.go` · `checkSolveOrigin` / `decodeQueryRequest` / `handleSolve`；`internal/server/server_test.go` · `TestLegacyGETQueryCompatibility` / `TestLegacyGETQueryValidationNeverCallsSolver` / `TestSolveParameterSourcesStaySeparated` / `TestConcurrentRequestsAlwaysEnterSolver` / `TestBrowserOriginBoundaryPreservesLegacyClients`；`cmd/server/main_test.go` · `TestHeaderBudgetAcceptsMaximumLegacyQuery` | POST JSON 与 deprecated GET query 的输入、分源、HTTP header 预算和跨源合同有回归；HTTP 无本地 admission，64 个合法并发 POST 全部进入 Solver。 | origin + JSON/query gate → every valid Solve entered → release → 200 |
| `internal/server/testpage.go`；`internal/server/server_test.go` · `TestEmbeddedAPITestPage` | 页面嵌入二进制，GET 不调用 Solver，nonce/安全头/无外链和无持久化合同均有离线回归。 | embedded source → GET `/` → constrained browser page |
| `internal/server/openapi.go` · `legacyQueryParameters` / `solveResponses`；`internal/server/server_test.go` · `TestOnlyFrozenRoutesAreExposed` | OpenAPI 描述四路径和 Solve POST/deprecated GET；两种方法只声明 200/400/403/500，且回归禁止 429、`Retry-After` 和旧本地过载字段。 | generated contract → positive/negative assertions → stable surface |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165`；`internal/device/online_session_test.go:17` | Device schema 按 action 解码，离线回归与约 `0.53s` 在线 Log1/2/3 探针通过。 | raw JSON → Log1 DeviceConfig / Log2-3 Code → authorized probe |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 32 路 Mock 正确性与 6,400 次压力通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛。 | concurrent mock / repeated stress / local sampler → verified compute and capacity gates |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 候选 200/32/应用层零重试批次严格成功 `196/200`，Client 完整求解链 P95 `984ms`；不经过 HTTP Handler，最终 one-shot 加固后未在线重跑。 | authorized jobs → one Solve each → aggregate summary → bounded acceptance |
| `Makefile` · `build-linux-amd64` / `build-linux-arm64`；`Dockerfile` | BuildKit 同架构执行 Rust native 测试和 Go→V8 ABI 测试，导出 launcher + `.so` + notices，最终使用 Debian/glibc。 | Go + Rust source → native/ABI tests → Linux package/image |
| `Makefile` · `build-windows`；`packaging/windows/start.bat`；CI `windows-package` | Windows console 服务与静态 CRT V8 DLL/第三方许可说明一同打包，双击入口显式选择包内 DLL。 | Go + Rust source → native Windows ABI tests → verified ZIP → local API |
| `internal/vision/solver_test.go:260` | 困难视觉 benchmark 只产生均值和分配，不能提供 P99。 | fixture → repeated Solve → mean only |
