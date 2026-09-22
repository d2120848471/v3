# 文档索引

首次使用从 [启动指南](guides/getting-started.md) 或 [Go SDK 示例](../README.md#go-sdk-接入) 开始。当前文档按使用、接口和设计组织；归档保留原始快照的命令、指标与限制，不作为当前实现的验收结论。

## 使用与开发指南

| 文档 | 内容 |
|---|---|
| [启动服务](guides/getting-started.md) | 源码、Windows 和 Docker 的完整运行前提 |
| [开发与验证](guides/development.md) | 离线检查、架构门禁、原生 V8、覆盖率与平台 CI |
| [Windows 便携包](guides/windows.md) | 下载、完整解压、启动、平台要求和校验 |
| [故障排查](guides/troubleshooting.md) | 启动、HTTP、运行时、网络和资源问题 |
| [迁移指南](guides/migration.md) | 当前目录迁移、兼容边界及旧 Python 对照位置 |

## 接口参考

| 文档 | 内容 |
|---|---|
| [HTTP API](reference/http-api.md) | 验证码与 Baxia bx-ua 的路由、代理、curl 示例、结果、错误及 OpenAPI |
| [配置](reference/configuration.md) | flags、环境变量、默认值与资源预算 |
| [Go SDK](reference/go-sdk.md) | 公共源码导航、错误和生命周期边界 |

## 设计

| 文档 | 内容 |
|---|---|
| [架构与资源所有权](design/architecture.md) | 消费方 ports、生产组装、类型分流和共享 service |
| [性能与测量口径](design/performance.md) | 离线优化记录、计时边界与待测范围 |
| [安全边界](design/security.md) | 访问控制、代理、动态脚本、脱敏和失败样本 |
| [变更记录](../CHANGELOG.md) | 用户可见变化与开发里程碑 |
| [原生运行时许可](../native/v8runtime/THIRD-PARTY-NOTICES.txt) | wrapper 所需第三方许可 |

## 历史证据

`archive/evidence` 保存脱敏验证证据与当次范围；`archive/reports` 保存带日期的调查报告。正文中的旧路径、命令、预热池和 Node oracle 描述按历史原样保留。复现当前版本应使用上方指南中的命令；历史授权总量也不能视为新批次授权。

| 记录 | 当次范围 |
|---|---|
| [2026-08-07 脱敏验证](archive/evidence/validation-2026-08-07.md) | 旧实现离线检查与授权候选批次 |
| [2026-08-08 无本地 admission 快照](archive/evidence/validation-2026-08-08-no-local-admission.md) | 当次入口合同与动态 PE 修复 |
| [2026-08-09 性能证据](archive/evidence/validation-2026-08-09-performance.md) | PE 差分、历史预热池候选和测量边界 |
| [V8 迁移范围](archive/evidence/v8-runtime-migration-scope.md) | 当次范围、验收目标和约束 |
| [V8 迁移时间线](archive/evidence/v8-runtime-migration-timeline.md) | 当次调查、实现与验证记录 |
| [2026-08-09 V8 迁移报告](archive/reports/2026-08-09_reverse-ali-slider-v8-runtime-report.md) | Node 到同进程 V8 的实现与平台证据 |
| [2026-08-11 TRACELESS 适配](archive/reports/2026-08-11_js-web-pixcake-traceless-report.md) | PixCake 场景组件合同与单次验证 |
| [2026-08-11 SLIDING 适配](archive/reports/2026-08-11_js-web-dji-sliding-report.md) | DJI 场景组件合同与单次验证 |
| [2026-08-11 运行时性能优化](archive/reports/2026-08-11_js-web-captcha-runtime-performance-report.md) | 逻辑轨迹时钟、组件缓存与小样本测量 |
