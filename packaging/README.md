# 打包为 Windows 免安装分发包

产出一个解压即用的文件夹：目标机器不装 Python、不装 OpenCV，双击
`AliSlider.exe` 就能跑。

```text
packaging/
├── ali_slider.spec       PyInstaller 打包描述
├── entry.py              冻结分发的入口脚本
├── build_windows.ps1     一条命令完成构建
├── build_windows.bat     双击即构建（绕过 PowerShell 执行策略）
└── 使用说明.txt          随分发包一起发给使用者
```


## 1. 先说清楚一件事：不能交叉编译

PyInstaller 把**当前平台**的解释器和扩展模块打进产物，没有交叉编译能力。
在 macOS 或 Linux 上跑它只会得到 macOS/Linux 的可执行文件。

所以想要 `.exe`，只有两条路：

```text
A  在 Windows 机器（或虚拟机）上执行 packaging\build_windows.ps1
B  用 .github/workflows/build-windows.yml 在 GitHub 的 Windows runner 上构建
```


## 2. 路线 A：在 Windows 上构建

前置只有一个：Python 3.10 以上，安装时勾选「Add python.exe to PATH」。

```powershell
git clone <本仓库> ; cd <仓库目录>
powershell -ExecutionPolicy Bypass -File packaging\build_windows.ps1
```

或者直接双击 `packaging\build_windows.bat`。

脚本会依次完成：建独立构建虚拟环境、安装依赖、调用 PyInstaller、把使用说明
拷进产物、自检可执行文件、压缩。

产物：

```text
dist\AliSlider\AliSlider.exe     可执行文件
dist\AliSlider-win64.zip         发给别人的压缩包
```

常用开关：

```powershell
# 打成单个 exe（不推荐，见下文）
-OneFile

# 只要目录，不压缩
-SkipZip
```


## 3. 路线 B：用 GitHub Actions 构建

推送到 GitHub 后，在 Actions 页面手动运行 **build-windows**，或推一个 `v*`
标签自动触发。跑完从该次运行的 Artifacts 里下载 `AliSlider-win64.zip`。

这条路适合手边只有 macOS 的情况——它用的就是路线 A 的同一个脚本。


## 4. 包里装了什么

```text
AliSlider.exe            入口，双击进入快速 / 标准 / 自定义启动菜单
_internal\               PyInstaller 运行时目录
  ├── python3xx.dll      Python 解释器
  ├── cv2\               OpenCV（headless 版）
  ├── numpy\             NumPy
  └── cryptography\      AES/HMAC
使用说明.txt
```

体积以实际构建产物为准，主要来自 OpenCV、NumPy 和 Python 运行时。


## 5. 冻结分发下的行为差异

打包后图像识别行为与源码运行不同，自动生效，无需配置：

**图像识别在进程内跑。** 源码运行时 OpenCV 在独立解释器里（主环境不必装
OpenCV）；打包后 OpenCV 已经在同一个可执行文件里，再去 spawn 一个「装了
OpenCV 的 Python」既找不到也没必要，因此 `--vision-python` 默认取哨兵值
`<in-process>`，预热改在后台线程完成。

> 冻结分发下不要再传 `--vision-python C:\...\python.exe`：外部解释器无法
> 从打包归档里 import 本项目，那条路径只对源码运行有效。


## 6. 为什么默认不是单文件

单文件模式每次启动都要把完整 Python、OpenCV 与 NumPy 运行目录解压到临时目录，
冷启动更慢，退出后还可能残留目录。onedir 解压一次就是文件夹本体，双击即启动。

确实需要单文件分发时加 `-OneFile`，代价自负。


## 7. 常见问题

**杀毒软件报毒。** PyInstaller 的自解压结构长期被各家引擎误报。加白名单，
或让对方自己用本脚本从源码构建。

**产物在别的机器上起不来。** 先确认对方是 64 位 Windows 10 或更高版本。
PyInstaller 产物不支持比构建机更老的 Windows，必要时在更老的系统上构建。

**中文乱码。** 程序启动时会把控制台切到 UTF-8；老版 conhost 仍可能出问题，
用 Windows Terminal 打开即可。
