# 目录与接口迁移

本次迁移把单轮业务编排、共享生命周期、纯计算、外部适配和生产组装分成真实包边界。Go module 根仍为 `ali-slider-go`，公共包仍为 `github.com/d2120848471/v3/ali-slider-go/pkg/slider`；HTTP 字段、别名、状态、OpenAPI 操作和 Go SDK 导出签名保持兼容。

## 对使用方的影响

现有 HTTP 和 SDK 调用无需因目录重构改路径。新 HTTP 接入继续使用 POST JSON，旧 GET query 仅作已废弃的兼容入口；结果与错误语义见 [HTTP API](../reference/http-api.md)。SDK 的请求、结果、选项、方法与错误类型以 [公共包](../../pkg/slider) 为准。

内部包不构成稳定 API，本次不保留旧 internal 路径的别名或空壳。仓库内开发代码、测试、构建及文档需要使用新路径。`native/v8runtime` 继续是独立 Rust 项目，Cargo manifest 和 release 库位置不变。

## Go 源码映射

| 原位置/职责 | 当前位置 |
|---|---|
| `cmd/server` 中的配置/生产组装/HTTP 运行 | `internal/bootstrap/server` 运行 HTTP，`internal/bootstrap` 组装共享资源，`internal/bootstrap/config` 解析配置；cmd 只保留进程入口 |
| `pkg/slider` 中的共享资源构造与关闭协调 | `internal/bootstrap` 组装，`internal/application/service` 协调；公共包保留映射 |
| `internal/challenge` 的 Solver 编排 | `internal/application/solve` |
| `internal/challenge` 的 RPC、图片与单轮适配状态 | `internal/infrastructure/aliyun` |
| `internal/challenge` 的代理/连接池/SOCKS | `internal/infrastructure/httpclient` |
| `internal/server` | `internal/interfaces/httpapi` |
| `internal/pe` 的纯 Builder 与输入/结果 | `internal/domain/pe` |
| `internal/pe` 的 Device 宿主、公开脚本、差分与 fallback | `internal/infrastructure/engine` |
| `internal/device` 的画像、指纹与 DTO | `internal/domain/device` |
| `internal/device` 的兼容 RPC/session | `internal/infrastructure/aliyun/device` |
| `internal/protocol`、`internal/track`、`internal/vision` | `internal/domain/protocol`、`track`、`vision` |
| `internal/runtimekit` | `internal/foundation/runtimekit` |
| `internal/v8runtime` | `internal/platform/v8runtime` |
| `internal/artifact` | `internal/infrastructure/artifact` |
| `internal/config` | `internal/bootstrap/config` |
| `pkg/slider/online_acceptance_test.go` | `internal/bootstrap/online_acceptance_test.go`；在线测试仍须显式授权/开关 |

`application/solve` 定义 `RoundFactory`、`Round` 与 `FailureRecorder`，Aliyun/存储实现适配这些合同；它不导入 infrastructure、HTTP 或 SDK。`service.Service` 定义 `Executor` 与 `Resources`，生产实现通过 `bootstrap.NewService` 注入。HTTP 直接消费应用合同，SDK 映射到同一个 service；资源不再由公共 SDK 独占组装。实际调用关系和所有权见 [架构](../design/architecture.md)。

## 构建与文档映射

| 原路径 | 新路径/用法 |
|---|---|
| `Dockerfile` | `build/docker/Dockerfile`；构建始终以 module 根为 context |
| `packaging/windows` | `build/packaging/windows`；ZIP 内文件名和位置不变 |
| `docs/testing.md` | `docs/guides/development.md` |
| `docs/windows.md`、`troubleshooting.md`、`migration.md` | `docs/guides/` 中对应名称 |
| README 中的详细启动步骤 | `docs/guides/getting-started.md` |
| `docs/api.md`、`configuration.md` | `docs/reference/http-api.md`、`configuration.md` |
| `docs/architecture.md`、`performance.md`、`security.md` | `docs/design/` 中对应名称 |
| `docs/evidence` | `docs/archive/evidence` |
| `docs/2026-*.md` 调查报告 | `docs/archive/reports` |

Docker 命令由 module 根执行：

```bash
docker build --platform linux/amd64 \
  -f build/docker/Dockerfile -t ali-slider-go:local .
```

Makefile 和 CI 已使用新路径。`make bench` 指向 domain/vision 与 interfaces/httpapi，真实 V8 Go 测试指向 platform/v8runtime 与 infrastructure/engine。`make test` 与 CI 的 `go test ./...` 包含 [架构与公开 API 检查](../../tests/architecture)。版本和全项目 80%/核心包 90% 覆盖率门槛未变。

## 生命周期兼容

`NewClient` 创建本地共享资源，`CheckRuntime` 校验本地动态库；二者不请求验证码上游。HTTP 启动器做同样的本地自检，并在启动及每小时清理失败样本。每次 Solve 独占 Device/V8 与挑战状态；Puzzle 在 Complete 后、Verify 前即可关闭 Device，所有失败和取消路径也释放资源。

`Prime` 继续是兼容空操作。`Close` 拒绝新工作、等待活动调用、关闭共享 engine/wrapper 与 Transport，不主动取消 Solve。`DevicePrewarmCapacity`、`DeviceSessionReserve` 及对应启动参数只接受 0，`MaxConcurrency` 仍只控制出站连接和 PE 空闲保留量。内部目录变化没有恢复预热、会话复用或本地 HTTP/Device 并发闸门。

## Python 对照与历史证据

旧 Python 源码保留在 Git 历史提交 `0509bfd`，此前指定的对照提交为 `d92c7d1`。当前工作树不维持 Python/Go 双栈。带 Python 名称的 JSON/PNG 是静态 oracle，Go 测试不启动 Python；Node bridge 只作可选测试 oracle，生产使用同进程 V8。

| 旧职责 | 当前代码 |
|---|---|
| 编码、签名、token、data codec | `internal/domain/protocol` |
| 画像与指纹 | `internal/domain/device` |
| PE payload 纯计算 | `internal/domain/pe` |
| 动态 Device/PE 脚本执行 | `internal/infrastructure/engine` + `internal/platform/v8runtime` + 独立 Rust wrapper |
| 网络、代理和上游会话 | `internal/infrastructure/httpclient`、`internal/infrastructure/aliyun` |
| 视觉与轨迹 | `internal/domain/vision`、`internal/domain/track` |
| 单轮流程与对外服务 | `internal/application`、`internal/interfaces/httpapi`、`internal/bootstrap` |

历史近似纯 Go、Node、预热池和组件 smoke 描述各自快照，不能作为当前冷建路径的线上正确性、性能或容量结论。原始正文和命令保留于 [归档索引](../README.md#历史证据)，当前前轮离线优化数字的范围见 [性能说明](../design/performance.md)。

部署切换只影响新请求，不迁移在途挑战，也不把已尝试 Verify 的请求交给另一实现重试。完整交付需要匹配的 Go launcher、V8 wrapper、notices 和配置；发布判断仍需对应 commit 的原生平台测试及实际部署验收，目录迁移本身不提供这些结论。
