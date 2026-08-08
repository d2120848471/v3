# 2026-08-07 脱敏验证证据

本文固化 `ali-slider-go` 的离线质量、性能、静态构建、容器和授权在线批次证据。只记录命令、环境、聚合数字和稳定分类；不保存 token、`CertifyId`、代理凭据、原始响应、挑战图片或 artifact 文件名。

> **后续合同变更（2026-08-08）**：本页仅属于 2026-08-07 快照。后续源码已删除 HTTP Handler 的本地 admission gate、主动 429 和 `Retry-After`；每个通过输入校验的 POST 请求直接进入 Solver。本页的 44 文件摘要、Linux 二进制哈希和 Docker 镜像不包含该后续变更，不得用作当前 HTTP 合同的发布证据。历史 200/32 Client harness 结果保留不改写；当前离线证据见 [2026-08-08 无本地 admission 验证](./validation-2026-08-08-no-local-admission.md)。

> 工作树尚未提交，不能用发布 commit 标识本次快照。选定生产源码与 fixture 的 44 文件选择集摘要为 `c590140a3734b0548077ee921bd4a688f46f378272b28238d49f541cd418f7de`；它不是发布 manifest、SBOM 或全部验证输入。Linux 二进制另有独立哈希，发布 commit 仍必须由 CI 重跑门禁。

该 44 文件选择集包含非测试 Go 源码、`go.mod`、`Dockerfile`、JSON 与 PNG fixture；包含离线 oracle/视觉 fixture，不包含测试源码、Makefile、CI 或文档。当前目录为 module 根时可按下列命令复算：

```bash
find . -type f \( \( -name '*.go' ! -name '*_test.go' \) \
  -o -name go.mod -o -name Dockerfile -o -name '*.json' -o -name '*.png' \) -print0 |
  sort -z |
  xargs -0 shasum -a 256 |
  sed 's#  \./#  #' |
  shasum -a 256
```

## 1. 结论矩阵

| 门禁 | 冻结条件 | 实测 | 判定与边界 |
|---|---:|---:|---|
| 纯计算 P99 | `<=100ms` | `57.05075ms` | 通过；仅 `Vision → Track → PE/Data`，不含网络 |
| 授权在线严格成功 | `>=190/200` | `196/200` | 已测候选通过；见第 7 节快照时序 |
| Client 完整求解链 P95 | `<=1000ms` | `984ms` | 已测候选通过；不是 HTTP 端到端 |
| 在线总数/并发 | `200/32` | `200/32` | 通过；每个 job 只调用一次 `Client.Solve` |
| 全项目覆盖率 | `>=80%` | `83.0%` | 通过 |
| protocol / vision | 各 `>=90%` | `91.4% / 91.9%` | 通过；PE 为 `95.1%` |
| 离线 Solver 压力 | 32 路正确性与短时稳定性 | `200×32=6,400` 次 PASS | 通过；不是生产长稳或 HTTP 压测 |
| Linux AMD64 静态构建 | `CGO_ENABLED=0`、无动态节 | PASS | 通过 |
| Docker smoke | AMD64、非 root、`/health` ready | PASS | 通过；预热关闭，未访问真实目标 |
| 在线审计元数据 | 精确起止时间、阶段聚合 | 未保留/未采集 | 候选批次证据缺口；不得事后补造 |
| HTTP 端到端 P95 | 未冻结为本批 harness 路径 | 未测 | 不得用 Client P95 替代 |
| 生产长稳资源 | RSS/GC/goroutine/持续吞吐 | 未测 | 短时 maxRSS 不能外推 |

## 2. 快照与在线时序边界

正式授权批次发生在最终传输 one-shot 加固之前：

1. 批次 harness 为每个 job 只调用一次 `Client.Solve`，没有应用层重试分支；聚合结果中网络错误为 0。
2. 批次当时没有 instrument `net/http` 内部透明重放，因此不能从历史输出反证 HTTP/2 GOAWAY/REFUSED_STREAM 场景绝未触发隐藏回卷。没有证据表明该批发生过透明重放，但当时也没有线级观测证据。
3. 终审随后把 Captcha Verify 和所有一次性 Device RPC POST 的 `Request.GetBody` 清空，同时保留 `ContentLength`；离线回归明确断言 `GetBody == nil`。这使已发送 body 在网络结果未知时不可回卷重发。
4. 终审还补上零值 `ClientOptions{}` 的默认预热容量，以及正式 200 轮必须并发 32、成功至少 190、Client P95 不超过 1000ms 的 test-only gate。
5. 冻结授权上限已经用完，没有为了这些加固再生成真实挑战。

因此：`196/200` 与 `P95=984ms` 是唯一授权候选批次的真实实测；最终源码的 one-shot 语义已由源码和离线测试闭合，但最终源码没有第二份在线 200 轮报告。若组织把“任何生产代码变更后必须重跑在线批次”设为发布门禁，需要新的书面授权和新批次，不能复用或扩大本次授权。

## 3. 环境

```text
OS/arch:       Darwin 24.6.0 arm64
CPU:           Apple M3 Max
logical CPU:   16
physical CPU:  16
memory:        51,539,607,552 bytes (48 GiB)
Go:            go1.26.5 darwin/arm64
```

项目门禁均显式使用 `CGO_ENABLED=0`。Go 1.26 的 Linux race runtime 强制要求 CGo，因此根 CI 把普通 Linux 门禁和 Darwin race 拆成两个 job；两者仍固定 `CGO_ENABLED=0`。

## 4. 离线质量门禁

执行入口：

```bash
CGO_ENABLED=0 GOPROXY=off go test -count=1 ./...
CGO_ENABLED=0 GOPROXY=off go test -count=1 -race ./...
CGO_ENABLED=0 GOPROXY=off go vet ./...
CGO_ENABLED=0 GOPROXY=off staticcheck ./...
CGO_ENABLED=0 GOPROXY=off govulncheck ./...
test -z "$(gofmt -l .)"
git diff --check -- .
GOPROXY=off go list -m all
GOPROXY=off go mod verify
```

结果：

```text
go test:       12/12 packages PASS
go test -race: 12/12 packages PASS, no race report
go vet:        PASS, no output
staticcheck:   PASS, no output (v0.7.0 / 2026.1)
govulncheck:   No vulnerabilities found (v1.1.4, DB 2026-07-27)
gofmt:         PASS, no changed files
diff check:    PASS, no whitespace error
module list:   only github.com/d2120848471/v3/ali-slider-go
mod verify:    all modules verified
```

普通 CI 没有 `online` build tag，也没有在线环境开关。额外执行下列命令只编译 online 测试并得到 SKIP，没有访问真实目标：

```bash
CGO_ENABLED=0 GOPROXY=off go test -count=1 -tags online ./internal/device ./pkg/slider
```

### 覆盖率

统一全项目 coverprofile 的最终输出：

```text
total:              83.0%
internal/protocol:  91.4%
internal/vision:    91.9%
internal/pe:        95.1%
```

### Fuzz 短跑

四个 target 均使用 `-run '^$' -fuzz '^<target>$' -fuzztime=1s`，最终结果：

| Target | executions | 结果 |
|---|---:|---|
| `FuzzUnpackData` | `136,357` | PASS |
| `FuzzProtocolDecoders` | `120,439` | PASS |
| `FuzzPercentAndFormEncoding` | `94,577` | PASS |
| `FuzzPNGPairSeeds` | `128,522` | PASS |

execution 数受调度影响，只用于证明短跑实际执行；不是稳定性能指标。

## 5. 纯计算硬门槛

命令：

```bash
CGO_ENABLED=0 GOPROXY=off ALI_SLIDER_PERF=1 GOMAXPROCS=16 \
  go test -count=1 -run '^TestPureComputeP99$' -v ./internal/challenge
```

真实输出：

```text
pure-compute samples=200
p50=50.577458ms
p95=55.711708ms
p99=57.05075ms
max=60.337542ms
PASS
```

统计使用 200 个顺序样本和 nearest-rank。测试范围是固定困难 PNG fixture 的 `Vision → Track → PE/Data`，不含图片下载、Device、Captcha Init/Verify、HTTP handler 或调用方网络。

## 6. 32 路离线 Solver 容量筛查

命令：

```bash
/usr/bin/time -l env CGO_ENABLED=0 GOPROXY=off \
  go test ./internal/challenge \
    -run '^TestSolverOffline32ConcurrentSuccess$' \
    -count=200 -timeout=2m
```

真实输出摘要：

```text
200 × 32 = 6,400 complete offline Solve
package test: 31.996s
command wall: 32.27s
maximum resident set size: 253,968,384 bytes (242.2 MiB)
PASS
```

每个 32 路测试批次要求恰好 32 Init、32 Verify、128 次 Device action，并要求所有结果满足 `T001`、`VerifyResult=true`、token 非空。传输是内存 Mock，图片是本地 fixture；wall 和 maxRSS 包含 Go 驱动、热构建缓存检查与测试进程。该数据只支持短时并发正确性和资源筛查，不证明生产内存预算、绝对无泄漏、HTTP admission 延迟或长时间稳定性。

## 7. 唯一授权在线批次

命令：

```bash
ALI_SLIDER_ONLINE=1 \
ALI_SLIDER_ONLINE_COUNT=200 \
ALI_SLIDER_ONLINE_CONCURRENCY=32 \
ALI_SLIDER_ONLINE_ARTIFACT_DIR=/tmp/ali-slider-go-online-acceptance.rCYCX2 \
go test -count=1 -tags online -run '^TestOnlineAcceptance$' -v ./pkg/slider
```

脱敏聚合输出：

```text
attempts=200
concurrency=32
application retry=0
strict success=196
business failure=3
errorsByKind={VisionError:1}
network error=0
success rate=98%
Client wall P50/P95/P99/max=816/984/1018/1555ms
successful Client wall P95=989ms
batch wall≈6.16s
```

证据缺口：当次 runner 输出没有保留可审计的精确开始/结束时间，也没有输出各求解阶段的批次聚合；现有 `batch wall≈6.16s` 只来自测试墙钟摘要。这些数据无法从当前聚合结果可靠重建，故明确记为“未保留/未采集”，不得补造。它不改变 200 次分类、严格成功率和 Client 墙钟分位数的实测值，但意味着该候选批次不满足后续完整发布报告模板；任何新批次都必须在发送首个挑战前启用相应采集。

严格成功定义为 `VerifyCode == "T001" && VerifyResult && securityToken != ""`。harness 直接调用 `pkg/slider.Client.Solve`，不经过 HTTP Handler；这些分位数不能写成 HTTP 端到端分位数。P95 目标通过，但 P99 为 1018ms，项目没有“P99 或所有调用小于 1 秒”的结论。

失败证据目录只做聚合校验：

```text
managed files: 12
managed sets: 4
total bytes: 208,896
directory mode: 0700
file modes: 0600
```

4 组分别对应 3 个 Verify 业务拒绝和 1 个低置信失败。文件名、图片和 metrics 正文不写入本文。服务进程会启动清理并每小时清理；独立 library 调用方必须定期调用 `PurgeArtifacts`。

## 8. Linux AMD64 静态构建

命令：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOPROXY=off \
  go build -trimpath -ldflags='-s -w -buildid=' \
  -o /tmp/ali-slider-go-linux-final.hDix28/ali-slider-go-linux-amd64 \
  ./cmd/server
```

结果：

```text
file: ELF 64-bit LSB executable, x86-64, statically linked, stripped
Go: go1.26.5
CGO_ENABLED=0 GOOS=linux GOARCH=amd64
dynamic section: none
size: 7,692,414 bytes
sha256: a9493e22ad71ef7877c68594fe0bf200c271e937c0986ef44c5f8c373f837374
```

路径位于临时目录，不是仓库发布物；哈希用于定位本次验证产物。

## 9. Docker AMD64 smoke

构建与运行：

```bash
docker build --platform linux/amd64 -t ali-slider-go:final-20260807 .
docker run --rm -d --platform linux/amd64 \
  -e ALI_SLIDER_HOST=0.0.0.0 \
  -e ALI_SLIDER_DEVICE_PREWARM=0 \
  -p 127.0.0.1::8000 \
  ali-slider-go:final-20260807
```

结果：

```text
image id: sha256:c91a2b7ab19fec1cb48fe080ce06cbbea9ebac988f0cec24c5dd91cc72837247
architecture/os: amd64/linux
image size: 3,367,458 bytes
configured user: 65532:65532
GET /health: {"ok":true,"status":"ready"}
```

smoke 显式关闭设备预热，只验证容器启动、非 root 用户、端口与 health；没有访问 Device/Captcha/CDN。临时 smoke 容器已停止并由 `--rm` 删除，镜像保留在本机。

## 10. Evidence → Finding → Path

| Evidence | Finding | Path |
|---|---|---|
| 全量 test/race/vet/staticcheck/govulncheck 输出 | 该 2026-08-07 快照源码的离线正确性、竞态与静态门禁通过。 | source → offline gates → PASS |
| 单一 coverprofile | 总覆盖率 `83.0%`，protocol/vision 达到核心门槛。 | tests → one profile → threshold |
| 200 样本逐次计时 | 纯计算 P99 `57.05075ms`，硬门槛通过。 | fixture → Vision/Track/PE → nearest-rank |
| 6,400 次完整 Mock Solve | 32 路 Solver 链短时正确性与资源筛查通过。 | local assets + memory transport → concurrent Solver → PASS |
| 唯一授权聚合输出 | 候选批次成功率 `98%`、Client P95 `984ms`；不是 HTTP E2E。 | authorized jobs → one Solve/job → aggregate |
| 缺失的精确起止时间与阶段聚合 | 当前候选证据不满足后续完整发布报告模板，且不可事后补造。 | historical runner output → explicit gap → future hard requirement |
| `GetBody == nil` 回归 + 单次 Verify 状态机 | 最终源码禁止已发送的一次性 POST 被回卷重发。 | issued CertifyId → irreversible attempt → one-shot body |
| 静态 ELF 与 Docker health | Linux AMD64 无动态节，scratch/non-root 容器可启动。 | source → CGO0 build → image → health |
| 快照时序说明 | 在线数字没有在 one-shot 加固后的最终源码上重跑。 | candidate online evidence → audit delta → new authorization if required |
