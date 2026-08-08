# 测试与质量门禁

> **当前结论**：Go `1.26.5` 下，全量 test、race、vet、staticcheck、govulncheck、Linux AMD64 静态构建及 Docker 非 root `/health` smoke 已验证通过；统一覆盖率为 `83.0%`，protocol/vision/PE 分别为 `91.4%/91.9%/95.1%`。Device Log1/2/3 在线探针约 `0.53s` 通过。Mac ARM64 纯计算 200 样本 `P99=57.05075ms`，达到 `<=100ms`；2026-08-07 唯一授权候选批次恰好 200 轮、并发 32、每个 job 一次 Solve、应用层零重试，严格成功 `196/200`，Client 完整求解链墙钟 `P95=984ms`，达到成功率和 P95 两项冻结门槛。1 秒只约束 P95；本批 `P99=1018ms`、`max=1555ms`，且不经过 HTTP Handler。该批先于传输 one-shot 加固，最终源码未获授权再跑第二批。离线 6,400 次 Solve 压力已通过；HTTP 64 并发直通与 OpenAPI 无本地 429 已回归，生产形态的无界在途资源风险和长时间稳定性仍未完成压测。

## 测试原则

- 普通开发测试与 CI 的项目代码执行必须离线，不访问真实验证码、设备 RPC、图片 CDN或第三方代理。
- Python 只用于迁移阶段生成确定性 oracle。Go 测试只读取已提交的 JSON/PNG fixture，运行时不启动 Python。
- 正确性优先于性能；oracle、负例、唯一 Verify 或竞态测试失败时，性能结果无效。
- 在线验收必须有明确授权、由人工显式发起，并与普通 CI 隔离。
- 同一个 `CertifyId` 最多尝试一次 Verify；网络结果未知也视为已经消耗尝试位。
- 覆盖率必须从同一源码快照、同一份全项目 `coverprofile` 计算，不能拼接不同时间点的包级数字。

## 分层测试模型

| 层级 | 目的 | 主要入口 | 网络策略 | 当前状态 |
|---|---|---|---|---|
| L0 单元 | 编码、加密、配置、设备、PE、视觉、轨迹和边界分支 | 各包 `*_test.go` | 禁止真实外网 | 通过 |
| L1 跨语言 oracle | 锁定 Python 与 Go 的协议和困难视觉语义 | `protocol/testdata`、`vision/testdata`、PE oracle | 静态 fixture | 通过 |
| L2 组件集成 | 验证设备 RPC schema、Captcha RPC、代理、下载、预热池及清理 | `internal/device`、`internal/challenge`、`internal/artifact` | Fake `RoundTripper` | 通过 |
| L3 完整离线链 | 验证 Device → Init → Assets → Vision → PE → Device Complete → Verify | `solver_test.go` | 单一 Mock transport | 通过 |
| L4 服务合同 | HTTP 别名、状态码、OpenAPI、64 并发直通、无本地主动 429、日志脱敏、启动配置 | `server_test.go`、`cmd/server/main_test.go` | Mock Solver/占用端口 | 通过 |
| L5 工程门禁 | fmt、vet、test、race、coverage、staticcheck、govulncheck、静态构建 | `Makefile`、Go CI | Linux 普通门禁 + Darwin race 均为 `CGO_ENABLED=0`；工具安装/漏洞库可联网 | 通过；发布 commit 重跑 |
| L6 授权在线 | Device 探针及 200 个新挑战、32 并发、每 job 一次 Solve、应用层零重试、成功率和 Client 完整求解链 P95 | `online` build tag 测试 | 显式授权 | 候选批次通过；one-shot 加固后未获授权重跑 |

## Go 版本与快速验证

模块、CI 和 Docker 构建层统一固定 Go `1.26.5`。以下命令从 `ali-slider-go` 目录执行：

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
make build-linux
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
| 全项目语句覆盖率 | `>= 80%` | `83.0%` | 通过；发布 commit 仍由 CI 重跑 |
| `internal/protocol` | `>= 90%` | `91.4%` | 通过；包含 fuzz seeds |
| `internal/vision` | `>= 90%` | `91.9%` | 通过 |
| `internal/pe` | 纳入全项目门禁 | `95.1%` | 通过 |

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
- OpenAPI Solve response 只声明 200/400/500；测试同时断言文档不含 429、`Retry-After`、`TooManyChallenges` 等旧本地过载字段。
- JSON/字段校验失败不调用 Client；每个合法请求都进入 Solve，业务失败仍返回 HTTP `200`。
- 外层反向代理、负载均衡或 API 网关可能在请求到达进程前返回 429；第三方上游也可能在 Solve 内返回 429。两者必须分开归因，不能写成本服务的本地 admission 429。
- Solver error/panic、普通日志和 artifact 均不泄漏 token、`CertifyId`、代理凭据或原始正文。
- `slider.Client.Close` 等待活跃 Solve/Prime/Purge，再关闭预热会话和空闲连接。

64 并发测试是服务合同回归，不是生产容量证明。当前 Handler 不设置 HTTP 在途硬上限；`MaxConcurrency` 只限制 Client 每 route/host 的出站连接与预热资源。生产来流超过下游消化速度时，goroutine、等待中的 Solve、内存和超时可继续增长，相关耗尽边界尚未实测，详见[性能测试与容量口径](./performance.md#mock-solverhttp-分层容量方法)。

## Linux 静态构建

正式命令构建可部署的 `cmd/server`，不是仅编译包：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' \
  -o dist/ali-slider-go-linux-amd64 ./cmd/server

file dist/ali-slider-go-linux-amd64
```

当前验证结果为 Linux x86-64、statically linked、stripped，且全链使用纯 Go。该结果证明构建属性，不证明 Linux 与 Mac 的性能等价。

## Docker 功能冒烟

Docker 镜像使用 Go `1.26.5` 构建层、`scratch` 运行层、静态二进制和非 root UID/GID `65532`。当前 Linux AMD64 镜像构建及关闭预热后的非 root `/health` smoke 已通过。以下是复现命令；它只访问 `/health`，不会触发设备外部请求：

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

`scratch` 镜像内没有 shell 或 curl，健康检查应从宿主执行。Docker 构建可能需要拉取基础镜像；它不属于“项目测试代码离线执行”的网络范围。

## 离线 CI

根仓库工作流 `.github/workflows/ali-slider-go-ci.yml` 已实现，固定：

- Go `1.26.5`，`GOTOOLCHAIN=local`；
- `staticcheck@v0.7.0`、`govulncheck@v1.1.4`；
- 零生产依赖检查与 `go mod verify`；
- gofmt、vet、staticcheck、unit、integration、race；
- 全项目 `>=80%`、protocol/vision 各 `>=90%`；
- govulncheck；
- `CGO_ENABLED=0` Linux AMD64 静态二进制及 `file` 断言。

工具下载和漏洞数据库查询可以联网，但测试步骤设置 `GOPROXY=off`，且不配置真实目标凭据，不访问验证码、设备 RPC、CDN或代理。工作流由相关路径的 pull request/push 触发，也可 `workflow_dispatch` 手动运行。

## 唯一授权候选批次结果

候选批次已在明确授权下恰好执行一次，参数和结果如下：

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
| `go.mod:3`；`Dockerfile:3`；`.github/workflows/ali-slider-go-ci.yml:41` | module、Docker 构建层和 CI 统一固定 Go `1.26.5`。 | source → pinned toolchain → reproducible gates |
| `.github/workflows/ali-slider-go-ci.yml:32`、`:62`、`:70`、`:76`、`:94`、`:97`、`:107`、`:115`、`:131` | Linux quality job 在 `CGO_ENABLED=0` 下实现静态分析、全量 test、覆盖率、漏洞扫描和静态构建；Darwin job 在同一纯 Go 边界运行 race。 | PR/push → Linux quality + Darwin race → release evidence |
| `internal/protocol/protocol_test.go:47`、`:125`、`:211` | 协议关键输出由静态 Python oracle 锁定。 | Python fixture → Go primitives → equality |
| `internal/vision/solver_test.go:17` | edge-decoy 正负 fixture 已通过跨语言静态对照。 | PNG fixture → Go vision → accept/reject assertion |
| `internal/pe/builder_test.go:96` | PE 对 Python oracle 做完整解包语义对照。 | profile + track → PE build → Pack/Unpack equality |
| `internal/challenge/solver_test.go:227`、`:268`、`:286` | 完整离线链覆盖成功、前置零 Verify 和网络错误单次 Verify。 | Device → Init → Vision/PE → guarded Verify |
| `internal/challenge/device_pool_test.go:138`、`:177`、`:930` | 预热池覆盖有界并行、key 隔离及 Lease/Close 竞态。 | Prime/Lease → bounded ownership → Close |
| `internal/server/server.go:41`、`:105`、`:116`；`internal/server/server_test.go:260` | HTTP 无本地 admission；64 个合法并发请求全部进入 Solver 并返回 200。 | concurrent valid POST → every Solve entered → release → 200 |
| `internal/server/openapi.go:20`；`internal/server/server_test.go:239`、`:253` | OpenAPI 只声明 200/400/500，且回归禁止 429、`Retry-After` 和旧本地过载字段。 | generated contract → negative assertions → no local 429 |
| `internal/device/rpc.go:119`、`:130`；`internal/device/session_test.go:130`、`:146`、`:150`、`:165`；`internal/device/online_session_test.go:17` | Device schema 按 action 解码，离线回归与约 `0.53s` 在线 Log1/2/3 探针通过。 | raw JSON → Log1 DeviceConfig / Log2-3 Code → authorized probe |
| `internal/challenge/performance_test.go:21`、`:65`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 32 路 Mock 正确性与 6,400 次压力通过；200 样本纯计算 `P99=57.05075ms`，达到硬门槛。 | concurrent mock / repeated stress / local sampler → verified compute and capacity gates |
| `pkg/slider/online_acceptance_test.go:37`、`:49`、`:98`、`:128`、`:147`、`:165`、`:168`；[脱敏验证证据](./evidence/validation-2026-08-07.md) | 候选 200/32/应用层零重试批次严格成功 `196/200`，Client 完整求解链 P95 `984ms`；不经过 HTTP Handler，最终 one-shot 加固后未在线重跑。 | authorized jobs → one Solve each → aggregate summary → bounded acceptance |
| `Makefile:40`；`Dockerfile:14` | 本地和容器均从 `cmd/server` 生成 `CGO_ENABLED=0` 静态二进制。 | Go source → Linux AMD64 binary → scratch image |
| `internal/vision/solver_test.go:260` | 困难视觉 benchmark 只产生均值和分配，不能提供 P99。 | fixture → repeated Solve → mean only |
