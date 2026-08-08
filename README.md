# Ali Slider Go

阿里 V3 滑块协议的纯 Go 实现。生产运行、测试、Linux 构建和容器均不依赖 Python、Node.js、浏览器、OpenCV、GoCV、CGo、动态库或子进程。

> 仅用于自有系统或获得明确授权的研究、兼容性验证与测试环境。服务默认只监听 `127.0.0.1:8000`，不应把未加鉴权的端口直接暴露到公网。

## 快速验证

```bash
cd ali-slider-go
CGO_ENABLED=0 GOPROXY=off go test -count=1 ./...
go run ./cmd/server
```

项目入口：

- [完整 README](./ali-slider-go/README.md)
- [HTTP API](./ali-slider-go/docs/api.md)
- [架构](./ali-slider-go/docs/architecture.md)
- [配置](./ali-slider-go/docs/configuration.md)
- [测试与质量门禁](./ali-slider-go/docs/testing.md)
- [安全边界](./ali-slider-go/docs/security.md)
- [最新离线验证证据](./ali-slider-go/docs/evidence/validation-2026-08-08-no-local-admission.md)

## 目录

```text
.
├── .github/workflows/ali-slider-go-ci.yml
├── ali-slider-go/
│   ├── cmd/server/
│   ├── docs/
│   ├── internal/
│   ├── pkg/slider/
│   ├── Dockerfile
│   ├── Makefile
│   └── go.mod
└── README.md
```

## 恢复旧 Python 版本

旧 Python 实现已从当前工作树删除，但完整保留在 Git 历史提交 `0509bfd` 中。

查看删除前的完整版本：

```bash
git switch --detach 0509bfd
```

只恢复旧 Python 文件到当前工作树：

```bash
git restore --source=0509bfd -- \
  ali_slider_reverse packaging tests pyproject.toml \
  '2026-08-07_迁移-阿里滑块纯Python-report.md' \
  .github/workflows/build-windows.yml
```

Go module 内名称包含 `python_oracle` 或 `python-edge-decoy` 的 JSON/PNG 是脱敏静态回归 fixture，不包含可执行 Python，也不引入 Python 运行时依赖。
