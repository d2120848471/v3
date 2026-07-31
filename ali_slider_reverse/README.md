# Ali V3 滑块纯协议复现：最终实现与正确思路

> 仅用于用户明确授权的本地 CTF、兼容性研究与协议验证环境。禁止用于批量解题、
> 并发挑战、同一挑战重试、规避第三方访问控制或任何未授权目标。

## 1. 最终结论

最终实现不启动 Chrome，也不依赖 Playwright、Selenium、Puppeteer、CDP 或人工
拖动。它把各层职责拆开：

```text
Python：HTTP/RPC 编排、资源下载、结构校验和单次 Verify 安全门
Node VM：执行当前公开 AliyunCaptcha.js、FeiLin 和本轮动态 pe.*.js
OpenCV：识别缺口并给出 xPos
```

在 2026-07-31 的用户授权环境中，当前版本已经观察到：

- 新挑战只发送一次 `VerifyCaptchaV3`；
- 返回 `VerifyCode=T001`、`VerifyResult=true` 和非空 `securityToken`；
- 只有 T001 安全门全部满足后才提交一次本地业务请求；
- 业务请求返回 HTTP `200`；
- 全程没有浏览器、刷新、同一 `CertifyId` 重试或失败后业务提交。

这些是特定公开 SDK、动态 PE、服务端契约和运行环境下的实测结果，不是对未来
版本或任意挑战永久成功的保证。

## 2. 最终运行链

```mermaid
flowchart TD
    A["同一个 Node FeiLin VM：Log1 → Log2，生成 Init DeviceToken"]
    B["Python：InitCaptchaV3，取得本轮 CertifyId、图片和 StaticPath"]
    C["四路并发下载：back.png、shadow.png、pe.*.js、main.css"]
    D["并行执行：UploadLog 与 OpenCV 缺口识别"]
    E["Node PE VM：回放轨迹，以虚拟逻辑时钟构造原生 data"]
    F["原 FeiLin VM：只回放 getter 前事件前缀，再执行一参 getToken"]
    G["Python：严格校验并发送一次 VerifyCaptchaV3"]
    H["T001 + true + 非空 securityToken"]
    I["可选：重算本地业务签名并提交一次"]
    J["停止：不重试、不提交业务"]

    A --> B --> C --> D --> E --> F --> G
    G -->|满足安全门| H --> I
    G -->|任一条件不满足| J
```

核心原则是：Python 负责可审计的控制流，动态且容易变化的前端行为由本轮公开
JavaScript 自己执行，最终结果再由 Python 独立解包和校验。

## 3. 真正决定成败的正确思路

### 3.1 DeviceToken 必须保持同一个 FeiLin VM

DeviceToken 不是静态指纹字符串，也不能在 Init 与 Verify 阶段各启动一个互不
相关的环境。正确链路是：

```text
公开 SDK / FeiLin
  → Log1
  → 解密 DeviceConfig
  → Log2
  → Init DeviceToken
  → 保持 worker 和 session
  → 回放本轮 PE getter 前已经发生的交互
  → 实际一参 getToken(...)
  → Log3
  → Log2
  → Verify DeviceToken
```

Python 会解析并校验 DeviceToken 的容器、session、AES/MD5 结构，但最终在线
token 由公开 FeiLin 原调用链生成。只替换 UA、伪造字段数量或重新启动第二个 VM
都会破坏跨阶段状态一致性。

### 3.2 最终 `data` 应由当前动态 PE 原生构造

不能把某个旧 `pe.*.js` 的混淆字段、checksum 或 payload 分片硬编码成长期协议。
每轮 Init 返回新的 `StaticPath`；实现会下载该挑战对应的动态 PE，并在隔离、
禁网的 Node `vm` 中：

1. 加载当前公开 SDK 与本轮 PE；
2. 提供 PE 实际读取的最小 DOM、事件和时钟接口；
3. 回放触摸轨迹；
4. 截获 `captchaVerifyCallback`；
5. 保留 PE 原生生成的完整 `data`；
6. 由 Python 解包并严格检查 schema、字段顺序、坐标、时间和 getter 时序。

这样既避免 Python 猜测动态密文，也避免 Node VM 直接发送网络请求。

### 3.3 `86 → 86 → 87` 是两个不同的时序合同

当前公开 PE 的关键观察是：

```text
输入 touch 事件                    86
getter 调用前已经形成的 native mm   86
最终 data.TrackList.mm             87
getter 后、序列化前的 RAF 尾项        1
```

因此：

- Verify `data` 必须保留序列化时存在的全部 87 条 native `mm`；
- FeiLin getter 只能看到 getter 调用前的 86 条连续前缀；
- 不能把 getter 后的 RAF 尾项提前回放到 FeiLin；
- `mm.timeStamp` 与 getter `observedAtMs` 必须来自同一个 PE VM 的
  `performance.now()` 时间域。

这是此前最容易把“最终 payload 状态”和“getter 当时状态”混为一谈的地方。

### 3.4 虚拟时钟压缩等待，但不删除时间语义

正确优化不是删掉 36 秒首触时龄或 2.233 秒轨迹，而是分离两个时间域：

```text
逻辑时间：TrackStartTime、firstTouchAge、每个 dt、timeStamp、VerifyTime
真实墙钟：JavaScript 执行、HTTP、OpenCV、子进程和事件循环 turn
```

PE 使用显式、单调、离散推进的逻辑时钟。每个事件之后仍跨一个真实
`setTimeout(0)` macrotask，让 dispatch 期间同步安排的 RAF/host callback 在
下一事件前完成，但不会按逻辑 `dt` 真实 sleep。

逻辑 epoch 会整体向过去对齐，避免快速回放后生成“未来的 VerifyTime”；Python
在发送网络请求前还会独立拒绝超过当前墙钟 2 秒的 `VerifyTime`。

默认首 touch 逻辑时龄为 650～850ms；如需复现实验中的长分布，可显式传入：

```bash
--first-touch-age-min 36000 --first-touch-age-max 36250
```

这只改变逻辑时间，不会真实等待约 36 秒。

### 3.5 `xPos` 是 shadow 画布左坐标，不是 alpha 边界

真实 `shadow.png` 常是窄画布，内部透明区的 `alpha_bbox.left` 可能不是 0。
协议需要的是整个 shadow 画布在 `back.png` 原始像素坐标系中的左坐标：

```text
xPos = 匹配到的 alpha 形状位置 - alpha_bbox.left
```

图像路径组合多类候选：

- 多阈值 edge-template；
- 高亮低饱和区域；
- 多阈值 contour；
- alpha bbox 的 canvas-left 换算；
- 完整 alpha 轮廓的双向 Chamfer 距离仲裁。

双向 Chamfer 同时检查“alpha 边界到背景边缘”和“背景局部边缘到 alpha 边界”，
可以拒绝只贴中缺口单边的伪峰。

当 alpha 形状触及图片顶边时，edge-template 可能偏向 2px 外的旧候选。最终修复
没有硬编码 `+2`，而是只在同一候选组内、由独立 non-edge 方法支持且 Chamfer
明显改善时选择已有候选。门槛不满足就保持原结果或在 Verify 前停止。

### 3.6 `xPos` 与 `slidePos` 必须分别校验

当前 PE 的运动关系为：

```text
xPos = s * (3*s + 65) / 845
s = (-65 + sqrt(65² + 12*845*xPos)) / 6
```

其中 `s` 是手柄位移 `slidePos`。逆根先限制到可拖动范围，再使用非负输入上的
JavaScript `Math.round` 语义，即 `floor(value + 0.5)`。最终 PE 会原生生成两者，
Python 和 Node 各自检查它们与图像预期值的偏差；超过允许范围时在 Verify 前停止。

### 3.7 Verify 必须是不可重试的单次消耗点

控制流只有一个 Verify 调用点：

```text
图片共识通过
AND PE schema/坐标/时间通过
AND FeiLin getter 与 session 通过
→ 每个 CertifyId 最多一次 VerifyCaptchaV3
```

图片弱、资源失败、schema 不符、时间异常或 getter 不一致时都应在网络 Verify
之前停止。收到 F015 或其他失败码后不能复用同一 `CertifyId` 试邻近坐标。

可选业务提交的硬门是：

```text
VerifyCode == "T001"
AND VerifyResult == true
AND securityToken 非空
```

任一条件不满足都不发送业务请求。

### 3.8 并发只用于相互独立的本轮工作

Init 之前没有本轮图片和 `StaticPath`，所以无法安全预取 challenge-bound 资源。
Init 之后可并行的只有：

- `back.png`、`shadow.png`、动态 PE、CSS 四项公开资源；
- 只尝试一次的 best-effort `UploadLog` 与本地 OpenCV 识别。

所有 worker 都会在 PE/Verify 前回收。实现不共享跨线程 `requests.Session`，也不让
UploadLog 在后台悬空。

单轮之内不并发多个挑战。`api.py` 允许多轮挑战同时进行，但每轮都持有自己的客户端、
FeiLin worker、临时目录和 `CertifyId`，轮与轮之间没有共享可变状态。

`GatherCost` 是唯一一处曾隐含“单轮独占 CPU”假设的地方：Node 桥在 Init 与 Verify
阶段各测一次采集耗时，空闲时两次都落到 0ms 并被统一归一化，并发抢占 CPU 时两次都
会超过下限且互不相等。这属于桥的测量方式，不代表 FeiLin session 不一致，因此统一
取先发生的 Init 耗时——真实页面里 Verify token 复用的也正是 Init 之前那次采集。

## 4. 最终保留文件

```text
ali_slider_reverse/
├── README.md                  # 本文：最终思路、运行方法和边界
├── __init__.py                # 对外 Python API
├── cli.py                     # solve / run 命令入口
├── api.py                     # HTTP 接口入口（SceneId / proxy / AaduaneId）
├── client.py                  # Init、资源、Verify 与业务编排
├── device.py                  # DeviceConfig/DeviceToken 容器与校验
├── device_runtime.py          # 持久 FeiLin worker 和两阶段 token
├── frontend_profile.py        # 公开前端运行常量的密文恢复
├── image_solver.py            # OpenCV 缺口定位与坐标换算
├── vision_bridge.py           # 独立图像解释器 JSON 桥
├── track.py                   # 默认轨迹缩放与 TrackList 序列化
├── default_touch_track.json   # 默认运行必需的脱敏触摸轨迹
├── pe_runtime.py              # PE 子进程调用与结果校验
├── pe_data_bridge.mjs         # 当前动态 PE 的隔离 VM
├── sdk_device_bridge.mjs      # 公开 SDK/FeiLin 的持久 VM
├── data_codec.py              # data/arg 解包、编码与离线校验
├── checksum_bridge.mjs        # data_codec 的可选离线 PE 校验桥
└── protocol.py                # RPC、Verify 和业务签名编码
```

`default_touch_track.json` 不是测试记录，而是默认 `run` 必需的运行资产。它只包含
脱敏的相对触摸轨迹，不包含 token、Cookie、session、CertifyId 或签名密钥。

## 5. 环境与安装

已验证环境：

```text
Python 3.12.13
Node v24.14.1
requests 2.32.5
cryptography 49.0.0
OpenCV 4.13.0
NumPy 2.4.4（图像解释器）
```

主协议解释器需要：

```bash
python3 -m pip install requests cryptography
```

图像解释器需要：

```bash
/path/to/vision-python -m pip install opencv-python numpy
```

也可以让同一个 Python 安装全部四个依赖，并把它同时作为主解释器和
`--vision-python`。

## 6. 运行

以下命令都从本 README 的上一级工作区根目录执行。

先确认入口和参数：

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli --help
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli solve --help
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli run --help
```

### 6.1 只 Init、下载和识别，不发送 Verify

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli solve \
  --vision-python /path/to/vision-python
```

`solve` 会执行 DeviceToken Init、`InitCaptchaV3`、资源下载、UploadLog 和图片
识别，但不会构造或发送 Verify。

### 6.2 完成一轮验证码，只发送一次 Verify

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli run \
  --vision-python /path/to/vision-python
```

`--sdk-js` 可以省略；省略时会下载官方公开 SDK。若要锁定本地副本：

```bash
--sdk-js /path/to/public/AliyunCaptcha.js
```

默认不写运行产物，图片和动态 PE 位于自动清理的临时目录。仅在调试时显式保留：

```bash
--artifacts-dir /private/tmp/ali-slider-run
```

默认非业务 `run` 的 stdout 只有四字段：

```json
{
  "securityToken": "<本轮返回值>",
  "VerifyCode": "T001",
  "VerifyResult": true,
  "certifyId": "<本轮 CertifyId>"
}
```

这四个字段包含本轮敏感结果，不要把 stdout 随意写入日志或提交到仓库。成功和
验证码失败的退出码分别为 `0` 和 `2`；协议/参数/运行错误为 `1`。

### 6.3 T001 后提交一次用户本地业务请求

原始抓包和当前公开 app bundle 不属于最终源码，也没有保留在本目录。需要该
可选能力时，由用户从受控路径显式提供：

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.cli run \
  --vision-python /path/to/vision-python \
  --submit-business \
  --capture /secure/path/to/raw-capture.json \
  --app-bundle /secure/path/to/current-app-bundle.js
```

程序只在 T001 安全门通过后读取模板、重算时间/nonce/SHA-512 签名并提交一次。
业务退出码为：成功 `0`、验证码失败 `2`、业务 HTTP 非成功状态 `3`。

### 6.4 HTTP 接口

`api.py` 把 `run` 的一轮挑战包装成接口调用，只用标准库 `http.server`，不引入
Web 框架依赖。Node、视觉解释器和超时等运行环境由启动命令固定，逐轮变化的只有
三个请求参数。

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.api \
  --vision-python /path/to/vision-python \
  --host 127.0.0.1 --port 8000
```

请求参数：

```text
SceneId     可选，验证码场景 ID，省略时使用默认场景
proxy       可选，传了则本轮所有请求走该代理，省略或空串则整轮直连
AaduaneId   可选，覆盖 RPC key id，省略时使用当前公开前端恢复出的值
prefix      可选，验证码实例域名前缀，同时决定 Init/Verify 域名
```

```bash
curl -X POST http://127.0.0.1:8000/api/slider \
  -H 'Content-Type: application/json' \
  -d '{"SceneId":"1ug4aptr","proxy":"http://user:pass@host:8080","AaduaneId":"..."}'
```

`GET /api/slider?SceneId=...` 是等价的便捷写法，`GET /health` 用于健康检查。
成功响应在 CLI 四字段之外附带本轮元信息：

```json
{
  "ok": true,
  "securityToken": "<本轮返回值>",
  "VerifyCode": "T001",
  "VerifyResult": true,
  "certifyId": "<本轮 CertifyId>",
  "sceneId": "<本轮场景>",
  "proxied": false,
  "elapsedMs": 2149
}
```

状态码语义：入参不合法 `400`（不消耗挑战）、超出并发上限 `429`、协议或运行错误
`500`。验证码未通过仍是一次正常往返，返回 `200` 且 `ok=false`，由 `VerifyCode`
说明原因。

`proxy` 生效范围覆盖本轮全部三个网络出口，避免同一 `CertifyId` 的请求出现在多个
源 IP 上：

```text
公开 SDK 下载      o.alicdn.com
Init/Verify/日志   <prefix>.captcha-open.aliyuncs.com
图片与动态 PE      static-captcha.aliyuncs.com、g.alicdn.com
```

`proxy` 接受纯 `ip:port`，省略 scheme 时按 `http://` 处理；也接受完整 URL 和带
认证的写法：

```text
1.1.1.1:4321                  → http://1.1.1.1:4321
user:pass@1.1.1.1:4321        → http://user:pass@1.1.1.1:4321
http://1.1.1.1:4321           → 原样
socks5://1.1.1.1:4321         → 原样，需额外安装 requests[socks]
```

接口默认不限制并发，`--max-concurrency N` 才会把同时进行的挑战数限制为 N 并对
超出部分返回 `429`。每轮挑战使用独立的客户端、FeiLin worker、临时目录与
`CertifyId`，不共享任何可变状态；单次 Verify 的边界仍然逐轮成立。

日志只记录方法、路径和状态码，`proxy` 凭据与 `securityToken` 不会写入 stderr。

并发实测（本机 16 核 / 48GB，全部直连同一出口 IP）：

```text
串行  8 轮   T001 7、置信度保护 1        单轮中位 1.9s
并发 30 轮   T001 22、F001 3、F015 2、
             置信度保护 3               单轮中位 3.2s，总墙钟 3.5s
```

并发下多出的 `F001`/`F015` 来自同一出口 IP 的高频请求，不是本地时序或轨迹问题；
每轮使用不同 `proxy` 时不适用。置信度保护在两种模式下都会出现，属图像识别的固有
边界，此时不会发送 Verify，也不消耗挑战。

## 7. 清理前后的验证

删除测试源码前，当前生产代码通过：

```text
主解释器 unittest：Ran 118 tests，OK，skipped=10
OpenCV 专项：Ran 19 tests，OK
```

10 项跳过是主解释器没有安装 OpenCV；相同图像测试已由独立 OpenCV 解释器全部
执行通过。测试源码按本次“只保留最终可运行文件”的要求移出工作区。

清理后已经重新验证：

```bash
node --check ali_slider_reverse/sdk_device_bridge.mjs
node --check ali_slider_reverse/pe_data_bridge.mjs
node --check ali_slider_reverse/checksum_bridge.mjs
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$PWD" \
  python3 -m ali_slider_reverse.cli --help
```

同时确认默认运行资产可解析为 86 个事件、三个 CLI 帮助入口均正常退出，以及
图像解释器可以导入 OpenCV 4.13.0 / NumPy 2.4.4。这里没有再次发送真实 Init、
Verify 或业务请求，避免仅为清理验收而消耗新挑战或产生外部副作用。

## 8. 运行边界

- `CertifyId`、DeviceConfig、图片、动态 PE、时间和轨迹都与本轮挑战绑定，不得
  跨挑战复用。
- 公开 SDK、FeiLin、动态 PE、图片格式或服务端 schema 更新后，旧验证证据失效。
- 图像置信度或候选共识不足时应停止，不应靠扫描邻近坐标消耗 Verify。
- `--x-pos` 仅用于授权研究和人工复核，自动主链默认使用 OpenCV。
- 虚拟时钟不缩短 CDN、Init、UploadLog 或 Verify 的公网响应时间；2～3 秒实测
  不能视为硬实时 SLA。
- `frontend_profile.py` 只保存公开 JavaScript 中的密文；不要打印
  `resolve_frontend_secrets()` 的返回值。
- 原始抓包、Cookie、业务 token、响应正文和 app bundle 不应进入源码、日志或
  Markdown 记录。
