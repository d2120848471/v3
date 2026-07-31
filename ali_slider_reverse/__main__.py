"""``python -m ali_slider_reverse`` 的入口，等价于 ``entrypoints.cli``。"""

from __future__ import annotations

from .entrypoints.cli import main

if __name__ == "__main__":
    raise SystemExit(main())
