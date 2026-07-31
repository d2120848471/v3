"""程序入口。

```text
cli.py            solve / run 命令行
api.py            标准库 http.server 实现的 HTTP 接口
vision_worker.py  OpenCV 求解子进程（由 runtime.vision 拉起，一般不手动调用）
options.py        两个入口共享的参数定义与对象装配
```

本模块刻意**不导入**任何子模块：``vision_worker`` 会在没有协议依赖的解释器里
运行，导入 CLI/API 会连带拉进 requests 等它不需要的包。
"""

from __future__ import annotations

__all__: list[str] = []
