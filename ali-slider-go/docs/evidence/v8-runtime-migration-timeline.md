# 内嵌 V8 运行时迁移时间线

| 日期 | 事件 | 证据/结论 |
|---|---|---|
| 2026-08-08 | 用户反馈 Go 服务在服务器运行后全失败，历史 Python/对照版仍成功。 | 问题更符合迁移语义/运行时差异，而非单纯 IP 差异。 |
| 2026-08-08 | 对比用户指定的 `d92c7d1760b416badb06cac04018106904f7cade`。 | 确认旧动态路径在同挑战保持 FeiLin 状态，并执行当轮 `StaticPath` 对应 PE；早期 Go 路径曾用静态 key/近似 payload 代替。 |
| 2026-08-08 | 先恢复动态 Device/PE 语义并保留 Node oracle。 | 证明“动态而不是每次随机”：公开源码/结构画像可 5 分钟复用，挑战级状态必须逐轮生成。 |
| 2026-08-09 | 用户选择 B：Go + 进程内 V8，不运行 Node。 | 实现转为 `purego` + Rust `cdylib` + V8 Isolate。 |
| 2026-08-09 | V8 142 在 ARM64 可运行，Linux AMD64 共享库链接失败。 | 链接器报 `R_X86_64_TPOFF32`；定位为上游 shared-library TLS 问题，不是 PE 算法错误。 |
| 2026-08-09 | 升级到 `v8=149.4.0`、`deno_core_icudata=0.77.0`。 | 使用包含上游 TLS 修复的版本，嵌入 ICU 77 common data，同时修复 Promise/microtask 递归和 `performance` Proxy 兼容。 |
| 2026-08-09 | Linux AMD64/ARM64 完成 Rust native 与 Go→V8 ABI 测试。 | 两架构 Rust 6 项测试全通过；`internal/v8runtime` 和 `internal/pe` 使用真实 `.so` 通过。 |
| 2026-08-09 | Windows AMD64 DLL 交叉构建完成。 | PE32+ x86-64 DLL；预期 `ali_slider_v8_*` 导出存在；静态 MSVC CRT，只导入 Windows 系统 DLL。本机无 Windows/Wine，原生执行交给 CI。 |
| 2026-08-09 | Node 24.14.1 与 V8 149.4.0 浏览器上下文合同差异测试通过。 | 生产 bundle 所依赖的对象/属性语义对齐；Node 只用于该回归 oracle。 |
| 2026-08-09 | Linux AMD64 最终 Debian/glibc 包执行生产 V8 Device/PE 组件探针。 | `tokenLength=1616`、`dataLength=1164`、142 字段、4 请求，测试 `2.78s`；不创建 Captcha Init/Verify。 |
| 2026-08-09 | 加入启动快速失败。 | `cmd/server` 在 ready/预热前调用 `Client.CheckRuntime`，缺少/错架构 DLL/`.so` 不再表现为所有 Solve 运行期失败。 |
| 2026-08-09 | 最终包双架构复核发现并修复 ARM64 混包/libc 问题。 | 移除 Dockerfile 的 amd64 默认覆盖，Go 构建层统一为 Debian bookworm；AMD64/ARM64 launcher 与 `.so` 均为同架构 glibc 产物，两个非 root 镜像 `/health` 通过。 |
