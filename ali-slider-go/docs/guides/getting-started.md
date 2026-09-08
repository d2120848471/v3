# 启动服务

运行需要 Go 主程序和同平台的 Rust/V8 wrapper。源码构建基线为 Go `1.26.6`、Rust `1.88.0`，见 [go.mod](../../go.mod)、[Cargo.toml](../../native/v8runtime/Cargo.toml) 与 [Dockerfile](../../build/docker/Dockerfile)。下面的构建和启动命令以 `ali-slider-go/` 为工作目录；依赖获取可能联网，启动本身不请求验证码上游。

## 源码运行

在仓库根目录进入 module，然后构建 wrapper：

```bash
cd ali-slider-go
make native-build
```

Linux：

```bash
CGO_ENABLED=0 \
  ALI_SLIDER_V8_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.so" \
  go run ./cmd/server
```

macOS：

```bash
CGO_ENABLED=0 \
  ALI_SLIDER_V8_LIBRARY="$PWD/native/v8runtime/target/release/libali_slider_v8_runtime.dylib" \
  go run ./cmd/server
```

wrapper 必须匹配运行系统和 CPU 架构。Linux 需要 glibc；macOS 使用 `.dylib`，Windows 使用 `ali_slider_v8_runtime.dll`。`go run` 的可执行文件在临时目录，因此上面显式指定 wrapper 路径。只执行 `go build` 不会生成 Rust 动态库。

启动先绑定端口，再检查本地动态库、C ABI 与 V8/ICU，并清理失败样本。看到 `event=listen status=ready` 后检查：

```bash
curl --fail --silent --show-error http://127.0.0.1:8000/health
```

返回 `{"ok":true,"status":"ready"}` 只表示本地服务就绪。浏览器打开 `http://127.0.0.1:8000/` 可使用内嵌测试页；页面只自动请求 health，手工提交后才执行真实 Solve。

## Windows 便携包

在 `ali-slider-go-ci` 成功且已上传产物的运行中下载 `ali-slider-go-windows-amd64.zip`，完整解压后双击 `start.bat`。包内包含 EXE、同平台 V8 DLL、第三方许可及校验信息；运行机无需安装 Go、Rust 或 Node。不要单独复制 EXE。

系统要求、下载步骤、参数转发及完整性校验见 [Windows 指南](windows.md)。仓库中的打包源文件在 [build/packaging/windows](../../build/packaging/windows)，发布 ZIP 中的 `start.bat` 仍位于根目录。

## Docker

Dockerfile 位于 `build/docker/Dockerfile`，**context 仍是 module 根目录 `.`**：

```bash
docker build --platform linux/amd64 \
  -f build/docker/Dockerfile -t ali-slider-go:local .

docker run --rm \
  -e ALI_SLIDER_HOST=0.0.0.0 \
  -p 127.0.0.1:8000:8000 \
  ali-slider-go:local
```

容器内监听 `0.0.0.0`，端口只发布到宿主回环。镜像包含 Go 主程序、同架构 V8 wrapper 和 notices，使用 Debian/glibc 与非 root 用户。Linux 双架构文件导出可运行 `make build-linux-amd64` 或 `make build-linux-arm64`；原生测试和镜像 smoke 见 [开发指南](development.md#平台构建与便携包)。

## 提交一次 Solve

在自己的授权场景中替换参数后执行；此命令会访问真实上游：

```bash
curl --fail-with-body \
  --header 'Content-Type: application/json' \
  --data '{"SceneId":"1ug4aptr","prefix":"fsgtmi"}' \
  http://127.0.0.1:8000/api/slider
```

每轮最多一次 Verify。完成态为 HTTP `200`，仍需检查 `ok`、`VerifyCode` 和 `VerifyResult`；网络结果未知时不要自动重试同一挑战。响应里的令牌和挑战标识只交给需要它们的业务调用方，不写普通日志。

服务默认没有应用内鉴权。代理和跨源边界见 [安全设计](../design/security.md)，参数与错误见 [HTTP API](../reference/http-api.md)，启动覆盖见 [配置](../reference/configuration.md)。在源码终端按 `Ctrl+C` 会停止接收工作并等待活动请求关闭。
