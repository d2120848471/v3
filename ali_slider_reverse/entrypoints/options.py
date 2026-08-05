"""CLI 与 HTTP 接口共享的参数定义与对象装配。

两个入口需要的运行环境完全一致——Node 可执行文件、视觉解释器、超时、两个正常化
区间。这里集中定义参数、默认值与装配逻辑，保证改一处即可，不会出现 CLI 与接口
默认值悄悄漂移的情况。

:class:`RuntimeSettings` 承载"启动时固定、与单轮无关"的配置；逐轮变化的
``SceneId``、``proxy``、``AaduaneId`` 由调用方在装配时传入。
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass

from .. import config
from ..challenge.device_pool import MAX_VMS_PER_NODE
from ..challenge.session import AliSliderClient
from ..device_profile import DeviceProfile
from ..runtime.node_device import DeviceRuntimeClient


FAST_MODE_DEFAULT_CONCURRENCY = 5
"""快速模式未显式指定并发数时的有界容量。"""

FAST_MODE_DEFAULT_VMS_PER_NODE = 3
"""快速模式一个 Node host 默认承载的隔离 VM 数。"""


def add_runtime_arguments(parser: argparse.ArgumentParser) -> None:
    """添加两个入口共享的运行环境参数。"""

    parser.add_argument(
        "--node",
        default=config.DEFAULT_NODE_BINARY,
        help=f"Node 可执行文件（默认 {config.DEFAULT_NODE_BINARY}）",
    )
    parser.add_argument(
        "--vision-python",
        default=config.DEFAULT_VISION_PYTHON,
        help=(
            "带 OpenCV/NumPy 的 Python"
            f"（默认 {config.DEFAULT_VISION_PYTHON}）"
        ),
    )
    parser.add_argument(
        "--sdk-js",
        help="可选：本地公开 AliyunCaptcha.js；省略则从官方 CDN 下载",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=config.DEFAULT_TIMEOUT,
        help=f"单步网络/子进程超时秒数（默认 {config.DEFAULT_TIMEOUT:g}）",
    )
    parser.add_argument(
        "--gather-cost-min",
        type=int,
        default=config.DEFAULT_GATHER_COST_RANGE[0],
        help=(
            "Node 过快时 GatherCost 正常化下界"
            f"（默认 {config.DEFAULT_GATHER_COST_RANGE[0]}）"
        ),
    )
    parser.add_argument(
        "--gather-cost-max",
        type=int,
        default=config.DEFAULT_GATHER_COST_RANGE[1],
        help=(
            "Node 过快时 GatherCost 正常化上界"
            f"（默认 {config.DEFAULT_GATHER_COST_RANGE[1]}）"
        ),
    )
    parser.add_argument(
        "--first-touch-age-min",
        type=int,
        default=config.DEFAULT_FIRST_TOUCH_AGE_RANGE[0],
        help=(
            "首 touch 相对 TrackStartTime 的下界毫秒"
            f"（默认 {config.DEFAULT_FIRST_TOUCH_AGE_RANGE[0]}）"
        ),
    )
    parser.add_argument(
        "--first-touch-age-max",
        type=int,
        default=config.DEFAULT_FIRST_TOUCH_AGE_RANGE[1],
        help=(
            "首 touch 相对 TrackStartTime 的上界毫秒"
            f"（默认 {config.DEFAULT_FIRST_TOUCH_AGE_RANGE[1]}）"
        ),
    )


def add_confidence_argument(parser: argparse.ArgumentParser) -> None:
    """添加图像置信度下限参数。"""

    parser.add_argument(
        "--min-confidence",
        type=float,
        default=config.DEFAULT_MIN_CONFIDENCE,
        help=(
            "低于此图像置信度时停止且不发 Verify"
            f"（默认 {config.DEFAULT_MIN_CONFIDENCE}）"
        ),
    )


def add_server_arguments(parser: argparse.ArgumentParser) -> None:
    """添加 HTTP 服务端参数；纯接口入口与桌面窗口入口共用。"""

    parser.add_argument("--host", default=config.API_HOST, help="监听地址")
    parser.add_argument(
        "--port",
        type=int,
        default=config.API_PORT,
        help=f"监听端口（默认 {config.API_PORT}）",
    )
    parser.add_argument(
        "--max-concurrency",
        type=int,
        default=0,
        help="同时挑战数上限（0：标准模式不限，快速模式使用 5）",
    )
    parser.add_argument(
        "--runtime-mode",
        choices=("standard", "fast"),
        default="standard",
        help=(
            "服务运行模式：standard 逐轮画像且不投机预热；"
            "fast 固定本进程画像并按并发容量预热设备会话"
        ),
    )
    parser.add_argument(
        "--prewarm-device-session",
        action="store_true",
        help=(
            "兼容旧启动命令：等同 --runtime-mode fast；固定本进程画像并按并发"
            "容量预热 FeiLin 会话"
        ),
    )
    parser.add_argument(
        "--vms-per-node",
        type=int,
        default=FAST_MODE_DEFAULT_VMS_PER_NODE,
        help=(
            "快速模式每个 Node host 的隔离 VM 槽位数"
            f"（1..{MAX_VMS_PER_NODE}，默认 {FAST_MODE_DEFAULT_VMS_PER_NODE}）"
        ),
    )


def fast_mode_enabled(args: argparse.Namespace) -> bool:
    """兼容新模式名与旧预热开关，返回是否启用快速模式。"""

    return bool(
        getattr(args, "runtime_mode", "standard") == "fast"
        or getattr(args, "prewarm_device_session", False)
    )


def effective_server_concurrency(args: argparse.Namespace) -> int:
    """返回真正执行的并发上限；0 只在标准模式表示不限。"""

    configured = int(args.max_concurrency)
    if configured > 0:
        return configured
    return FAST_MODE_DEFAULT_CONCURRENCY if fast_mode_enabled(args) else 0


@dataclass(frozen=True, slots=True)
class RuntimeSettings:
    """启动时固定的运行环境，与单次挑战无关。"""

    node_binary: str = config.DEFAULT_NODE_BINARY
    vision_python: str = config.DEFAULT_VISION_PYTHON
    sdk_js: str | None = None
    timeout: float = config.DEFAULT_TIMEOUT
    minimum_confidence: float = config.DEFAULT_MIN_CONFIDENCE
    gather_cost_range: tuple[int, int] = config.DEFAULT_GATHER_COST_RANGE
    first_touch_age_range: tuple[int, int] = config.DEFAULT_FIRST_TOUCH_AGE_RANGE

    @classmethod
    def from_args(cls, args: argparse.Namespace) -> "RuntimeSettings":
        """从已解析的命令行参数构造。

        ``--min-confidence`` 只在部分子命令上存在，缺失时回落到默认值。
        """

        return cls(
            node_binary=args.node,
            vision_python=args.vision_python,
            sdk_js=args.sdk_js,
            timeout=args.timeout,
            minimum_confidence=getattr(
                args, "min_confidence", config.DEFAULT_MIN_CONFIDENCE
            ),
            gather_cost_range=(args.gather_cost_min, args.gather_cost_max),
            first_touch_age_range=(
                args.first_touch_age_min,
                args.first_touch_age_max,
            ),
        )

    def build_device_runtime(
        self,
        *,
        device_profile: DeviceProfile,
        prefix: str = config.DEFAULT_PREFIX,
        proxies: dict[str, str] | None = None,
    ) -> DeviceRuntimeClient:
        """装配设备运行时客户端。

        ``device_profile`` 必须与同一轮的 :meth:`build_client` 用同一个对象——
        两者分别决定 FeiLin 指纹与 HTTP 头，不同步就等于一轮里出现两台设备。
        """

        return DeviceRuntimeClient(
            node_binary=self.node_binary,
            sdk_path=self.sdk_js,
            prefix=prefix,
            region=config.DEFAULT_REGION,
            timeout=self.timeout,
            gather_cost_range=self.gather_cost_range,
            first_touch_age_range=self.first_touch_age_range,
            proxies=proxies,
            device_profile=device_profile,
        )

    def build_client(
        self,
        *,
        device_profile: DeviceProfile,
        scene_id: str = config.DEFAULT_SCENE_ID,
        prefix: str = config.DEFAULT_PREFIX,
        proxies: dict[str, str] | None = None,
        rpc_key_id: str | None = None,
        adapter: object | None = None,
        upload_executor: object | None = None,
    ) -> AliSliderClient:
        """装配协议客户端。"""

        return AliSliderClient(
            scene_id=scene_id,
            prefix=prefix,
            node_binary=self.node_binary,
            vision_python=self.vision_python,
            timeout=self.timeout,
            proxies=proxies,
            rpc_key_id=rpc_key_id,
            device_profile=device_profile,
            adapter=adapter,
            upload_executor=upload_executor,
        )


__all__ = [
    "FAST_MODE_DEFAULT_CONCURRENCY",
    "FAST_MODE_DEFAULT_VMS_PER_NODE",
    "RuntimeSettings",
    "add_confidence_argument",
    "add_runtime_arguments",
    "add_server_arguments",
    "effective_server_concurrency",
    "fast_mode_enabled",
]
