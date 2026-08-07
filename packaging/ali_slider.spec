# -*- mode: python ; coding: utf-8 -*-
"""PyInstaller 打包描述：把协议实现与 OpenCV 收进一个免安装分发包。

目标机器上不装 Python、不装 OpenCV 也能运行：

```text
Python 解释器 + 标准库     PyInstaller 自带
requests / cryptography    协议层依赖
opencv-python-headless     图像识别；headless 版没有 GUI 依赖，体积小得多
```

## 为什么默认是 onedir 而不是单文件

单文件模式每次启动都要把完整 Python、OpenCV 与 NumPy 运行目录解压到临时目录，
冷启动更慢，而且退出后可能残留目录。onedir 解压一次就是文件夹本体，双击即启动。
真要单文件就设环境变量 ``ALI_SLIDER_ONEFILE=1``。

## 环境变量

```text
ALI_SLIDER_ONEFILE   置为 1 时打成单文件
```
"""

import os
from pathlib import Path

from PyInstaller.utils.hooks import collect_all

SPEC_DIR = Path(SPECPATH).resolve()  # noqa: F821 - PyInstaller 注入的全局量。
PROJECT_ROOT = SPEC_DIR.parent
PACKAGE_DIR = PROJECT_ROOT / "ali_slider_reverse"

ONEFILE = os.environ.get("ALI_SLIDER_ONEFILE") == "1"

# --------------------------------------------------------------------------
# 随包资产
#
# 默认轨迹是运行必需的，且要落在与源码布局一致的相对位置；冻结分发中的
# config.py 会从解包目录内定位它。
# --------------------------------------------------------------------------

datas = [
    (
        str(PACKAGE_DIR / "challenge" / "default_touch_track.json"),
        "ali_slider_reverse/challenge",
    ),
]

binaries = []
hiddenimports = [
    # 都是被惰性 import 的，静态分析能扫到，这里显式列出以防将来改成动态导入。
    "ali_slider_reverse.vision.gap_solver",
    "ali_slider_reverse.entrypoints.vision_worker",
]

# cv2 的 .pyd 与配置文件散落在包内多处，交给官方收集器最稳妥。
_cv2_datas, _cv2_binaries, _cv2_hidden = collect_all("cv2")
datas += _cv2_datas
binaries += _cv2_binaries
hiddenimports += _cv2_hidden


# --------------------------------------------------------------------------
# 排除项：这些库要么根本用不到，要么会把包撑大一倍
# --------------------------------------------------------------------------

excludes = [
    "tkinter",
    "matplotlib",
    "scipy",
    "pandas",
    "PIL",
    "PyQt5",
    "PyQt6",
    "PySide2",
    "PySide6",
    "IPython",
    "jupyter",
    "notebook",
    "pytest",
    "sphinx",
]


analysis = Analysis(  # noqa: F821
    [str(SPEC_DIR / "entry.py")],
    pathex=[str(PROJECT_ROOT)],
    binaries=binaries,
    datas=datas,
    hiddenimports=hiddenimports,
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=excludes,
    noarchive=False,
    optimize=0,
)

pyz = PYZ(analysis.pure)  # noqa: F821

# UPX 会压坏 numpy/cv2 的扩展模块，且对本包的启动速度是净负收益，一律关掉。
_EXE_COMMON = dict(
    name="AliSlider",
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
)

if ONEFILE:
    exe = EXE(  # noqa: F821
        pyz,
        analysis.scripts,
        analysis.binaries,
        analysis.datas,
        [],
        runtime_tmpdir=None,
        **_EXE_COMMON,
    )
else:
    exe = EXE(  # noqa: F821
        pyz,
        analysis.scripts,
        [],
        exclude_binaries=True,
        **_EXE_COMMON,
    )
    coll = COLLECT(  # noqa: F821
        exe,
        analysis.binaries,
        analysis.datas,
        strip=False,
        upx=False,
        name="AliSlider",
    )
