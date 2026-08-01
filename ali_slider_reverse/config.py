"""集中式运行常量。

重构前同一个默认值会同时硬编码在 ``client.py``、``cli.py``、``api.py`` 和两个
runtime 模块里（超时、GatherCost 区间、首触年龄、vision 解释器路径……），改一处
就要记得改另外三处。这里作为唯一事实来源：

* **协议常量** —— 由当前公开 SDK/服务端契约决定，改动意味着协议版本变了；
* **运行默认值** —— 只是默认行为，入口层允许通过命令行参数覆盖。

本模块只保存公开的、非敏感的值。前端密文常量在
:mod:`ali_slider_reverse.protocol.secrets`，运行时才解密。
"""

from __future__ import annotations

import sys
from pathlib import Path


# ==========================================================================
# 冻结分发（PyInstaller）
#
# 打包后的可执行文件里没有 pip、没有 PATH 上的 node，也没有第二个装了 OpenCV 的
# 解释器。所以运行时要能自己回答两个问题：Node 在哪、图像识别怎么跑。
# ==========================================================================

FROZEN = bool(getattr(sys, "frozen", False))
"""是否运行在 PyInstaller 冻结分发里。"""

BUNDLE_ROOT = Path(getattr(sys, "_MEIPASS", "")) if FROZEN else None
"""冻结分发的资源根目录；非冻结运行时为 ``None``。"""

VISION_IN_PROCESS = "<in-process>"
"""``vision_python`` 的哨兵值：不拉起子解释器，直接在本进程内求解。

冻结分发已经把 OpenCV/NumPy 打进同一个可执行文件，再 spawn 一个"带 OpenCV 的
Python"既找不到、也没必要。
"""


def _bundled_node_binary() -> str | None:
    """定位随包携带的 Node 可执行文件。

    先看可执行文件旁边（允许分发后自行替换版本），再看打包资源目录。两处都没有
    时返回 ``None``，由调用方回落到 PATH 上的 ``node``。
    """

    if not FROZEN:
        return None
    name = "node.exe" if sys.platform == "win32" else "node"
    roots = [Path(sys.executable).resolve().parent]
    if BUNDLE_ROOT is not None:
        roots.append(BUNDLE_ROOT)
    for root in roots:
        candidate = root / "node" / name
        if candidate.is_file():
            return str(candidate)
    return None


# ==========================================================================
# 包内资源路径
# ==========================================================================

PACKAGE_ROOT = Path(__file__).resolve().parent
"""包根目录；Node 桥脚本与默认轨迹资产都相对它定位。

冻结分发下 ``__file__`` 指向解包目录内的同名路径，因此这里无需特判。
"""

BRIDGES_DIR = PACKAGE_ROOT / "runtime" / "bridges"
"""Node 侧桥脚本目录。"""

SDK_DEVICE_BRIDGE = BRIDGES_DIR / "sdk_device_bridge.mjs"
"""公开 SDK/FeiLin 的持久 VM 脚本。"""

PE_DATA_BRIDGE = BRIDGES_DIR / "pe_data_bridge.mjs"
"""本轮动态 PE 的隔离 VM 脚本。"""

DEFAULT_TOUCH_TRACK = PACKAGE_ROOT / "challenge" / "default_touch_track.json"
"""默认运行必需的脱敏触摸轨迹；不含 token、Cookie、CertifyId 或签名密钥。"""


# ==========================================================================
# 协议常量：由当前公开 SDK 与服务端契约决定
# ==========================================================================

CAPTCHA_API_VERSION = "2023-03-05"
"""验证码 OpenAPI 版本，参与 RPC v1 签名。"""

DEFAULT_SCENE_ID = "1ug4aptr"
"""默认验证码场景 ID。"""

DEFAULT_PREFIX = "fsgtmi"
"""默认验证码实例前缀；它同时决定 Init 与 Verify 的域名。"""

DEFAULT_REGION = "cn"
"""设备链使用的区域标识。"""

UPLOAD_URL = "https://upload.captcha-open.aliyuncs.com/"
"""best-effort 遥测上报地址；与实例 prefix 无关。"""

IMAGE_BASE = "https://static-captcha.aliyuncs.com/"
"""back.png / shadow.png 的公开 CDN 前缀。"""

PE_BASE = "https://g.alicdn.com/captcha-frontend/dynamicJS/"
"""动态 PE 与 main.css 的公开 CDN 前缀。"""

SDK_URL = (
    "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
)
"""公开 AliyunCaptcha.js 下载地址。"""

ALLOWED_SDK_HOSTS = frozenset({"o.alicdn.com", "g.alicdn.com"})
"""允许下载公开 SDK 的主机白名单。"""

ALLOWED_ASSET_HOSTS = frozenset(
    {"static-captcha.aliyuncs.com", "g.alicdn.com"}
)
"""允许下载图片与动态 PE 的主机白名单。"""

BUSINESS_SALT_SHA256 = (
    "f97a59184f21e111608dac2e6397f2cf07866638e5df657937e98f36279615d6"
)
"""业务 SHA-512 签名尾串的 SHA-256 指纹。

用哈希锁定目标字面量，避免把二次提取的明文常量复制进源码，也避免把 app
bundle 中相邻的短字符串误识别为签名盐。
"""


def init_url(prefix: str = DEFAULT_PREFIX) -> str:
    """返回该实例的 ``InitCaptchaV3`` 域名。"""

    return f"https://{prefix}.captcha-open.aliyuncs.com/"


def verify_url(prefix: str = DEFAULT_PREFIX) -> str:
    """返回该实例的 ``VerifyCaptchaV3`` 域名。

    Init 与 Verify 是同一实例的两个域名，覆盖 prefix 时必须同步，
    否则会用 A 实例的域名发送 B 实例的 SceneId。
    """

    return f"https://{prefix}-verify.captcha-open.aliyuncs.com/"


# ==========================================================================
# 浏览器画像：随请求头发出，需与 Node 桥内的 UA 保持一致
# ==========================================================================

ORIGIN = "http://localhost:38185"
"""本地授权环境的页面来源。"""

REFERER = ORIGIN + "/"

USER_AGENT = (
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) "
    "AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 "
    "Mobile/15E148 Safari/604.1"
)

SEC_CH_UA = (
    '"Not;A=Brand";v="8", "Chromium";v="150", '
    '"Google Chrome";v="150"'
)


def browser_headers(
    *,
    referer: str = REFERER,
    origin: str = ORIGIN,
    destination: str = "empty",
    mode: str = "cors",
    include_origin: bool = True,
) -> dict[str, str]:
    """构造与真实页面一致的请求头。

    ``Sec-Fetch-*`` 三兄弟随请求类型变化：RPC 是 ``empty/cors``，静态资源是
    ``image|script|style`` 配 ``no-cors`` 且不带 ``Origin``。这些值浏览器会自动
    填，服务端也可能据此判断请求来源，因此按实际场景传参而不是写死一套。
    """

    headers = {
        "Accept": "*/*",
        "Accept-Language": "zh-CN,zh;q=0.9",
        "Referer": referer,
        "Sec-CH-UA": SEC_CH_UA,
        "Sec-CH-UA-Mobile": "?1",
        "Sec-CH-UA-Platform": '"iOS"',
        "Sec-Fetch-Dest": destination,
        "Sec-Fetch-Mode": mode,
        "Sec-Fetch-Site": "cross-site",
        "Priority": "u=1, i",
        "User-Agent": USER_AGENT,
    }
    if include_origin:
        headers["Origin"] = origin
    return headers


# ==========================================================================
# 滑块几何：与页面 DOM 渲染尺寸绑定
# ==========================================================================

SLIDER_RENDERED_WIDTH = 300
"""滑轨在 DOM 中的渲染宽度（px）。"""

SLIDER_HANDLE_WIDTH = 40
"""滑块手柄宽度（px）；最大位移为 rendered - handle。"""


# ==========================================================================
# 运行默认值：入口层可通过命令行覆盖
# ==========================================================================

DEFAULT_NODE_BINARY = _bundled_node_binary() or "node"
"""Node 可执行文件。

冻结分发优先用随包携带的那一份，保证目标机器没装 Node 也能跑；否则取 PATH。
"""

DEFAULT_VISION_PYTHON = (
    VISION_IN_PROCESS
    if FROZEN
    else ("/opt/homebrew/bin/python3" if sys.platform == "darwin" else sys.executable)
)
"""默认的 OpenCV/NumPy 解释器。

主协议解释器只需 requests + cryptography，图像识别则需要 opencv-python +
numpy。两者常常不是同一个环境，因此拆成独立进程；macOS 的默认值对应已验证的
Homebrew 环境，其他平台默认取当前解释器，都可用 ``--vision-python`` 覆盖。

冻结分发没有"第二个解释器"可言，取 :data:`VISION_IN_PROCESS` 走进程内求解。
"""

DEFAULT_TIMEOUT = 25.0
"""单步网络请求或子进程调用的超时秒数。"""

DEFAULT_MIN_CONFIDENCE = 0.45
"""低于此图像置信度就停止，且不发送 Verify、不消耗挑战。

这个值与 ``vision.gap_solver`` 的置信度标定配套：在 50 张真实挑战图上全部放行
（最低 0.47），在同一批图做过缺口修补（即真的没有缺口）的 50 张负样本上误放行
6 张。改动阈值前请先看 README 第 8 节的离线基准。
"""

DEFAULT_GATHER_COST_RANGE = (180, 260)
"""Node 补环境过快导致 GatherCost 落到 0ms 时的正常化区间（毫秒）。

真实页面采集约 226ms，同一公开脚本在轻量 Node DOM 中会瞬间完成。
"""

DEFAULT_FIRST_TOUCH_AGE_RANGE = (650, 850)
"""首个 touch 相对 TrackStartTime 的逻辑年龄区间（毫秒）。

这是逻辑时钟上的时长，不会真实 sleep。需要复现实验中的长分布时，可通过
``--first-touch-age-min/max`` 显式传入更大的值。
"""

VERIFY_FUTURE_SKEW_LIMIT_MS = 2_000
"""VerifyTime 允许超前当前墙钟的上限（毫秒）。

虚拟时钟快速回放后可能生成"未来的 VerifyTime"，发请求前独立拒绝掉。
"""

SDK_CACHE_TTL_SECONDS = 900.0
"""公开 ``AliyunCaptcha.js`` 本地缓存的有效期（秒）。

这是个 225KB 的公开静态文件，每轮重新下载要付一次 CDN 往返（实测约 120ms），
而它的更新频率是天/周级。15 分钟意味着每天仍会重新拉取近百次，足够跟上前端热
更新，同时把绝大多数轮次的这次下载降到零。

设为 0 或负数则每轮都重新下载；``--sdk-js`` 指定本地副本时缓存完全不参与。
"""


# ==========================================================================
# HTTP 接口默认值
# ==========================================================================

API_HOST = "127.0.0.1"
API_PORT = 8000
API_SOLVE_PATH = "/api/slider"
API_HEALTH_PATH = "/health"

API_MAX_BODY_BYTES = 64 * 1024
"""请求体上限；一轮挑战的入参只有几个短字符串，不需要更大。"""


__all__ = [
    "ALLOWED_ASSET_HOSTS",
    "ALLOWED_SDK_HOSTS",
    "API_HEALTH_PATH",
    "API_HOST",
    "API_MAX_BODY_BYTES",
    "API_PORT",
    "API_SOLVE_PATH",
    "BRIDGES_DIR",
    "BUNDLE_ROOT",
    "BUSINESS_SALT_SHA256",
    "CAPTCHA_API_VERSION",
    "DEFAULT_FIRST_TOUCH_AGE_RANGE",
    "DEFAULT_GATHER_COST_RANGE",
    "DEFAULT_MIN_CONFIDENCE",
    "DEFAULT_NODE_BINARY",
    "DEFAULT_PREFIX",
    "DEFAULT_REGION",
    "DEFAULT_SCENE_ID",
    "DEFAULT_TIMEOUT",
    "DEFAULT_TOUCH_TRACK",
    "DEFAULT_VISION_PYTHON",
    "FROZEN",
    "IMAGE_BASE",
    "ORIGIN",
    "PACKAGE_ROOT",
    "PE_BASE",
    "PE_DATA_BRIDGE",
    "REFERER",
    "SDK_CACHE_TTL_SECONDS",
    "SDK_DEVICE_BRIDGE",
    "SDK_URL",
    "SEC_CH_UA",
    "SLIDER_HANDLE_WIDTH",
    "SLIDER_RENDERED_WIDTH",
    "UPLOAD_URL",
    "USER_AGENT",
    "VERIFY_FUTURE_SKEW_LIMIT_MS",
    "VISION_IN_PROCESS",
    "browser_headers",
    "init_url",
    "verify_url",
]
