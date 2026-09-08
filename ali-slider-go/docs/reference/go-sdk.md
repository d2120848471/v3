# Go SDK 参考

公开包为 `github.com/d2120848471/v3/ali-slider-go/pkg/slider`，module 路径与既有导出签名保持兼容。最小可运行接入与生命周期示例统一放在 [项目 README](../../README.md#go-sdk-接入)；本页用于定位合同与理解内部映射。

## 公共合同的来源

| 来源 | 内容 |
|---|---|
| [doc.go](../../pkg/slider/doc.go) | 包用途、敏感字段与并发约定 |
| [client.go](../../pkg/slider/client.go) | `Client` 构造、求解、运行时检查、清理及关闭方法 |
| [types.go](../../pkg/slider/types.go) | `Request`、`Result`、`Solver` 与脱敏格式化 |
| [options.go](../../pkg/slider/options.go) | `ClientOptions` 与 `DefaultClientOptions` |
| [errors.go](../../pkg/slider/errors.go) | `ErrorKind`、`Error` 和错误映射 |
| [example_test.go](../../pkg/slider/example_test.go) | 可编译的使用示例 |

从 module 根目录查看当前签名：

```bash
go doc ./pkg/slider
go doc ./pkg/slider.Client
go doc ./pkg/slider.ClientOptions
```

本页不复制所有字段表；HTTP 字段名及历史 JSON 大小写以 [HTTP API](http-api.md) 为准，服务 flags 与环境变量以 [配置](configuration.md) 为准。

## 结果与失败

`Solve` 返回 `Result` 且 `error == nil` 表示协议已取得完成态结果，包括业务拒绝。调用方读取 `OK`、`VerifyCode` 和 `VerifyResult` 判断业务状态，按需要显式使用敏感 `SecurityToken` 等字段。错误类别保留 `InvalidRequest`、`ProtocolError`、`NetworkError`、`VisionError`、`InternalError`；可用 `errors.As` 取得 `*slider.Error` 的稳定 `Kind` 和 `Stage`。

同一轮最多尝试一次 Verify，网络结果未知时不能据此重试该轮。普通日志使用稳定错误类别、阶段和聚合耗时；不要展开底层 cause 或完整请求、结果中的敏感字段。

## 生命周期与实现边界

一个 `Client` 可供多个 goroutine 复用。它持有应用 `service.Service`，由 `bootstrap.NewService` 组装共享 Transport、脚本/profile 缓存和原生运行时。`pkg/slider` 负责公开选项、请求、结果和错误的映射；HTTP 直接消费应用合同并使用同一个 service 实现。

每轮 Device/V8 与挑战状态独立创建并关闭。`Prime` 是兼容空操作；`Close` 幂等并等待活动调用返回，等待期间不接受新工作，也不主动取消已开始的 Solve。SDK 宿主负责停止入口、传播 context 取消和按需调度 `PurgeArtifacts`。具体关闭顺序与缓存边界见 [架构](../design/architecture.md#跨请求生命周期)。

`internal` 包不属于稳定 SDK，不能作为外部导入路径。内部重构由 [架构测试](../../tests/architecture/dependencies_test.go) 检查生产依赖方向，并由 [公开 API 黄金快照](../../tests/architecture/public_api_test.go) 检查公共方法、结构字段/JSON tag 和错误常量；它不能证明在线上游兼容或生产吞吐。
