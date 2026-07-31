"""统一的异常层级。

重构前每个模块各自定义根异常（``RuntimeError`` 或 ``ValueError``），调用方
必须写出 ``except (AliSliderError, DeviceRuntimeError, PeRuntimeError, ValueError)``
这样的长元组才能兜住一轮挑战的全部失败路径。这里把所有协议侧异常收敛到
:class:`AliSliderError` 之下，入口层捕获根类型即可，同时保留细分类型供需要
区分失败阶段的调用方使用。

层级设计：

```text
AliSliderError                  协议闭环失败的根类型
├── ProtocolError               纯算法层：编解码、schema、签名
│   └── DataCodecError          data 层的格式/schema 不符
├── RuntimeBridgeError          子进程/VM 桥的根类型
│   ├── DeviceRuntimeError      FeiLin 设备桥无法安全产生 token
│   ├── PeRuntimeError          动态 PE 桥无法生成或校验 data
│   └── VisionError             OpenCV 缺口求解不可用或失败
└── ApiRequestError             HTTP 接口入参不合法（不消耗挑战）
```

参数校验一律仍用内置 ``ValueError``/``TypeError``：那是调用方的编程错误，
不属于"协议没走通"，不应与挑战失败混在同一个 except 分支里。
"""

from __future__ import annotations


class AliSliderError(RuntimeError):
    """协议状态、响应或本地运行时不满足闭环要求。

    入口层（CLI/HTTP）捕获此类型即可覆盖全部挑战失败路径。
    """


class ProtocolError(AliSliderError):
    """纯算法层的编解码、schema 或签名不符合当前协议版本。"""


class DataCodecError(ProtocolError):
    """``CaptchaVerifyParam.data`` 的格式、编码或 schema 不符合当前协议。"""


class RuntimeBridgeError(AliSliderError):
    """Node/Python 子进程桥无法给出可信结果。"""


class DeviceRuntimeError(RuntimeBridgeError):
    """设备运行桥无法安全、完整地产生 DeviceToken。"""


class PeRuntimeError(RuntimeBridgeError):
    """动态 PE 桥无法生成或验证当前分片的 data。"""


class VisionError(RuntimeBridgeError):
    """OpenCV 缺口求解进程不可用，或返回了无法采信的结果。"""


class ApiRequestError(AliSliderError):
    """HTTP 接口入参不合法；对应 400，不会消耗任何挑战。"""


__all__ = [
    "AliSliderError",
    "ApiRequestError",
    "DataCodecError",
    "DeviceRuntimeError",
    "PeRuntimeError",
    "ProtocolError",
    "RuntimeBridgeError",
    "VisionError",
]
