# 阿里 V3 滑块纯协议复现

> 仅用于用户明确授权的本地 CTF、兼容性研究与协议验证环境。禁止用于批量解题、
> 并发挑战、同一挑战重试、规避第三方访问控制或任何未授权目标。

## 1. 这是什么

不启动 Chrome，不依赖 Playwright、Selenium、Puppeteer、CDP 或人工拖动，纯协议
完成一轮阿里 V3 滑块验证。各层职责严格分开：

```text
Python    HTTP/RPC 编排、资源下载、结构校验和单次 Verify 安全门
Node VM   执行当前公开 AliyunCaptcha.js、FeiLin 和本轮动态 pe.*.js
OpenCV    识别缺口并给出 xPos
```

在 2026-07-31 的用户授权环境中观察到：新挑战只发送一次 `VerifyCaptchaV3`；返回
`VerifyCode=T001`、`VerifyResult=true` 与非空 `securityToken`；只有安全门全部满足
后才提交一次本地业务请求；全程没有浏览器、刷新或同一 `CertifyId` 重试。

这些是特定公开 SDK、动态 PE、服务端契约和运行环境下的实测结果，**不是**对未来
版本或任意挑战永久成功的保证。

## 2. 运行链路

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

核心原则：Python 负责可审计的控制流，动态且易变的前端行为交给本轮公开 JavaScript
自己执行，最终结果再由 Python 独立解包和校验。

## 3. 项目结构

```text
ali_slider_reverse/
├── config.py              全部默认值与协议常量的唯一来源
├── errors.py              统一异常层级，入口层捕获 AliSliderError 即可
│
├── protocol/              纯算法，无 IO，可脱离网络单独测试
│   ├── signing.py         浏览器等价编码 + 阿里 RPC v1 HMAC-SHA1 签名
│   ├── params.py          Verify 与业务请求的参数封装
│   ├── device_token.py    DeviceToken/DeviceConfig 容器的拆装与校验
│   ├── data_codec.py      CaptchaVerifyParam.data 的逐层解包与 schema 校验
│   └── secrets.py         公开前端静态密文的运行时恢复
│
├── vision/                图像
│   ├── geometry.py        xPos ↔ slidePos 运动式与 JS 取整语义（纯标准库）
│   └── gap_solver.py      缺口渲染模型驱动的定位与复核（惰性导入 OpenCV）
│
├── runtime/               子进程与 VM 桥，重环境依赖都隔离在这一层
│   ├── node_device.py     FeiLin 设备链，跨 Init 保持同一个 Node VM
│   ├── node_pe.py         动态 PE 的隔离 VM，原生生成 data 并独立复核
│   ├── vision.py          OpenCV 求解子进程的预热与消费
│   └── bridges/
│       ├── sdk_device_bridge.mjs   公开 SDK/FeiLin 的持久 VM
│       └── pe_data_bridge.mjs      当前动态 PE 的隔离 VM
│
├── challenge/             编排层，持有全部对外网络出口
│   ├── session.py         一轮挑战的状态机与单次 Verify 边界
│   ├── assets.py          四项公开资源的并发下载与 PNG 尺寸解析
│   ├── transport.py       共享连接池与设备链空窗期的 TLS 预热
│   ├── track.py           触摸轨迹资产的读取与缩放
│   ├── business.py        可选的业务请求提交
│   └── default_touch_track.json    脱敏触摸轨迹（运行必需资产）
│
├── entrypoints/
│   ├── cli.py             solve / run 命令
│   ├── api.py             标准库 http.server 实现的 HTTP 接口
│   ├── vision_worker.py   OpenCV 求解子进程入口
│   └── options.py         两个入口共享的参数定义与对象装配
```

依赖方向严格单向：`entrypoints → challenge → runtime → {protocol, vision}`。三个桥
都不发网络请求，所以代理设置、请求头画像与"每个 CertifyId 只 Verify 一次"的约束
都能在 `challenge` 一层集中审计。

`default_touch_track.json` 不是测试记录，而是默认 `run` 必需的运行资产。它只包含
脱敏的相对触摸轨迹，不含 token、Cookie、session、CertifyId 或签名密钥。

## 4. 真正决定成败的设计决策

### 4.1 DeviceToken 必须保持同一个 FeiLin VM

DeviceToken 不是静态指纹字符串，也不能在 Init 与 Verify 阶段各启动一个互不相关的
环境。正确链路是：

```text
公开 SDK / FeiLin
  → Log1 → 解密 DeviceConfig → Log2 → Init DeviceToken
  → 保持 worker 和 session 存活
  → 回放本轮 PE getter 前已经发生的交互
  → 实际一参 getToken(...)
  → Log3 → Log2 → Verify DeviceToken
```

Python 会解析并校验 DeviceToken 的容器、session、AES/MD5 结构，但最终在线 token 由
公开 FeiLin 原调用链生成。只替换 UA、伪造字段数量或重新启动第二个 VM，都会破坏
跨阶段的状态一致性。

### 4.2 最终 `data` 应由当前动态 PE 原生构造

不能把某个旧 `pe.*.js` 的混淆字段、checksum 或 payload 分片硬编码成长期协议。每轮
Init 返回新的 `StaticPath`；实现会下载该挑战对应的动态 PE，并在隔离、禁网的 Node
`vm` 中加载公开 SDK 与本轮 PE、提供 PE 实际读取的最小 DOM/事件/时钟接口、回放触摸
轨迹、截获 `captchaVerifyCallback`、保留 PE 原生生成的完整 `data`，最后由 Python
解包并严格检查 schema、字段顺序、坐标、时间和 getter 时序。

这样既避免 Python 猜测动态密文，也避免 Node VM 直接发送网络请求。

### 4.3 `86 → 86 → 87` 是三个不同的时序合同

当前公开 PE 的关键观察是这三个数**互不相等**：

```text
输入 touch 事件                     86
getter 调用前已形成的 native mm      86
最终 data.TrackList.mm              87    ← 多出 getter 之后的 RAF 尾项
```

因此：Verify `data` 必须保留序列化时存在的全部 87 条 native `mm`；FeiLin getter 只
能看到 getter 调用前的 86 条**连续前缀**；不能把 getter 后的 RAF 尾项提前回放给
FeiLin；`mm.timeStamp` 与 getter `observedAtMs` 必须来自同一个 PE VM 的
`performance.now()` 时间域。

这是最容易把"最终 payload 状态"与"getter 当时状态"混为一谈的地方。

### 4.4 虚拟时钟压缩等待，但不删除时间语义

正确的优化不是删掉 36 秒首触时龄或 2.233 秒轨迹，而是分离两个时间域：

```text
逻辑时间   TrackStartTime、firstTouchAge、每个 dt、timeStamp、VerifyTime
真实墙钟   JavaScript 执行、HTTP、OpenCV、子进程和事件循环 turn
```

PE 使用显式、单调、离散推进的逻辑时钟。每个事件之后仍跨一个真实 `setTimeout(0)`
macrotask，让 dispatch 期间同步安排的 RAF/host callback 在下一事件前完成，但不会按
逻辑 `dt` 真实 sleep。逻辑 epoch 会整体向过去对齐，避免快速回放后生成"未来的
VerifyTime"；Python 在发送前还会独立拒绝超过当前墙钟 2 秒的 `VerifyTime`。

默认首 touch 逻辑时龄为 650～850ms；如需复现实验中的长分布：

```bash
--first-touch-age-min 36000 --first-touch-age-max 36250
```

这只改变逻辑时间，不会真实等待约 36 秒。

### 4.5 缺口是"叠上去的半透明白"，不是抠掉的洞

这是图像识别一直做不准的根因。`back.png` 里的缺口不是把一块像素挖走，而是在原图上
叠了一层拼图形状的半透明白。在 50 张真实样本上做线性拟合（BGR 三通道系数一致）：

```text
fill = 0.224 * background + 187        等价于以 α≈0.776 混向 #F1F1F1
```

这条式子直接给出三条可以当判据用的性质：

```text
1  填充色落在 [150, 244]   上下界的性质完全不同，不要对称地调：
                           fill_max = 255 - 14α，α∈[0.6,1.0] 时只在 241~247 之间动；
                           fill_min = 241α，同区间内从 145 一直跑到 241。
                           上界几乎不随不透明度变化，且正是否掉雪地/云层的判据，必须收紧；
                           下界随 α 剧烈漂移，写死实测值会让前端一改透明度就大面积漏检，
                           所以放宽到 150（可容忍 α ≥ 0.62）。
2  色度压到原来的 22%      缺口在任何画面上都低饱和。
3  局部对比度也压到 22%    通常平坦，但雪山岩石这类高反差背景下缺口内部仍有起伏，
                           所以平坦度只能当弱证据，不能当门槛。
```

第 1 条的上界是唯一的**硬约束**，也是雪地、云层这些历史上最容易翻车的画面能被否掉的
原因：它们整片又亮又平又低饱和，只有"亮过 244"这一条能把它们判死。

几何上，缺口边界是 1px 硬阶跃，位置固定在 alpha 形状的带符号距离 `+2` 处。按"亮块
边缘对齐 alpha 边缘"会系统性偏 2px，所以模板建在**带符号距离场**上，过渡带做窄
（宽过渡带会把相关峰摊平）。

坐标语义上，协议需要的是整个 shadow **画布**在 `back.png` 原始像素坐标系中的左坐标，
因此求解全程以画布左坐标为单位滑动，不再需要 `alpha_bbox.left` 换算。

求解链路：

```text
逐像素缺口似然（颜色区间 × 低色度）
   ↓
三路互补的模板相关：似然 CCORR、亮度零均值 NCC、色度零均值 NCC
   ↓   失效场景互不重叠——雪地让似然处处成立，灰阶画面让色度失效，
   ↓   大片天空会把零均值相关吸引过去
各路取峰 → 并集 ±3px 逐一复核
   ↓
复核：边界阶跃一致性 / 混合模型局部符合率 / 区域可分性 / 颜色区间占比
   ↓
xPos + 置信度
```

复核里判别项（阶跃、混合、可分性、区间占比）与门控项（颜色、平坦度）是**相乘**而不是
相加：缺口压在雪地上时门控项在任何位置都成立，相加会被直接带偏。

置信度由复核质量、峰间差距与边界阶跃一致性经 logistic 映射得到，系数在两组各 50 张的
样本上标定——正样本是真实挑战图，负样本是把同一批图的缺口修补掉之后的版本。

### 4.6 `xPos` 与 `slidePos` 必须分别校验

当前 PE 的运动关系为：

```text
xPos = s * (3*s + 65) / 845
s    = (-65 + sqrt(65² + 12*845*xPos)) / 6
```

其中 `s` 是手柄位移 `slidePos`。逆根先限制到可拖动范围，再使用非负输入上的
JavaScript `Math.round` 语义（即 `floor(value + 0.5)`，不能用 Python 的银行家舍入）。
最终 PE 会原生生成两者，Python 和 Node 各自检查它们与图像预期值的偏差；超过允许
范围时在 Verify 前停止。

### 4.7 Verify 必须是不可重试的单次消耗点

控制流只有一个 Verify 调用点：

```text
图片共识通过
  AND PE schema/坐标/时间通过
  AND FeiLin getter 与 session 通过
    → 每个 CertifyId 最多一次 VerifyCaptchaV3
```

图片弱、资源失败、schema 不符、时间异常或 getter 不一致时都应在网络 Verify 之前
停止。收到 F015 或其他失败码后不能复用同一 `CertifyId` 试邻近坐标。

可选业务提交的硬门是 `VerifyCode == "T001"` **且** `VerifyResult == true` **且**
`securityToken` 非空；任一条件不满足都不发送业务请求。

### 4.8 并发只用于相互独立的本轮工作

Init 之前没有本轮图片和 `StaticPath`，无法安全预取 challenge-bound 资源。Init 之后
可并行的只有四项公开资源下载，以及只尝试一次的 best-effort `UploadLog` 与本地
OpenCV 识别。

`UploadLog` **不挡 PE**。它不参与任何控制流，公开 SDK 也不 await 它，因此没有理由
让 PE 站在那里干等一次完整的网络往返（实测约 300ms）。它的 worker 注册在本轮的
`ExitStack` 上，任何退出路径都会 join，不会在后台悬空；结果在 Verify 之后读取——
那时这次 join 反正要发生，取值不增加任何等待。它也走独立的 `requests.Session`，
不与主 RPC 会话并发共用。

还有一段容易被忽略的空窗：设备链（SDK 准备 → Node 启动 → Log1 → Log2）期间
Python 侧完全阻塞在读子进程输出上。本轮五个出网主机的 TLS 握手就安排在这里：

```text
o.alicdn.com                       公开 SDK（命中缓存时不出网）
<prefix>.captcha-open              Init
<prefix>-verify.captcha-open       Verify   ← 原本要等到最后一步才第一次握手
upload.captcha-open                UploadLog
static-captcha / g.alicdn          图片与动态 PE
```

预热只建立 TCP + TLS 连接并放回 urllib3 连接池，**不发送任何 HTTP 字节**，没有协议
侧副作用。走代理时整体跳过——urllib3 的 `CONNECT` 隧道要到 `urlopen` 内部才建立，
直接预连只会得到没有隧道的半成品连接。

`requests.Session` 仍然按线程隔离（它不承诺跨线程共享），但底层 `HTTPAdapter` 由
整轮共享——真正持有热连接的 `PoolManager` 本身是线程安全的。短会话结束时必须先把
共享 adapter 摘掉再 `close()`，否则一次下载就把整轮预热的连接全部销毁。

单轮之内不并发多个挑战。HTTP 接口允许多轮同时进行，但每轮持有自己的客户端、FeiLin
worker、临时目录和 `CertifyId`，轮与轮之间没有共享可变状态。

`GatherCost` 是唯一一处曾隐含"单轮独占 CPU"假设的地方：Node 桥在 Init 与 Verify
阶段各测一次采集耗时，空闲时两次都落到 0ms 并被统一归一化，并发抢占 CPU 时两次都
会超过下限且互不相等。这属于桥的测量方式，不代表 FeiLin session 不一致，因此统一取
先发生的 Init 耗时——真实页面里 Verify token 复用的也正是 Init 之前那次采集。


### 4.9 逻辑时间不 sleep，宿主 turn 也不该 sleep

虚拟时钟已经保证不按逻辑 `dt` 真实等待，但回放循环里"每条事件跨一个宿主 turn"
这件事本身也有代价：Node 把 `setTimeout(0)` 抬到 1ms，86 条事件就是 106ms。

`setImmediate` 跨的是同一个完整事件循环轮次（timers → poll → check），已到期的
timer 与排队的 microtask 照常执行，语义等价但没有那 1ms 下限——实测 86 次从
106ms 降到 1.7ms。设备桥回放 FeiLin mousemove 流时用的就是它。

动态 PE 桥的回放循环**没有**跟着改：那里的 `requestAnimationFrame` 是用
`setTimeout(cb, 1)` 实现的，宿主 turn 必须久到让当帧 RAF 回调在下一条事件前跑完，
否则会破坏 4.3 节的 `86 → 86 → 87` 合同。要拿掉那 106ms，得先把 RAF 换成桥自己
维护、每条事件后显式 drain 的队列，并重新验证 mm 计数。


## 5. 环境与安装

已验证环境：

```text
Python 3.12.13    Node v24.14.1
requests 2.32.5   cryptography 49.0.0
OpenCV 4.13.0     NumPy 2.4.4（图像解释器）
```

主协议解释器与图像解释器可以是两个不同的环境：

```bash
# 主协议解释器
python3 -m pip install requests cryptography

# 图像解释器（可以是另一个 Python）
/path/to/vision-python -m pip install opencv-python numpy
```

也可以让同一个 Python 装全四个依赖，并把它同时作为主解释器和 `--vision-python`：

```bash
python3 -m pip install -e '.[vision]'
```

安装后可用 `ali-slider` 与 `ali-slider-api` 两个命令；不安装则用 `python -m` 形式，
下文命令都从本 README 的上一级工作区根目录执行。

## 6. 运行

先确认入口和参数：

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse --help
PYTHONPATH="$PWD" python3 -m ali_slider_reverse solve --help
PYTHONPATH="$PWD" python3 -m ali_slider_reverse run --help
```

### 6.1 只 Init、下载和识别，不发送 Verify

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse solve \
  --vision-python /path/to/vision-python
```

`solve` 会执行 DeviceToken Init、`InitCaptchaV3`、资源下载、UploadLog 和图片识别，
但不会构造或发送 Verify，因此**不消耗挑战**。适合确认环境是否正常。

### 6.2 完成一轮验证码，只发送一次 Verify

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse run \
  --vision-python /path/to/vision-python
```

`--sdk-js` 可以省略；省略时会下载官方公开 SDK。要锁定本地副本时传
`--sdk-js /path/to/public/AliyunCaptcha.js`。

省略时下载到的 SDK 会缓存在 `${XDG_CACHE_HOME:-~/.cache}/ali-slider/`，默认 15 分钟
内复用（`config.SDK_CACHE_TTL_SECONDS`）。它是个 225KB 的公开静态文件，更新频率是
天/周级，而每轮重下要付一次 CDN 往返。写入是先写临时文件再 `os.replace`，并发的另
一轮不会读到半个脚本。想每轮强制重下就把 TTL 设为 0；想完全脱离网络就用 `--sdk-js`。

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

这四个字段包含本轮敏感结果，不要把 stdout 随意写入日志或提交到仓库。

退出码：`0` 成功、`1` 协议/参数/运行错误、`2` 验证码失败、`3` 业务 HTTP 非成功状态。

### 6.3 T001 后提交一次用户本地业务请求

原始抓包和当前公开 app bundle 不属于最终源码，也没有保留在本目录。需要该可选能力
时，由用户从受控路径显式提供：

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse run \
  --vision-python /path/to/vision-python \
  --submit-business \
  --capture /secure/path/to/raw-capture.json \
  --app-bundle /secure/path/to/current-app-bundle.js
```

程序只在 T001 安全门通过后读取模板、重算时间/nonce/SHA-512 签名并提交一次。

## 7. HTTP 接口

把 `run` 的一轮挑战包装成接口调用，只用标准库 `http.server`，不引入 Web 框架依赖。
Node、视觉解释器和超时等运行环境由启动命令固定，逐轮变化的只有四个请求参数。

```bash
PYTHONPATH="$PWD" python3 -m ali_slider_reverse.entrypoints.api \
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
  -d '{"SceneId":"1ug4aptr","proxy":"http://user:pass@host:8080"}'
```

`GET /api/slider?SceneId=...` 是等价的便捷写法，`GET /health` 用于健康检查。成功
响应在 CLI 四字段之外附带 `sceneId`、`proxied`、`elapsedMs` 等本轮元信息。

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

`proxy` 接受纯 `ip:port`（按 `http://` 处理）、完整 URL 和带认证的写法；`socks5://`
需额外安装 `requests[socks]`。

接口默认不限制并发，`--max-concurrency N` 才会把同时进行的挑战数限制为 N 并对超出
部分返回 `429`。日志只记录方法、路径和状态码，`proxy` 凭据与 `securityToken` 不会
写入 stderr。

串行实测（2026-08-01，全部直连同一出口 IP，95 轮）：

```text
批次1  n=20  间隔 1s     刚开始跑     T001 20  100%
批次2  n=30  间隔 1s                 T001 26   87%   验证失败 3、置信度保护 1
批次3  n=30  间隔 1.2s               T001 19   63%   F001 9、F015 1、置信度保护 1
批次4  n=15  冷却 2min + 间隔 4s     T001 15  100%
```

单轮中位 2.0～2.1s。**`F001` 随请求频率单调累积**：前 20 轮零失败，第三批升到 30%，
冷却两分钟并把间隔拉到 4s 后又回到 100%。这不是本地时序、轨迹或图像问题，而是同一
出口 IP 的频控；每轮换 `proxy` 时不适用。

按识别归因的话，95 轮里**没有一轮是因为坐标算错而失败**——11 个失败轮次的图片事后
逐张复核，10 张的红色轮廓与缺口严丝合缝（置信度多为 1.00），剩下 1 张是低对比度云层，
被置信度门槛正确拦下。图片相关的损失是 2/95。

## 8. 开发

### 图像识别的离线基准

参数标定与下面这些数字都基于 50 张真实挑战图（296×200 背景 + 52×200 拼图，覆盖雪山、
海滩、夜景、密林等画面）。这批图属于会话材料，**不在仓库里**；需要复现时用 `solve`
配 `--artifacts-dir` 逐张收集即可（`solve` 不发 Verify，不消耗挑战）。

逐张人工核对（把 alpha 轮廓画在求解结果上放大比对）：

```text
定位正确（红色轮廓与缺口完全贴合）    50 / 50
默认阈值 0.45 下置信度放行           50 / 50   最低 0.47
单张耗时                            约 13 ms（不含 OpenCV 冷导入）
```

冷导入之后的**首次** `solve_gap` 还要再付约 77ms 的 OpenCV 算子初始化。worker 预热
时会用一张合成图空跑一次求解，把这笔也提前付掉——预热窗口本来就有几百毫秒余量，
而真实图片到达时只应剩热调用的耗时。

置信度的负样本基准用同一批图做缺口修补（OpenCV `inpaint`）后得到——即"图里真的没有
缺口"的情形，此时 50 张中有 6 张会误过 0.45 门槛。

另有 500 例合成基准：用修补后的干净背景，按第 4.5 节的渲染式在随机位置回贴缺口：

```text
|误差| <= 1px   97.2%
|误差| <= 2px   97.8%
高置信度选错     0.8%      ← 唯一会真正浪费一次 Verify 的失败模式
```

### 前端改版时还剩多少余量

同一套合成基准扫描渲染参数漂移（`|误差|<=2px` / 高置信度选错）：

```text
不透明度 α   0.60→98%/0.5%   0.68→98%/0.5%   0.776→98%/1.0%   0.90→98%/1.0%
描边内缩     0px→99%/0.5%    2px→98%/1.0%    3px→98%/0.5%     4px→88%/6.0%
毛玻璃模糊   0.8→98%/0.5%    3.0→98%/1.0%    8.0→98%/1.0%
```

不透明度与模糊半径基本不敏感，因为颜色区间的**上界**（`255 - 14α`）几乎不随 α 变化，
而下界已经刻意放宽。真正的软肋是**描边内缩**：超过 3px 就会掉到 88%，且失败会带着高
置信度。前端改了缺口描边宽度时，先调 `gap_solver._INSET`。

### 静态检查

```bash
python3 -m compileall -q ali_slider_reverse
node --check ali_slider_reverse/runtime/bridges/sdk_device_bridge.mjs
node --check ali_slider_reverse/runtime/bridges/pe_data_bridge.mjs
```

### Node 桥的调试模式

`sdk_device_bridge.mjs` 支持 `--mode probe-log1 | live-token | profile-token |
challenge-worker` 四种模式。Python 主链路只使用 `challenge-worker`；其余三种保留为
手动诊断入口，可直接用 `node` 调用，不经过 Python。

## 9. 运行边界

- `CertifyId`、DeviceConfig、图片、动态 PE、时间和轨迹都与本轮挑战绑定，不得跨挑战
  复用。
- 公开 SDK、FeiLin、动态 PE、图片格式或服务端 schema 更新后，旧验证证据失效。
- 图像置信度或候选共识不足时应停止，不应靠扫描邻近坐标消耗 Verify。
- `--x-pos` 仅用于授权研究和人工复核，自动主链默认使用 OpenCV。
- 虚拟时钟不缩短 CDN、Init、UploadLog 或 Verify 的公网响应时间；2～3 秒实测不能视为
  硬实时 SLA。
- `protocol/secrets.py` 只保存公开 JavaScript 中的密文；不要打印
  `resolve_frontend_secrets()` 的返回值。
- 原始抓包、Cookie、业务 token、响应正文和 app bundle 不应进入源码、日志或 Markdown
  记录。
