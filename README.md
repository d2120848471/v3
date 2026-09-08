# Ali Slider Go

阿里 V3 验证码协议的 Go 实现，提供可复用的 Go SDK 和本机 HTTP 服务，按上游类型处理 `PUZZLE`、`TRACELESS` 与 `SLIDING`。主项目位于 [ali-slider-go](ali-slider-go/README.md)。仅用于自有系统或已获授权的研究、兼容性验证与测试。

生产运行需要 **Go 主程序和同平台 V8 动态库**。Go 通过 `purego` 调用 Rust wrapper，在同进程执行动态 Device SDK 与 PE；Go 构建使用 `CGO_ENABLED=0`。Windows 便携包和 Docker 镜像包含 wrapper，服务端无需 Python、Node.js 或浏览器。

- [启动服务](ali-slider-go/docs/guides/getting-started.md)：源码、Windows 与 Docker。
- [Go SDK 接入](ali-slider-go/README.md#go-sdk-接入)：公共 `pkg/slider` 包及最小调用。
- [HTTP API](ali-slider-go/docs/reference/http-api.md)：字段、结果、错误与旧 GET 兼容。
- [开发与架构](ali-slider-go/docs/README.md)：文档导航、质量门禁和职责边界。

所有 Go 和构建命令从 `ali-slider-go/` 执行。构建基线是 Go `1.26.6`、Rust `1.88.0`；只编译 Go 可执行文件还不足以运行服务。HTTP 默认监听 `127.0.0.1:8000`，当前没有应用内鉴权。

```text
ali-slider-go/       保持原 module 路径的 Go 项目
  cmd/              可执行入口
  pkg/slider/       保持兼容的公共 SDK
  internal/         应用、领域、适配与生产组装
  native/v8runtime/ 独立 Rust/V8 wrapper 项目
  build/            Docker 与 Windows 打包源文件
  docs/             指南、参考、设计和历史归档
.github/workflows/  离线质量、平台构建和发布门禁
```

内部目录会随实现调整；HTTP 合同与公共 SDK 路径保持兼容。目录迁移和旧 Python 实现位置见 [迁移指南](ali-slider-go/docs/guides/migration.md)。
