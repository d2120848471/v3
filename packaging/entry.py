"""PyInstaller 冻结分发的入口脚本。

打包后的 ``AliSlider.exe`` 从这里启动，直接转交给
:func:`ali_slider_reverse.entrypoints.desktop.main` 分发到各模式。

这个文件刻意保持极薄：它是构建产物的一部分，不属于库代码，因此不放任何逻辑。
"""

from __future__ import annotations

import multiprocessing
import sys

from ali_slider_reverse.entrypoints.desktop import main

if __name__ == "__main__":
    # 冻结分发里若有任何库私自起了 multiprocessing，缺了这一行会变成无限递归开
    # 进程。本项目不用它，这里纯粹是兜底。
    multiprocessing.freeze_support()
    sys.exit(main())
