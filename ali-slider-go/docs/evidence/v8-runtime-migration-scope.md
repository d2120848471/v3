# 内嵌 V8 运行时迁移范围

## 授权与目标

- 授权来源：用户明确要求对比可用旧实现，修复 Go 迁移后的失败，并选择“B：Go 进程内嵌 V8，不依赖 Node 进程”。
- 工作区：`ali-slider-go` 及根目录对应 CI 工作流。
- 目标平台：Linux AMD64、Linux ARM64、Windows AMD64。
- 目标产物：Go launcher + 同架构 V8 wrapper；生产运行时不启动 Node。

## 范围内

1. 只读对比用户指定的动态语义对照提交 `d92c7d1760b416badb06cac04018106904f7cade`。
2. 识别纯 Go 迁移丢失的 Device 状态与当前 PE 动态执行语义。
3. 实现 Rust V8 C ABI wrapper、Go `purego` 加载层、浏览器环境 bridge、Device/PE Isolate 生命周期与 Go host 回调。
4. 保留动态边界：同一 Device Isolate 跨 Open/Complete；每轮 PE 使用当前 `StaticPath`、`CertifyId`、DeviceConfig、轨迹和时钟。
5. 只缓存公开 SDK/PE 源码与结构画像 5 分钟；不缓存 DeviceToken、`CertifyId`、轨迹或 `data`。
6. 离线单测、跨语言上下文差异、Linux 双架构 native/ABI、Windows DLL 构建/依赖检查、Docker/Windows 打包和启动失败快速暴露。
7. 显式在线的公开 Device/PE 组件探针：仅 Open/Resolve/Build/Complete，不创建 Captcha Init/Verify。

## 范围外

- 未授权的批量真实挑战、成功率重测、P95/P99 和长时间容量压测。
- 对第三方系统的漏洞利用、鉴权绕过、扩大流量或重试同一 `CertifyId`。
- 改变 HTTP 产品面、业务提交链、鉴权策略或现有视觉算法。
- 将 V8 Isolate 声称为操作系统级沙箱，或将组件探针外推为完整验证码成功率。

## 数据与证据处理

- 只记录命令、版本、架构、返回长度、指纹字段数和请求数。
- 不写入 token、`CertifyId`、代理凭据、上游原始正文或真实挑战图片。
- 历史证据文档保持原样；本次另建报告，避免改写旧快照的时间语义。
