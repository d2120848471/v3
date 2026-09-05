# Ali Slider Go

本仓库的主要模块是 [`ali-slider-go`](./ali-slider-go/README.md)。Go 主进程通过 `purego` 加载 Rust wrapper 提供的 V8 原生动态库；不使用 CGo，不等于不依赖动态库。根 README 负责项目概览与文档导航，模块 README 负责当前实现、构建和使用说明。

> 仅用于自有系统或获得明确授权的研究、兼容性验证与测试环境。服务没有应用内鉴权，默认只监听 `127.0.0.1:8000`，不应把未加保护的端口直接暴露到公网。历史测试授权、实验次数与环境开关不构成新的上游访问授权。

## 运行依赖

Go 主进程和原生 V8 wrapper 是两个不同的构建产物。Linux 容器会构建并复制 `libali_slider_v8_runtime.so`；Windows 便携包也包含 `ali_slider_v8_runtime.dll`，并非仅靠独立 EXE 运行。原生库及其系统依赖不能因为 Go 构建设置了 `CGO_ENABLED=0` 就省略。

源码的 Go 版本与依赖以 [go.mod](./ali-slider-go/go.mod) 为准；原生运行时和构建方式见 [模块 README](./ali-slider-go/README.md) 与 [Dockerfile](./ali-slider-go/Dockerfile)。平台支持、打包内容和 Windows 运行要求见 [Windows AMD64 便携包](./ali-slider-go/docs/windows.md)。启动与运行步骤统一维护在模块 README。

## HTTP 边界

现有四个路径是 `/`、`/api/slider`、`/health` 和 `/openapi.json`。`/api/slider` 的 POST JSON 与已废弃 GET query 都会发起真实求解；旧 GET 不是无副作用的读取，不能用作健康检查。健康读取使用 `/health`。

GET 只读 query，POST 只读 JSON body，两个来源不合并。URL 可能进入浏览器历史、网关或代理访问日志，不要把 `AaduaneId` 或带凭据的代理放入 query。字段、别名、空值、重复键、请求大小、错误与跨源规则统一见 [HTTP API](./ali-slider-go/docs/api.md)。

## 文档入口

| 需要了解的内容 | 文档 |
| --- | --- |
| 当前实现与使用说明 | [模块 README](./ali-slider-go/README.md) |
| HTTP 合同 | [HTTP API](./ali-slider-go/docs/api.md) |
| 模块职责与资源生命周期 | [架构](./ali-slider-go/docs/architecture.md) |
| 配置与环境变量 | [配置](./ali-slider-go/docs/configuration.md) |
| 测试、构建与质量门禁 | [测试](./ali-slider-go/docs/testing.md)、[CI 配置](./.github/workflows/ali-slider-go-ci.yml) |
| 运行安全与故障排查 | [安全边界](./ali-slider-go/docs/security.md)、[排查](./ali-slider-go/docs/troubleshooting.md) |
| Windows 产物与系统要求 | [Windows AMD64 便携包](./ali-slider-go/docs/windows.md) |
| 性能测量与历史迁移 | [性能](./ali-slider-go/docs/performance.md)、[迁移](./ali-slider-go/docs/migration.md) |

## 当前说明与历史证据

带日期的实验报告和 `docs/evidence/` 中的记录仅说明当时版本、环境、输入与测量范围，不是当前 main 的验收结果。例如 [2026-08-08 无本地 admission 历史证据](./ali-slider-go/docs/evidence/validation-2026-08-08-no-local-admission.md) 保留原日期和原测量边界。

不能把历史会话池或旧架构的成绩直接用于当前版本，也不能把 Client 或纯计算测量写成 HTTP 端到端收益。当前版本是否通过测试、构建或性能目标，应依据对应 commit 的实际执行记录；未运行或受阻的项目不得标记为通过。

## 目录

```text
.
├── .github/workflows/ali-slider-go-ci.yml
├── ali-slider-go/
│   ├── cmd/server/
│   ├── docs/
│   ├── internal/
│   ├── native/v8runtime/
│   ├── packaging/windows/
│   ├── pkg/slider/
│   ├── Dockerfile
│   ├── Makefile
│   └── go.mod
└── README.md
```

## 旧版本资料

旧 Python 实现已从当前工作树删除，保留在 Git 历史提交 `0509bfd` 中。历史迁移资料见 [迁移文档](./ali-slider-go/docs/migration.md)，不应把旧实现的依赖说明套用到当前 Rust/V8 路径。
