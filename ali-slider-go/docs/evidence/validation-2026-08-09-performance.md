# 2026-08-09 动态 PE 与完整 Solve 性能优化证据

## 执行摘要

本轮将 Linux AMD64、并发 10、零应用重试的真实 50 次 Solve，从优化前 `46/50`、P50 `4722ms`、P95 `7136ms` 改善到最终安全候选 `47/50`、mean `2562ms`、P50 `2157ms`、P95 `4910ms`。3 次非成功结果全部是上游业务 `F015`；`ProtocolError`、`NetworkError`、`VisionError` 均为 0。后 25 次 mean 为 `2232ms`，不能代替全批结果。

纯 Go PE Builder 不是盲目启用：每个精确 `StaticPath` 先使用当前公开 SDK + PE 在禁网 V8 context 生成固定假输入结果，再对 payload、轨迹、互动事件和时钟做完整差分。只有差分为零的精确分片才在 TTL 内走纯 Go；任何不一致或采样失败都保留 V8 fallback。最终 50 次遇到 32 个精确分片，32/32 通过差分，0 个采样失败。

额外的默认直连公开 Device 组件 A/B 证明：5 个同时存活的动态 Device VM 已出现指纹字段不完整，10 个同时存活时稳定触发 `Verify device request sequence`；预热池以 4 个 live slot 通过 Complete→Recycle 服务 10 个 job 时全部通过。因此最终池库存默认和硬上限均为 4，不能用扩大同池 live VM 数交换延迟。代理组不在这项结论的覆盖范围内。

结论：正确性门槛“不低于 46/50”通过，P50 相对基线下降约 54%；但最终 50 次 mean `2562ms` 仍未达到约 1 秒，不得声称该目标已完成。曾出现的 `48/50`、mean `1890ms` 是发现 live Device 并发合同之前的中间候选，不是当前安全默认路径。

## 范围与授权

- 对象：工作区 `ali-slider-go`，候选版本基于 `66ee8479c602a9941451209ff137582b268af481`；交付版本为包含本报告的 commit。
- 目标：在不引入 Node、不复用挑战态、不重试 Verify 的前提下，将 50 次完整 Solve 平均耗时优化到约 1 秒。
- 在线边界：用户明确要求真实 50 次/多轮验收。每个 job 创建新挑战，每个 `CertifyId` 最多一次 Verify，应用层重试数为 0。
- 数据边界：报告只保留聚合延迟、成功/失败类别和不含路径/key 的缓存计数；不保留 token、`CertifyId`、RPC key、代理凭据或上游原文。
- 未覆盖：HTTP 端到端延迟、长时间 RSS/GC/goroutine/原生线程峰值、Windows 原生运行、代理组。

## 安全与正确性不变量

1. 每个 Solve 的 Init 和 Verify 绑定同一个新 `CertifyId`。
2. Verify 尝试位在构造请求前不可逆置位；DNS、TLS、超时或未知结果都不回滚。
3. Device recycle 只保留外层 V8 Isolate/桥；每轮重建浏览器 context、session ID、DeviceToken 和 FeiLin 挑战状态。
4. PE 画像以完整 `StaticPath` 隔离；不共享 `CertifyId`、DeviceConfig、轨迹或 `data`。
5. 纯 Go 快路必须由当前 V8 oracle 完整差分开启；不一致/失败不降级为随机 key，而是回退 V8。
6. 低置信、图像、协议、Device Complete 或 PE 合同错误仍在 Verify 前停止。
7. 每个 live Device slot 使用独立只读画像；同一 slot 内串行 recycle，默认直连池最多同时保留 4 个 Device VM。RPC headers、图片、PE 与 Device VM 必须使用租到会话的实际画像。

## 实现路径

```mermaid
sequenceDiagram
    participant C as "slider.Client"
    participant D as "Device pool / V8"
    participant I as "Captcha Init"
    participant K as "KeyResolver"
    participant V as "V8 PE oracle"
    participant G as "pure Go Builder"
    participant X as "Captcha Verify"

    C->>D: Lease one of 4 retained pool slots
    D-->>C: session + DeviceToken
    C->>I: Init once
    I-->>C: new CertifyId + exact StaticPath
    par PE source/profile
        C->>K: Prepare exact StaticPath
    and assets
        C->>C: download two images in parallel
    end
    alt profile missing or hard-expired
        K->>V: fixed dummy input with current SDK + exact PE
        V-->>K: payload + events + timing
        K->>G: run same dummy input
        G-->>K: full diff
    end
    alt full diff compatible
        C->>G: build current-round data
    else incompatible or sample failed
        C->>V: build current-round data
    end
    C->>D: Complete same round
    D-->>C: VerifyToken; recycle outer Isolate with fresh context
    C->>X: Verify exactly once
    X-->>C: business result
```

## 证据清单

| ID | Evidence | 观测 |
|---|---|---|
| E-001 | 优化前真实 50 次 | `46/50`，P50 `4722ms`，P95 `7136ms`，max `8578ms` |
| E-002 | `internal/pe/v8_runtime_online_test.go` 强制 V8/纯 Go 差分 | 解包 JSON、arg、x/slidePos、全部 TrackList、互动事件、时间和 post delay 为 0 差异；V8 约 `530ms`，纯 Go 约 `0.364ms` |
| E-003 | 快路后真实 50 次 | `49/50`，mean `2007ms`，P50 `1771ms`，P95 `3217ms`；`buildVerifyData` mean `2ms` |
| E-004 | 中间 `reserve=0` 真实 50 次 | `48/50`，mean `1890ms`，P50 `1909ms`，P95 `2996ms`；后 25 mean `1376ms`；发生在 live Device 边界确认之前，不是最终候选 |
| E-005 | `ProfileCacheStats` 脱敏计数 | 32 个精确分片，32 个已采样，32 个兼容，0 个失败 |
| E-006 | reserve A/B | reserve=1 mean `2112ms`；reserve=4 mean `2305ms`，都慢于默认 0 |
| E-007 | 真实 prewarm=16 失败聚类 | `33/50`；16 次全部在 `completeDevice` 为 `ProtocolError`，恰好覆盖 16 个预热会话；另 1 次业务拒绝 |
| E-007b | prewarm=32 资源实验 | Docker可见内存约8.2GB；Prime/验收过程中进程被SIGKILL，未产生可用acceptance汇总，因此不能作为容量配置 |
| E-008 | 公开 Device live-concurrency A/B | 2/4 live 全部通过；5 live 的 5 路均只有 138 字段；6 live 在 Open 报 sequence；10 live 失败；4 live slot 接力 10 job 全部通过（9.27s） |
| E-009 | 最终安全候选第一轮真实 50 次 | `48/50`，业务 `F015=2`，分类错误 0；mean `2657ms`、P50 `2760ms`、P95 `4356ms` |
| E-010 | 满池等待调度真实 50 次 | `47/50`，业务 `F015=3`，分类错误 0；mean `2562ms`、P50 `2157ms`、P95 `4910ms`；后 25 mean `2232ms` |
| E-011 | 单元/竞态/平台构建门禁 | Go普通全量、全量race、vet、online-tag编译、diff-check、Linux AMD64/ARM64与Windows AMD64无CGO构建全部通过；Rust 7/7；Linux AMD64真实wrapper全量Go通过 |

## Finding → Evidence → Path

| Finding | 结论 | Evidence | Path |
|---|---|---|---|
| F-001 | 每轮重建 Device/PE 外层 V8 宿主是可消除开销，但挑战状态不能复用 | E-001、Device 两轮公开链探针 | Complete → release → outer-Isolate recycle → fresh context/session/token |
| F-002 | 当前 PE 算法可纯 Go 等价生成，但必须以精确分片 V8 oracle 作动态门禁 | E-002、E-003 | StaticPath → V8 dummy output → full diff → pure Go or V8 fallback |
| F-003 | 纯 Go Builder 已不是主瓶颈；最终候选的主瓶颈是默认直连池4槽安全边界产生的session等待 | E-005、E-008、E-009、E-010 | 10 concurrent Solve → 4 retained pool slots → Complete/recycle → waiting Lease |
| F-004 | 额外 Device reserve 在该环境与冷 PE/Complete 争用，不应默认开启 | E-006 | leased session → speculative Device open → CPU/network contention → slower total |
| F-005 | 默认直连测试中同时存活超过4个动态Device VM会破坏公开SDK/上游合同并放大内存，预热池必须硬限制 | E-007、E-007b、E-008 | aligned prewarm → excess live sessions → incomplete fields/sequence error/OOM |
| F-006 | 目标部分达成：正确性和 P50 加速通过，mean 约 1s 未通过 | E-001、E-010 | baseline → final safe candidate → explicit failed latency gate |

## 缓存与失效合同

| 对象 | 边界 | 失效 |
|---|---|---|
| SDK 源码 | 路由隔离、单飞、5 分钟软 TTL | 重下后字节不同则立即清空全部 PE profile |
| 精确 PE 源码/profile | 完整 `StaticPath` 隔离、同路径单飞、异路径并发 | SDK 改变立即失效；不论 SDK 是否变化，30 分钟强制重下 PE 与 V8 差分 |
| V8 精确源码 code cache | 进程内、最多 64 项/64 MiB，不跨版本持久化 | LRU 淘汰或进程结束 |
| Device 外层 Isolate | 每个 slot 独立画像；默认直连池库存默认/硬上限 4，reserve 0 | 每轮必须重建 context/session/token；满池等待 recycle；失败即关闭并冷补货 |
| 挑战级数据 | 不缓存 | Solve 结束即释放 |

## 时间线

| 阶段 | 结果 |
|---|---|
| 基线 | 50 次 `46/50`，P50 `4722ms`，P95 `7136ms` |
| Device recycle + PE runtime pool | mean `3040ms`，瓶颈收敛到 profile/V8 Build |
| SDK/分片单飞 + 预取 | 不同分片可并发，与双图下载重叠 |
| native V8 code cache | 强制 V8 热 Build 约从 `629ms` 降到 `526ms` |
| 精确分片快路 | 真实 Build mean 降到 `1–2ms`，50 次 `49/50` |
| reserve A/B | reserve 1/4 均回归，默认回到 0 |
| Device池库存边界 | prewarm16产生16个Complete协议错误；默认直连公开A/B确认池最多保留4个slot |
| 最终安全候选 | `47/50`，mean `2562ms`，P50 `2157ms`，P95 `4910ms`，分类错误0，未达1s |
| 长运行刷新 | SDK 5 分钟字节复核 + PE 30 分钟硬差分 |

## 可复现验证

离线门禁：

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
git diff --check
```

在线验收只可在明确授权环境显式启用，且不得对同一 `CertifyId` 重试：

```bash
ALI_SLIDER_ONLINE=1 \
ALI_SLIDER_ONLINE_COUNT=50 \
ALI_SLIDER_ONLINE_CONCURRENCY=10 \
ALI_SLIDER_ONLINE_PREWARM=4 \
ALI_SLIDER_ONLINE_DEVICE_RESERVE=0 \
ALI_SLIDER_ONLINE_ARTIFACT_DIR=/controlled/artifacts \
ALI_SLIDER_V8_LIBRARY=/absolute/path/libali_slider_v8_runtime.so \
go test -count=1 -tags online -run '^TestOnlineAcceptance$' ./pkg/slider
```

## 最终验证状态

| 门禁 | 状态 |
|---|---|
| 精确分片 V8/纯 Go 差分 | 通过；当前公开分片 0 差异 |
| 公开 Device 组件 10 job / 4 live slot | 通过；10/10 完整 142 字段、4请求合同 |
| 真实 50 次成功率不低于 46/50 | 通过；最终候选 47/50，3次均为业务F015 |
| 真实冷态 50 次 mean 约 1s | **未通过**；最终候选 mean 2562ms |
| 全量 Go test/race/vet/diff-check | 通过；全部退出码0 |
| online build-tag编译 | 通过；未启用在线开关、未产生新挑战 |
| Linux AMD64/ARM64、Windows AMD64无CGO构建 | 通过；全部退出码0 |
| Rust native fmt / test | 通过；固定Rust 1.88容器，7/7测试通过 |
| Linux AMD64 真实 V8 wrapper | 通过；x86-64，SHA-256 `6979fb7ac88eca4d32fa5de04a9b81ac18b748b84c9d250ea85f86aecc453974`，全量Go测试通过 |

## 限制与后续

- 在 50 次内遇到 32 个分片仍有冷态成本，但最终 mean 中 `resolvePEKey=204ms`，已经不是最大阶段；不得为了速度将未验证分片共用一个 key/profile。
- 最终 50 次 `deviceSession=1644ms`、`completeDevice=289ms`；并发10下的主要代价来自默认直连池4槽安全边界。除非公开 Device 合同变化或按独立路由重新取得证据，否则不得提高该池上限。
- 上游 Device、Init、Complete、Verify 仍是网络阶段，单次延迟受 DNS/TLS/出口/上游影响；放大 timeout 不是性能优化。
- reserve 只保留为有界显式 A/B 开关，并要求 `prewarm + reserve <= 4`；当前默认和建议值为 0。
