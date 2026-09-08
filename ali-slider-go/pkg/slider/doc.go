// Package slider 提供可嵌入其他 Go 服务的单轮滑块验证接口。
//
// 通过 NewClient 创建可并发复用的 Client，并在退出前调用 Close。
// NewClient 只准备本地配置与共享资源，每次 Solve 才打开独立的设备会话。
// CheckRuntime 可在接收请求前校验本地 V8 运行库。
//
// ClientOptions 的零值使用默认配置。需要显式设置置信度 0 或采集耗时
// 0..0 时，先调用 DefaultClientOptions 再覆盖字段。
//
// Solve 的返回错误可通过 errors.As 提取为 *Error；上游正常返回的业务拒绝
// 则体现在 Result.OK 和 VerifyCode 中。宿主可依赖 Solver 接口注入离线实现。
package slider
