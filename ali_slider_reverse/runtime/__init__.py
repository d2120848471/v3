"""运行时桥接层：把重环境依赖隔离到独立进程。

```text
node_device.py  FeiLin 设备链，跨 Init 保持同一个 Node VM
node_pe.py      本轮动态 PE 的隔离 VM，原生生成 Verify data
vision.py       OpenCV 缺口求解子进程
bridges/*.mjs   Node 侧脚本
```

三个桥的共同约定：**子进程不发网络请求**。所有出网动作都由 Python 完成，桥只做
本地计算，这样代理设置、单次 Verify 边界等控制流才能在一个地方审计。
"""

from __future__ import annotations

from .node_device import (
    DeviceRuntimeClient,
    DeviceRuntimeResult,
    DeviceRuntimeSession,
)
from .node_pe import PeRuntimeClient, PeRuntimeResult
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
