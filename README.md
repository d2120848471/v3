# Ali Slider Go

阿里 V3 滑块协议的纯 Go 实现。服务端生产求解、自动化测试、Linux 构建和容器均不依赖 Python、Node.js、浏览器、OpenCV、GoCV、CGo、动态库或子进程；内嵌 API 测试页只需系统现有的现代浏览器。

> 仅用于自有系统或获得明确授权的研究、兼容性验证与测试环境。服务默认只监听 `127.0.0.1:8000`，不应把未加鉴权的端口直接暴露到公网。

## 快速验证

```bash
cd ali-slider-go
CGO_ENABLED=0 GOPROXY=off go test -count=1 ./...
go run ./cmd/server
```

## HTTP 调用

服务只开放四个路径：`/`、`/api/slider`、`/health` 和 `/openapi.json`。新接入应使用 `POST /api/slider` + JSON；为兼容 Git 历史提交 `0509bfd` 中的旧 Python 客户端，同一 Solve 路径也保留已废弃的 `GET /api/slider?...` query 调用。两种方式都会发起真实求解。

```bash
curl --fail-with-body \
  --header 'Content-Type: application/json' \
  --data '{"SceneId":"1ug4aptr","prefix":"fsgtmi"}' \
  http://127.0.0.1:8000/api/slider
```

只识别 `SceneId/sceneId`、`prefix/Prefix`、`AaduaneId/aaduaneId`、`proxy/Proxy` 这 8 个精确名称。GET 只读 query，POST 只读 JSON body，两个来源不合并，也不支持 form body。旧 GET 有真实副作用且 URL 可能进入历史或访问日志；不要把 `AaduaneId` 或带凭据的代理放入 query。完整字段、空值、重复键、64 KiB 和跨源合同见 [HTTP API](./ali-slider-go/docs/api.md)。

## Windows 免环境压缩包

GitHub Actions 会在全部质量门禁通过后生成 `ali-slider-go-windows-amd64.zip`。包内是独立 EXE、双击启动脚本、中文说明、构建信息和 SHA-256；适用于 Windows 10 / Windows Server 2016 或更高版本的 AMD64/x64 机器，接收者无需安装 Go、Python、Node.js 或 VC++ Runtime。

下载：GitHub 仓库 **Actions** → `ali-slider-go-ci` → `main` 最新成功运行 → `ali-slider-go-windows-amd64.zip`。完整解压后双击 `start.bat`，等待 `event=listen status=ready`，再用现代浏览器打开 `http://127.0.0.1:8000/`。程序不是桌面 GUI，但 EXE 内置了无额外 Web 运行时或 CDN 依赖的 API 测试页，可手工填写参数并查看状态、耗时、trace 和脱敏结果。

完整说明见 [Windows AMD64 便携包](./ali-slider-go/docs/windows.md)。

项目入口：

- [完整 README](./ali-slider-go/README.md)
- [HTTP API](./ali-slider-go/docs/api.md)
- [架构](./ali-slider-go/docs/architecture.md)
- [配置](./ali-slider-go/docs/configuration.md)
- [测试与质量门禁](./ali-slider-go/docs/testing.md)
- [安全边界](./ali-slider-go/docs/security.md)
- [Windows AMD64 便携包](./ali-slider-go/docs/windows.md)
- [2026-08-08 无本地 admission 历史证据](./ali-slider-go/docs/evidence/validation-2026-08-08-no-local-admission.md)

## 目录

```text
.
├── .github/workflows/ali-slider-go-ci.yml
├── ali-slider-go/
│   ├── cmd/server/
│   ├── docs/
│   ├── internal/
│   ├── packaging/windows/
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
