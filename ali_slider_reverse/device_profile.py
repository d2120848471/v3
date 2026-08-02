"""逐轮生成的浏览器设备画像。

## 为什么需要

FeiLin 采集的指纹明文里，绝大多数字段来自浏览器环境本身：UA、屏幕、GPU、核数、
内存、Canvas 读回值……只要这些字节恒定，服务端就能把它们哈希成一个稳定的设备主
键。用同一套常量跑几百轮，等于用同一台设备连续过几百次验证码——即使每轮换出口
IP，设备侧仍然是同一条曲线。

本模块把"一套写死的常量"换成"每轮抽一套画像"，并且是**自洽的**画像。

## 自洽是硬约束，不是锦上添花

随机化本身会制造新的破绽：一台 UA 报 Windows、屏幕却是 430×932 竖屏、GPU 又是
Apple M3 Max 的机器，比任何固定指纹都可疑。因此这里不逐字段独立抽样，而是先抽
一个**设备家族**（按芯片平台划分的移动 Chromium），再在该家族内部抽取彼此相容的
取值：

```text
family = android-adreno
  ├── UA          Linux; Android 14; SM-S9210
  ├── model       SM-S9210              ← 与 UA 里的机型是同一个
  ├── GPU         Adreno (TM) 750       ← 骁龙机型不会配 Mali
  ├── screen      412×915  dpr 2.625    ← 不会出现 2560×1440
  ├── plugins     []                    ← 移动 Chrome 没有 PDF 插件
  └── Canvas      由本轮种子派生         ← 轮内稳定、轮间不同
```

## 一轮只有一套画像

同一轮里，Python 发出的 HTTP 头、FeiLin 采集所见的环境、动态 PE 所见的环境必须
是同一套。任何一处不同步都会让服务端同时看到两台设备。因此画像由入口层生成一次，
再显式传给协议客户端与两个 Node 桥。
"""

from __future__ import annotations

import base64
import json
import secrets
from dataclasses import dataclass, field
from random import Random
from typing import Any


# ==========================================================================
# 家族定义
#
# 每个家族是一组**已知彼此相容**的取值域。新增家族时，务必把 UA、platform、
# 屏幕、触点、插件、GPU 一并核对过再加进来，不要只补一个 UA 模板。
#
# ⚠️ 为什么只有移动家族：动态 PE 的轨迹回放走的是 touch 事件。桌面画像
# （maxTouchPoints 0、非 Mobile UA）会让 PE 注册鼠标处理器，回放的 touch 事件
# 无人接收，整轮直接失败。想引入桌面家族，得先给 PE 桥补一条 mouse 回放路径。
# ==========================================================================


@dataclass(frozen=True, slots=True)
class _Family:
    """一个设备家族内部彼此相容的取值域。"""

    name: str
    models: tuple[str, ...]
    """UA 与 UA-CH 共用的机型标识。"""

    gpus: tuple[tuple[str, str], ...]
    """与机型同芯片平台的 GPU；骁龙配 Adreno、天玑/Tensor 配 Mali。"""

    screens: tuple[tuple[int, int, float], ...]
    """``(CSS 宽, CSS 高, devicePixelRatio)``——三者必须来自同一台真机。"""

    concurrency: tuple[int, ...]
    memory: tuple[int, ...]
    max_texture_sizes: tuple[int, ...]


_UA_TEMPLATE = (
    "Mozilla/5.0 (Linux; Android {os_major}; {model}) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/{chrome}.0.0.0 "
    "Mobile Safari/537.36"
)

_ANDROID_PLATFORM = "Linux armv8l"
_ANDROID_VERSIONS = ("13.0.0", "14.0.0", "15.0.0")

_ANDROID_SCREENS = (
    (393, 852, 2.75),
    (412, 915, 2.625),
    (360, 800, 3.0),
    (384, 854, 2.8125),
    (412, 892, 3.5),
    (360, 780, 3.0),
    (393, 873, 2.75),
    (411, 914, 2.625),
)

_QUALCOMM = _Family(
    name="android-adreno",
    models=(
        "SM-S9110",
        "SM-S9210",
        "SM-G9980",
        "2211133C",
        "23013RK75C",
        "PJD110",
        "2304FPN6DC",
    ),
    gpus=(
        ("Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 730, OpenGL ES 3.2)"),
        ("Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 740, OpenGL ES 3.2)"),
        ("Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 750, OpenGL ES 3.2)"),
    ),
    screens=_ANDROID_SCREENS,
    concurrency=(8, 8, 6),
    memory=(8, 8, 4),
    max_texture_sizes=(16384,),
)

_MALI = _Family(
    name="android-mali",
    models=(
        "Pixel 7",
        "Pixel 8",
        "Pixel 7a",
        "V2309A",
        "22041216C",
        "CPH2531",
    ),
    gpus=(
        ("Google Inc. (ARM)", "ANGLE (ARM, Mali-G610 MC6, OpenGL ES 3.2)"),
        ("Google Inc. (ARM)", "ANGLE (ARM, Mali-G710 MC10, OpenGL ES 3.2)"),
        (
            "Google Inc. (ARM)",
            "ANGLE (ARM, Mali-G715-Immortalis MC11, OpenGL ES 3.2)",
        ),
    ),
    screens=_ANDROID_SCREENS,
    concurrency=(8, 8, 6),
    memory=(8, 8, 4),
    max_texture_sizes=(8192, 16384),
)

_FAMILIES = (_QUALCOMM, _MALI)

_BROWSER_CHROME_HEIGHT = (112, 190)
"""移动 Chrome 地址栏与系统栏占用的高度区间（CSS 像素）。"""

_CHROME_BUILDS = (
    ("148", "148.0.7647.212"),
    ("149", "149.0.7742.96"),
    ("150", "150.0.7871.187"),
    ("151", "151.0.7955.63"),
)
"""在网 Chromium 大版本及其完整版本号。"""

_GREASE_BRANDS = (
    "Not;A=Brand",
    "Not)A;Brand",
    "Not.A/Brand",
    "Not/A)Brand",
    "Not_A Brand",
    "Not-A.Brand",
    "Not A(Brand",
    "Not?A_Brand",
)
"""Chrome 用来防止 UA-CH 被硬编码解析的 GREASE 品牌名。"""

_GREASE_VERSIONS = ("8", "24", "99")

_LANGUAGE_SETS = (
    ("zh-CN", "zh"),
    ("zh-CN", "zh", "en-US", "en"),
    ("zh-CN", "en-US", "en", "zh"),
)


# ==========================================================================
# 画像
# ==========================================================================


@dataclass(frozen=True, slots=True)
class ScreenProfile:
    """屏幕与视口。取值来自同一台真机，不可逐字段替换。"""

    width: int
    height: int
    avail_width: int
    avail_height: int
    avail_left: int
    avail_top: int
    color_depth: int
    orientation_type: str
    orientation_angle: int
    device_pixel_ratio: float
    inner_width: int
    inner_height: int
    outer_width: int
    outer_height: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "width": self.width,
            "height": self.height,
            "availWidth": self.avail_width,
            "availHeight": self.avail_height,
            "availLeft": self.avail_left,
            "availTop": self.avail_top,
            "colorDepth": self.color_depth,
            "orientationType": self.orientation_type,
            "orientationAngle": self.orientation_angle,
            "devicePixelRatio": self.device_pixel_ratio,
            "innerWidth": self.inner_width,
            "innerHeight": self.inner_height,
            "outerWidth": self.outer_width,
            "outerHeight": self.outer_height,
        }


@dataclass(frozen=True, slots=True)
class GpuProfile:
    """WEBGL_debug_renderer_info 暴露的真实 GPU 标识。"""

    unmasked_vendor: str
    unmasked_renderer: str
    max_texture_size: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "unmaskedVendor": self.unmasked_vendor,
            "unmaskedRenderer": self.unmasked_renderer,
            "maxTextureSize": self.max_texture_size,
        }


@dataclass(frozen=True, slots=True)
class DeviceProfile:
    """一轮挑战内共享的完整设备画像。

    ``profile_id`` 只用于日志与预热池分桶，不参与任何指纹字节。
    """

    profile_id: str
    family: str
    user_agent: str = field(repr=False)
    app_version: str = field(repr=False)
    platform: str
    vendor: str = field(repr=False)
    mobile: bool
    pdf_viewer: bool = field(repr=False)
    ua_brands: tuple[tuple[str, str], ...] = field(repr=False)
    ua_full_versions: tuple[tuple[str, str], ...] = field(repr=False)
    ua_platform: str = field(repr=False)
    ua_platform_version: str = field(repr=False)
    ua_model: str = field(repr=False)
    ua_full_version: str = field(repr=False)
    ua_architecture: str = field(repr=False)
    ua_bitness: str = field(repr=False)
    language: str = field(repr=False)
    languages: tuple[str, ...] = field(repr=False)
    screen: ScreenProfile = field(repr=False)
    gpu: GpuProfile = field(repr=False)
    hardware_concurrency: int = field(repr=False)
    device_memory: int = field(repr=False)
    max_touch_points: int = field(repr=False)
    canvas_seed: str = field(repr=False)
    """Node 桥据此确定性地派生 Canvas 读回值与字体度量。"""

    text_metric_scale: float = field(repr=False)

    # -- 派生的 HTTP 头字段 ------------------------------------------------

    @property
    def sec_ch_ua(self) -> str:
        return ", ".join(
            f'"{brand}";v="{version}"' for brand, version in self.ua_brands
        )

    @property
    def sec_ch_ua_mobile(self) -> str:
        return "?1" if self.mobile else "?0"

    @property
    def sec_ch_ua_platform(self) -> str:
        return f'"{self.ua_platform}"'

    @property
    def accept_language(self) -> str:
        """按 Chrome 的写法给次要语言递减的 q 值。"""

        parts = [self.languages[0]]
        quality = 10
        for language in self.languages[1:]:
            quality -= 1
            parts.append(f"{language};q=0.{quality}")
        return ",".join(parts)

    # -- 传给 Node 桥 ------------------------------------------------------

    def as_bridge_dict(self) -> dict[str, Any]:
        """序列化成两个 Node 桥共同消费的 JSON。"""

        return {
            "profileId": self.profile_id,
            "family": self.family,
            "userAgent": self.user_agent,
            "appVersion": self.app_version,
            "platform": self.platform,
            "vendor": self.vendor,
            "mobile": self.mobile,
            "pdfViewer": self.pdf_viewer,
            "secChUa": self.sec_ch_ua,
            "secChUaMobile": self.sec_ch_ua_mobile,
            "secChUaPlatform": self.sec_ch_ua_platform,
            "acceptLanguage": self.accept_language,
            "brands": [
                {"brand": brand, "version": version}
                for brand, version in self.ua_brands
            ],
            "fullVersionList": [
                {"brand": brand, "version": version}
                for brand, version in self.ua_full_versions
            ],
            "uaPlatform": self.ua_platform,
            "uaPlatformVersion": self.ua_platform_version,
            "uaModel": self.ua_model,
            "uaFullVersion": self.ua_full_version,
            "uaArchitecture": self.ua_architecture,
            "uaBitness": self.ua_bitness,
            "language": self.language,
            "languages": list(self.languages),
            "screen": self.screen.as_dict(),
            "gpu": self.gpu.as_dict(),
            "hardwareConcurrency": self.hardware_concurrency,
            "deviceMemory": self.device_memory,
            "maxTouchPoints": self.max_touch_points,
            "canvasSeed": self.canvas_seed,
            "textMetricScale": self.text_metric_scale,
        }


# ==========================================================================
# 生成
# ==========================================================================


def _build_brands(
    rng: Random,
    *,
    chrome_major: str,
    chrome_full: str,
) -> tuple[tuple[tuple[str, str], ...], tuple[tuple[str, str], ...]]:
    """构造 UA-CH 的 brands 与 fullVersionList。

    GREASE 品牌的**取值和位置**都由 Chrome 随机化，这里照做——把它钉死等于自曝
    一个恒定字段。
    """

    grease_brand = rng.choice(_GREASE_BRANDS)
    grease_version = rng.choice(_GREASE_VERSIONS)
    entries = [
        (grease_brand, grease_version, f"{grease_version}.0.0.0"),
        ("Chromium", chrome_major, chrome_full),
        ("Google Chrome", chrome_major, chrome_full),
    ]
    rng.shuffle(entries)
    brands = tuple((brand, short) for brand, short, _ in entries)
    full_versions = tuple((brand, full) for brand, _, full in entries)
    return brands, full_versions


def _build_screen(rng: Random, family: _Family) -> ScreenProfile:
    """抽一块屏幕，并推导出可用区与浏览器视口尺寸。

    移动 Chrome 铺满整屏，只有地址栏与系统栏占高，因此 ``outer`` 等于屏幕本身，
    ``inner`` 是扣掉浏览器 UI 之后的那块。
    """

    width, height, pixel_ratio = rng.choice(family.screens)
    chrome_height = rng.randint(*_BROWSER_CHROME_HEIGHT)

    return ScreenProfile(
        width=width,
        height=height,
        avail_width=width,
        avail_height=height,
        avail_left=0,
        avail_top=0,
        color_depth=24,
        orientation_type="portrait-primary",
        orientation_angle=0,
        device_pixel_ratio=pixel_ratio,
        inner_width=width,
        inner_height=max(400, height - chrome_height),
        outer_width=width,
        outer_height=height,
    )


def generate_device_profile(rng: Random | None = None) -> DeviceProfile:
    """抽取一套自洽的设备画像。

    默认使用 :class:`secrets.SystemRandom`；传入固定种子的 :class:`Random` 可以
    复现同一套画像，用于测试与问题复盘。
    """

    generator = rng if rng is not None else secrets.SystemRandom()
    family = generator.choice(_FAMILIES)
    chrome_major, chrome_full = generator.choice(_CHROME_BUILDS)
    platform_version = generator.choice(_ANDROID_VERSIONS)
    model = generator.choice(family.models)
    user_agent = _UA_TEMPLATE.format(
        chrome=chrome_major,
        model=model,
        os_major=platform_version.split(".", 1)[0],
    )
    brands, full_versions = _build_brands(
        generator, chrome_major=chrome_major, chrome_full=chrome_full
    )
    unmasked_vendor, unmasked_renderer = generator.choice(family.gpus)
    languages = generator.choice(_LANGUAGE_SETS)

    return DeviceProfile(
        profile_id=secrets.token_hex(8),
        family=family.name,
        user_agent=user_agent,
        # Chrome 的 appVersion 恒等于去掉 "Mozilla/" 前缀的 UA。
        app_version=user_agent.removeprefix("Mozilla/"),
        platform=_ANDROID_PLATFORM,
        vendor="Google Inc.",
        mobile=True,
        # 移动版 Chrome 没有内置 PDF 插件。
        pdf_viewer=False,
        ua_brands=brands,
        ua_full_versions=full_versions,
        ua_platform="Android",
        ua_platform_version=platform_version,
        ua_model=model,
        ua_full_version=chrome_full,
        ua_architecture="",
        ua_bitness="64",
        language=languages[0],
        languages=languages,
        screen=_build_screen(generator, family),
        gpu=GpuProfile(
            unmasked_vendor=unmasked_vendor,
            unmasked_renderer=unmasked_renderer,
            max_texture_size=generator.choice(family.max_texture_sizes),
        ),
        hardware_concurrency=generator.choice(family.concurrency),
        # navigator.deviceMemory 被 Chrome 量化后最大只有 8，报 16/32 是穿帮。
        device_memory=generator.choice(family.memory),
        max_touch_points=5,
        canvas_seed=secrets.token_hex(16),
        text_metric_scale=round(generator.uniform(5.62, 6.48), 4),
    )


def encode_device_profile(profile: DeviceProfile) -> str:
    """把画像编码成 Node 桥 ``--device-profile`` 的实参。

    走 base64 是为了让 argv 里不出现引号、空格与非 ASCII——机型名与 UA 里两者都
    有，直接拼进命令行迟早会踩到某个平台的转义差异。
    """

    payload = json.dumps(
        profile.as_bridge_dict(),
        ensure_ascii=False,
        separators=(",", ":"),
    )
    return base64.b64encode(payload.encode("utf-8")).decode("ascii")


__all__ = [
    "DeviceProfile",
    "GpuProfile",
    "ScreenProfile",
    "encode_device_profile",
    "generate_device_profile",
]
