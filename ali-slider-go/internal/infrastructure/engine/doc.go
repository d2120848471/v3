// Package engine 管理 Aliyun V3 公开脚本缓存、动态 PE 执行和 SDK 设备会话。
//
// 生产路径由 KeyResolver 管理公开 SDK/PE 源码与结构画像缓存，通过内嵌 V8
// 维持每轮 SDK/FeiLin 设备会话。当前 SDK 与精确 PE 分片通过 V8 对照校验后，
// Build 可使用 domain/pe.Builder 构造 Puzzle data；其余情况由动态 PE 运行时处理。
// 挑战级 DeviceToken、CertifyId 和轨迹始终绑定当前会话，不进入脚本缓存。
//
// Node bridge 保留为可选 oracle 和回滚测试路径，不由生产构造器启用。
package engine
