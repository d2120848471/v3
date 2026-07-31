"""编排层：把协议、运行时与图像三层组装成一轮完整挑战。

```text
session.py   Init → 资源 → 识别 → PE → Verify 的状态机与单次 Verify 边界
assets.py    四项公开资源的并发下载与 PNG 尺寸解析
track.py     触摸轨迹资产的读取与缩放
business.py  可选的业务请求提交
```

这一层持有全部对外网络出口。下层的三个 Node/Python 桥都不发网络请求，因此代理
设置、请求头画像与"每个 CertifyId 只 Verify 一次"的约束都能在这里集中审计。
"""

from __future__ import annotations

from .assets import ChallengeAssets
from .business import (
    BusinessRequestTemplate,
    BusinessResponse,
    load_business_template_from_capture,
)
from .session import (
    AliSliderClient,
    CaptchaChallenge,
    CaptchaVerifyResult,
    ChallengeOutcome,
    VerifyBuild,
    normalize_proxies,
)

__all__ = [
    "AliSliderClient",
    "BusinessRequestTemplate",
    "BusinessResponse",
    "CaptchaChallenge",
    "CaptchaVerifyResult",
    "ChallengeAssets",
    "ChallengeOutcome",
    "VerifyBuild",
    "load_business_template_from_capture",
    "normalize_proxies",
]
