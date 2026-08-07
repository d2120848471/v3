"""集中式运行常量。

重构前同一个默认值会同时硬编码在 ``client.py``、``cli.py``、``api.py`` 和多个
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

from .device_profile import DeviceProfile

# ==========================================================================
# 冻结分发（PyInstaller）
#
# 打包后的可执行文件里没有 pip，也没有第二个装了 OpenCV 的解释器，因此图像识别
# 需要支持直接在冻结进程内运行。
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


# ==========================================================================
# 包内资源路径
# ==========================================================================

PACKAGE_ROOT = Path(__file__).resolve().parent
"""包根目录；默认轨迹资产相对它定位。

冻结分发下 ``__file__`` 指向解包目录内的同名路径，因此这里无需特判。
"""

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

UPLOAD_LOG_ENABLED = False
"""UploadLog 遥测是否启用。

真实无 UploadLog 请求验证仍可正常获得 T001，因此默认关闭这条不参与
Verify 控制流的 best-effort 遥测。发送实现仍保留，需要对照公开 SDK
行为时可临时打开。
"""

IMAGE_BASE = "https://static-captcha.aliyuncs.com/"
"""back.png / shadow.png 的公开 CDN 前缀。"""

DEVICE_ENDPOINT = (
    "https://cloudauth-device-dualstack.cn-shanghai.aliyuncs.com"
)
"""公开 FeiLin 设备日志 RPC；Log1/Log2/Log3 均发往此处。"""

ALLOWED_ASSET_HOSTS = frozenset({"static-captcha.aliyuncs.com"})
"""允许下载验证码图片的主机白名单。"""

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
# 页面来源：与设备无关，因此仍是常量
#
# 设备相关的 UA、Sec-CH-UA、语言全部来自逐轮生成的
# :class:`~ali_slider_reverse.device_profile.DeviceProfile`——写死它们等于让
# 服务端拿到一个恒定的设备主键。
# ==========================================================================

ORIGIN = "http://localhost:38185"
"""本地授权环境的页面来源。"""

REFERER = ORIGIN + "/"


def browser_headers(
    *,
    profile: DeviceProfile,
    referer: str = REFERER,
    origin: str = ORIGIN,
    destination: str = "empty",
    mode: str = "cors",
    include_origin: bool = True,
) -> dict[str, str]:
    """构造与本轮设备画像一致的请求头。

    ``Sec-Fetch-*`` 三兄弟随请求类型变化：RPC 是 ``empty/cors``，静态资源是
    ``image|script|style`` 配 ``no-cors`` 且不带 ``Origin``。这些值浏览器会自动
    填，服务端也可能据此判断请求来源，因此按实际场景传参而不是写死一套。

    ``profile`` 必须与本轮纯 Python 指纹和 PE 计算用的是同一套，否则服务端会
    同时看到 HTTP 层与指纹层两台不同的设备。
    """

    headers = {
        "Accept": "*/*",
        "Accept-Language": profile.accept_language,
        "Referer": referer,
        "Sec-CH-UA": profile.sec_ch_ua,
        "Sec-CH-UA-Mobile": profile.sec_ch_ua_mobile,
        "Sec-CH-UA-Platform": profile.sec_ch_ua_platform,
        "Sec-Fetch-Dest": destination,
        "Sec-Fetch-Mode": mode,
        "Sec-Fetch-Site": "cross-site",
        "Priority": "u=1, i",
        "User-Agent": profile.user_agent,
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
"""单步网络请求或视觉 worker 调用的超时秒数。"""

DEFAULT_MIN_CONFIDENCE = 0.45
"""低于此图像置信度就停止，且不发送 Verify、不消耗挑战。

这个值与 ``vision.gap_solver`` 的置信度标定配套：在 50 张真实挑战图上全部放行
（最低 0.47），在同一批图做过缺口修补（即真的没有缺口）的 50 张负样本上误放行
6 张。改动阈值前请先看 README 第 8 节的离线基准。
"""

DEFAULT_GATHER_COST_RANGE = (180, 260)
"""指纹采集 GatherCost 的正常浏览器区间（毫秒）。"""

DEFAULT_FIRST_TOUCH_AGE_RANGE = (650, 850)
"""首个 touch 相对 TrackStartTime 的逻辑年龄区间（毫秒）。

这是逻辑时钟上的时长，不会真实 sleep。需要复现实验中的长分布时，可通过
``--first-touch-age-min/max`` 显式传入更大的值。
"""

VERIFY_FUTURE_SKEW_LIMIT_MS = 2_000
"""VerifyTime 允许超前当前墙钟的上限（毫秒）。

逻辑时钟快速构造后可能生成"未来的 VerifyTime"，发请求前独立拒绝掉。
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
    "API_HEALTH_PATH",
    "API_HOST",
    "API_MAX_BODY_BYTES",
    "API_PORT",
    "API_SOLVE_PATH",
    "BUNDLE_ROOT",
    "BUSINESS_SALT_SHA256",
    "CAPTCHA_API_VERSION",
    "DEFAULT_FIRST_TOUCH_AGE_RANGE",
    "DEFAULT_GATHER_COST_RANGE",
    "DEFAULT_MIN_CONFIDENCE",
    "DEFAULT_PREFIX",
    "DEFAULT_REGION",
    "DEFAULT_SCENE_ID",
    "DEFAULT_TIMEOUT",
    "DEFAULT_TOUCH_TRACK",
    "DEFAULT_VISION_PYTHON",
    "DEVICE_ENDPOINT",
    "FROZEN",
    "IMAGE_BASE",
    "ORIGIN",
    "PACKAGE_ROOT",
    "REFERER",
    "SLIDER_HANDLE_WIDTH",
    "SLIDER_RENDERED_WIDTH",
    "UPLOAD_URL",
    "UPLOAD_LOG_ENABLED",
    "VERIFY_FUTURE_SKEW_LIMIT_MS",
    "VISION_IN_PROCESS",
    "browser_headers",
    "init_url",
    "verify_url",
]
