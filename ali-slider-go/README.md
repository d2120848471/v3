# Ali Slider Go

`ali-slider-go` 提供可并发复用的 Go SDK 和 HTTP 服务。Go 负责单轮编排、网络、图像识别和轨迹；动态 Device SDK 与 PE 通过同进程 V8 执行。Init 返回的 `CaptchaType` 决定 `PUZZLE`、`TRACELESS` 或 `SLIDING` 分支，每轮最多发送一次 Verify。

生产运行需要 Go 主程序与同平台 V8 wrapper。Go 使用 `CGO_ENABLED=0` 和 `purego`，服务端无需 Python、Node.js 或浏览器。仅用于自有系统或已获授权的测试环境；服务没有应用内鉴权，默认监听 `127.0.0.1:8000`。

## 启动与 HTTP 接入

- [启动指南](docs/guides/getting-started.md)：Linux/macOS 源码、Windows 便携包和 Docker。
- [HTTP API](docs/reference/http-api.md)：推荐 `POST /api/slider`，保留已废弃的旧 GET query；完整字段、错误和 OpenAPI 合同。
- [配置参考](docs/reference/configuration.md)：命令行参数、环境变量、默认值与资源边界。

启动成功后打开 `http://127.0.0.1:8000/` 使用内嵌测试页，或访问 `/health` 检查进程就绪。启动和 health 不访问真实上游；手工提交 Solve 后才创建挑战。HTTP `200` 仍需读取 `ok`、`VerifyCode` 和 `VerifyResult` 判断业务结果。

## Go SDK 接入

公共包路径保持为 `github.com/d2120848471/v3/ali-slider-go/pkg/slider`。在宿主中复用一个 `Client`，停止接收工作后关闭。以下示例会访问真实上游，场景与 wrapper 路径应替换为自己的授权配置：

```go
package main

import (
	"context"
	"log"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	options := slider.DefaultClientOptions()
	options.V8RuntimeLibrary = "/opt/ali-slider/libali_slider_v8_runtime.so"
	client, err := slider.NewClient(options)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.CheckRuntime(); err != nil {
		return err
	}
	result, err := client.Solve(ctx, slider.Request{
		SceneID: "1ug4aptr",
		Prefix:  "fsgtmi",
	})
	if err != nil {
		return err
	}
	log.Printf("ok=%t verifyCode=%s", result.OK, result.VerifyCode)
	return nil
}
```

`NewClient` 不发外部请求，`CheckRuntime` 只检查本地 wrapper/ABI/V8/ICU。`err == nil` 表示取得完成态结果，业务可能未通过；技术失败可用 `errors.As` 读取 `*slider.Error` 的 `Kind` 与 `Stage`。从 `DefaultClientOptions()` 开始覆盖配置，可保留置信度 `0` 等有意义的零值。

每轮独占 Device/V8、token、挑战、轨迹和 Verify 状态，结束时关闭会话。`Close` 拒绝新工作并等待活动调用返回，不主动取消 Solve；取消由调用方 context 或总超时负责。`Prime` 保留为兼容空操作。HTTP 宿主在启动时和每小时清理失败样本；SDK 宿主负责定期调用 `PurgeArtifacts`。

方法与类型以 [公共包源码](pkg/slider) 和 `go doc ./pkg/slider` 为准；源码导航、错误合同和兼容边界见 [SDK 参考](docs/reference/go-sdk.md)。

## 开发与维护

```bash
make fmt-check
make vet
make test
```

`make test` 包括架构依赖检查；`make bench` 只运行离线视觉和 Mock HTTP 基准。完整命令、真实 V8 ABI、覆盖率和平台门禁见 [开发指南](docs/guides/development.md)。Dockerfile 位于 [build/docker](build/docker/Dockerfile)，构建 context 仍是本 module 根目录；Windows 包素材位于 [build/packaging/windows](build/packaging/windows)。

应用编排、共享生命周期和生产组装分别由 `internal/application/solve`、`internal/application/service` 与 `internal/bootstrap` 承担。HTTP 与 SDK 共同使用应用 service；公共 `pkg/slider` 保留 DTO、错误及兼容映射。目录和依赖方向见 [架构](docs/design/architecture.md)，迁移映射见 [迁移指南](docs/guides/migration.md)。内部路径不是稳定接入 API。

`MaxConcurrency` 只控制出站每 host 连接和 PE 空闲引擎保留量；HTTP 在途请求、Device 会话与活跃 PE 调用没有本地并发上限。完整 Client/HTTP 延迟和长时间资源稳定性需要针对实际部署测量，已有离线数字的范围见 [性能说明](docs/design/performance.md)。全部文档见 [索引](docs/README.md)，用户可见变化见 [CHANGELOG](CHANGELOG.md)。
