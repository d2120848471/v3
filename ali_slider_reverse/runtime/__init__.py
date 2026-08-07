"""纯 Python 设备/PE 计算与可选视觉 worker。

```text
device.py       兼容设备链，跨 Init 保持同一个 Python 状态对象
pe.py           TrackList、arg 与 Verify data 的纯 Python 计算
vision.py       OpenCV 缺口求解子进程
```

设备 RPC 与验证码 RPC 的所有出网动作均由 Python 完成；视觉 worker 只读本地图片。
"""

from __future__ import annotations

from .device import (
    DeviceRuntimeClient,
    DeviceRuntimeResult,
    DeviceRuntimeSession,
)
from .pe import PeRuntimeClient, PeRuntimeResult
from .vision import VisionResult, VisionWorker

__all__ = [
    "DeviceRuntimeClient",
    "DeviceRuntimeResult",
    "DeviceRuntimeSession",
    "PeRuntimeClient",
    "PeRuntimeResult",
    "VisionResult",
    "VisionWorker",
]
