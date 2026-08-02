"""阿里 V3 滑块纯协议复现工具包。

> 仅用于获得明确授权的本地 CTF、兼容性研究与协议验证环境。禁止用于批量解题、
> 并发挑战、同一挑战重试、规避第三方访问控制或任何未授权目标。

## 分层

```text
config / errors      集中式常量与统一异常层级
protocol/            纯算法：签名、参数封装、token 容器、data 解包、密文恢复
vision/              坐标运动式与 OpenCV 缺口定位
runtime/             Node 与 OpenCV 子进程桥，重环境依赖都隔离在这一层
challenge/           编排：Init → 资源 → 识别 → PE → Verify，持有全部网络出口
entrypoints/         CLI、HTTP 接口与视觉 worker
```

依赖方向严格单向：``entrypoints → challenge → runtime → {protocol, vision}``。

## 快速开始

```python
from ali_slider_reverse import AliSliderClient
from ali_slider_reverse.device_profile import generate_device_profile
from ali_slider_reverse.entrypoints.options import RuntimeSettings

settings = RuntimeSettings(vision_python="/path/to/vision-python")
# 一轮一套设备画像，协议客户端与设备运行时必须共用同一个对象。
profile = generate_device_profile()
client = settings.build_client(device_profile=profile)
try:
    client.prewarm_vision()
    outcome = client.run_captcha(
        settings.build_device_runtime(device_profile=profile)
    )
    print(outcome.verify.verify_code, outcome.verify.succeeded)
finally:
    client.close()
```

命令行等价物见 ``python -m ali_slider_reverse --help``。
"""

from __future__ import annotations

from . import config
from .challenge.assets import ChallengeAssets
from .challenge.business import (
    BusinessRequestTemplate,
    BusinessResponse,
    load_business_template_from_capture,
)
from .challenge.session import (
    AliSliderClient,
    CaptchaChallenge,
    CaptchaVerifyResult,
    ChallengeOutcome,
    VerifyBuild,
    normalize_proxies,
)
from .errors import (
    AliSliderError,
    ApiRequestError,
    DataCodecError,
    DeviceRuntimeError,
    PeRuntimeError,
    ProtocolError,
    RuntimeBridgeError,
    VisionError,
)
from .runtime.node_device import (
    DeviceRuntimeClient,
    DeviceRuntimeResult,
    DeviceRuntimeSession,
)
from .runtime.vision import VisionResult

__version__ = "2.0.0"

__all__ = [
    # 编排入口
    "AliSliderClient",
    "ChallengeOutcome",
    "DeviceRuntimeClient",
    # 结果模型
    "CaptchaChallenge",
    "CaptchaVerifyResult",
    "ChallengeAssets",
    "DeviceRuntimeResult",
    "DeviceRuntimeSession",
    "VerifyBuild",
    "VisionResult",
    # 可选业务提交
    "BusinessRequestTemplate",
    "BusinessResponse",
    "load_business_template_from_capture",
    # 异常
    "AliSliderError",
    "ApiRequestError",
    "DataCodecError",
    "DeviceRuntimeError",
    "PeRuntimeError",
    "ProtocolError",
    "RuntimeBridgeError",
    "VisionError",
    # 工具
    "config",
    "normalize_proxies",
    "__version__",
]
