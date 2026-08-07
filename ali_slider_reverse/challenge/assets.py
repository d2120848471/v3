"""本轮挑战两张公开图片的并发下载。

Init 返回后只下载背景图与拼图图。PE 和样式表都不参与纯 Python 协议计算，因而
不会被下载或落盘。

两条安全约束：

* 下载地址必须落在 HTTPS 白名单主机上，且资源路径来自服务端返回值，需按相对
  路径严格校验，防止被响应内容诱导去访问任意 URL；
* 每个并发 worker 用自己的短会话。``requests.Session`` 不承诺跨线程共享，而
  公开静态资源也不需要 Init/Verify 的 cookie。

Session 逐 worker 隔离，但底层连接池由整轮共享（见 :mod:`.transport`）。
"""

from __future__ import annotations

import re
import struct
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .. import config
from ..device_profile import DeviceProfile
from ..errors import AliSliderError
from .transport import pooled_session

_ASSET_PATH_RE = re.compile(r"^[A-Za-z0-9._/-]+$")
_PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


@dataclass(frozen=True, slots=True)
class ChallengeAssets:
    """一轮挑战下载到本地的两张 PNG。"""

    background: Path
    shadow: Path

    load_timings: dict[str, tuple[int, int]] = field(repr=False)
    """``img``/``pImg`` 各自的 ``(开始, 结束)`` epoch 毫秒。"""

def require_relative_asset_path(value: Any, *, label: str) -> str:
    """校验服务端返回的资源路径确实是安全的相对路径。

    拒绝绝对路径与 ``..`` 段，避免拼接出白名单之外的地址或写到目录之外。
    """

    if not isinstance(value, str) or not value:
        raise AliSliderError(f"{label} 缺失")
    if (
        not _ASSET_PATH_RE.fullmatch(value)
        or value.startswith("/")
        or ".." in value.split("/")
    ):
        raise AliSliderError(f"{label} 不是安全的相对资源路径")
    return value


def png_dimensions(path: str | Path, *, label: str) -> tuple[int, int]:
    """只读 PNG 的 IHDR 取宽高。

    纯 Python PE 需要真实的图片尺寸来布局，但主协议环境不一定装 Pillow 或 OpenCV。
    IHDR 固定在文件头前 24 字节，直接解析比引入依赖划算得多。
    """

    try:
        with Path(path).open("rb") as stream:
            header = stream.read(24)
    except OSError as exc:
        raise AliSliderError(f"无法读取 {label}") from exc

    if (
        len(header) != 24
        or header[:8] != _PNG_SIGNATURE
        or header[12:16] != b"IHDR"
    ):
        raise AliSliderError(f"{label} 不是有效 PNG")
    width, height = struct.unpack(">II", header[16:24])
    if width <= 0 or height <= 0 or width > 16_384 or height > 16_384:
        raise AliSliderError(f"{label} 尺寸异常")
    return width, height


class AssetDownloader:
    """按本轮挑战的路径并发拉取两张公开图片。"""

    def __init__(
        self,
        *,
        requests_module: Any,
        timeout: float,
        device_profile: DeviceProfile,
        proxies: dict[str, str] | None = None,
        referer: str = config.REFERER,
        image_base: str = config.IMAGE_BASE,
        adapter: Any | None = None,
    ) -> None:
        self._requests = requests_module
        self.timeout = timeout
        self.device_profile = device_profile
        self.proxies = proxies
        self.referer = referer
        self.image_base = image_base
        self._adapter = adapter

    def download(
        self,
        *,
        image_path: str,
        puzzle_image_path: str,
        directory: str | Path,
    ) -> ChallengeAssets:
        """并发下载两张图片并返回本地路径与计时。"""

        target = Path(directory)
        target.mkdir(parents=True, exist_ok=True)

        jobs = {
            "img": (self.image_base + image_path, target / "back.png"),
            "pImg": (self.image_base + puzzle_image_path, target / "shadow.png"),
        }

        # 退出 executor 上下文时会等待全部 worker，成功和异常路径都不会留下
        # 后台写入——否则临时目录回收后仍可能有线程在往里写文件。
        with ThreadPoolExecutor(
            max_workers=len(jobs),
            thread_name_prefix="ali-public-asset",
        ) as executor:
            futures = {
                name: executor.submit(self._fetch, url, destination)
                for name, (url, destination) in jobs.items()
            }

        # 固定必需资源的异常观察顺序，避免调度完成顺序改变最终报出的错误。
        timings = {name: futures[name].result() for name in ("img", "pImg")}

        return ChallengeAssets(
            background=jobs["img"][1],
            shadow=jobs["pImg"][1],
            load_timings=timings,
        )

    def _fetch(self, url: str, destination: Path) -> tuple[int, int]:
        """下载单个资源，返回 ``(开始, 结束)`` epoch 毫秒。"""

        parsed = urlparse(url)
        if (
            parsed.scheme != "https"
            or parsed.hostname not in config.ALLOWED_ASSET_HOSTS
        ):
            raise AliSliderError(f"拒绝下载非白名单资源：{url}")

        started_ms = int(time.time() * 1000)
        try:
            with pooled_session(
                self._requests, self._adapter, proxies=self.proxies
            ) as session:
                response = session.get(
                    url,
                    headers=config.browser_headers(
                        profile=self.device_profile,
                        referer=self.referer,
                        destination="image",
                        mode="no-cors",
                        include_origin=False,
                    ),
                    timeout=self.timeout,
                )
                response.raise_for_status()
                content = response.content
        except Exception as exc:
            raise AliSliderError("公开验证码资源下载失败") from exc

        try:
            destination.write_bytes(content)
        except OSError as exc:
            raise AliSliderError("公开验证码资源落盘失败") from exc
        return started_ms, int(time.time() * 1000)


__all__ = [
    "AssetDownloader",
    "ChallengeAssets",
    "png_dimensions",
    "require_relative_asset_path",
]
