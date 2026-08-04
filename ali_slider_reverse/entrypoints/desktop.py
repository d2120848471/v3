"""冻结分发入口：双击直接启动快速 HTTP API。

冻结分发（``AliSlider.exe``）无参数启动时不再进入交互菜单，而是直接转交给
:mod:`entrypoints.api`：启用 5 并发快速模式、共享 HTTP 连接池和常驻设备会话，
打印并打开本地接口文档。

它同时是打包后可执行文件的**总入口**，按第一个位置参数分发：

```text
（无参数）        快速 HTTP API（5 并发）并打开接口文档
api   [args...]  纯 HTTP 接口，不打印横幅，适合当后台服务跑
solve [args...]  CLI：只 Init/下载/识别，不发送 Verify，不消耗挑战
run   [args...]  CLI：完整一轮，只发送一次 Verify
```

## 与 ``entrypoints.api`` 的分工

协议与服务逻辑一行都不在这里——本模块只负责**呈现**：控制台编码、横幅、示例、
结果渲染、退出前驻留窗口。真正的一轮挑战仍然走 :func:`~.api.solve_once`，通过
:attr:`~.api.SliderApiHandler.result_hook` 把结果回调过来。

## 敏感字段

``securityToken`` 是本轮的敏感结果。窗口默认只显示前后各 8 个字符，需要完整值时
用 ``--show-token``；HTTP 响应体始终是完整的，不受此开关影响。
"""

from __future__ import annotations

import argparse
import importlib
import json
import subprocess
import sys
import threading
import time
import unicodedata
import urllib.error
import urllib.request
from collections.abc import Callable
from http.server import ThreadingHTTPServer
from typing import Any

from .. import __version__, config
from ..challenge.device_pool import DeviceSessionPool
from ..device_profile import generate_device_profile
from .api import SliderApiHandler, _Unlimited
from .api import main as api_main
from .cli import main as cli_main
from .options import (
    RuntimeSettings,
    add_confidence_argument,
    add_runtime_arguments,
    add_server_arguments,
    effective_server_concurrency,
    fast_mode_enabled,
)

# ==========================================================================
# 控制台外观
#
# 双击运行时宿主是 conhost：默认代码页不是 UTF-8，中文会变成乱码；ANSI 颜色也
# 要显式打开虚拟终端处理。三个动作全部 best-effort，失败只是变难看，不影响功能。
# ==========================================================================

_ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
_STD_OUTPUT_HANDLE = -11


class _Palette:
    """ANSI 颜色；不支持时全部退化为空串。"""

    def __init__(self, enabled: bool) -> None:
        def code(value: str) -> str:
            return value if enabled else ""

        self.reset = code("\033[0m")
        self.dim = code("\033[2m")
        self.bold = code("\033[1m")
        self.cyan = code("\033[36m")
        self.green = code("\033[32m")
        self.yellow = code("\033[33m")
        self.red = code("\033[31m")


def _prepare_console(title: str) -> _Palette:
    """把控制台切到 UTF-8 并打开 ANSI 支持，返回可用的调色板。"""

    ansi = sys.stdout.isatty()
    if sys.platform == "win32":
        try:
            import ctypes

            kernel32 = ctypes.windll.kernel32
            kernel32.SetConsoleOutputCP(65001)
            kernel32.SetConsoleCP(65001)
            kernel32.SetConsoleTitleW(title)

            handle = kernel32.GetStdHandle(_STD_OUTPUT_HANDLE)
            mode = ctypes.c_uint32()
            if kernel32.GetConsoleMode(handle, ctypes.byref(mode)):
                kernel32.SetConsoleMode(
                    handle, mode.value | _ENABLE_VIRTUAL_TERMINAL_PROCESSING
                )
        except Exception:
            ansi = False

    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except Exception:
            pass
    return _Palette(ansi)


def _owns_console() -> bool:
    """判断窗口是不是"双击这个 exe"才出现的。

    是的话，进程退出窗口就会一起消失，用户来不及看任何输出，因此退出前要停一下。
    从 cmd/PowerShell 里跑起来的则不该多这一步。
    """

    if sys.platform != "win32":
        return False
    try:
        import ctypes

        buffer = (ctypes.c_uint32 * 8)()
        return ctypes.windll.kernel32.GetConsoleProcessList(buffer, 8) <= 1
    except Exception:
        return False


def _pause(palette: _Palette) -> None:
    print(f"\n{palette.dim}窗口即将关闭，按 Enter 退出……{palette.reset}", end="")
    try:
        input()
    except (EOFError, KeyboardInterrupt):
        pass


# ==========================================================================
# 环境自检
# ==========================================================================


def _node_version(binary: str, *, timeout: float = 8.0) -> str | None:
    """返回 Node 版本号；不可用时返回 ``None``。"""

    try:
        completed = subprocess.run(
            [binary, "--version"],
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    version = completed.stdout.strip()
    return version if completed.returncode == 0 and version else None


def _opencv_version() -> str | None:
    """返回 OpenCV 版本号；未安装时返回 ``None``。"""

    try:
        import cv2
    except Exception:
        return None
    return str(getattr(cv2, "__version__", "unknown"))


def _print_environment(settings: RuntimeSettings, palette: _Palette) -> bool:
    """打印运行环境自检结果，返回是否具备完整能力。"""

    ok_mark = f"{palette.green}✅{palette.reset}"
    bad_mark = f"{palette.red}❌{palette.reset}"

    node = _node_version(settings.node_binary)
    if node is None:
        print(f"  {bad_mark} Node        找不到可用的 Node")
        print(f"     {palette.dim}{settings.node_binary}{palette.reset}")
    else:
        origin = (
            "系统 PATH" if settings.node_binary == "node" else settings.node_binary
        )
        print(
            f"  {ok_mark} Node        {node}  "
            f"{palette.dim}{origin}{palette.reset}"
        )

    in_process = settings.vision_python == config.VISION_IN_PROCESS
    opencv = _opencv_version() if in_process else None
    if not in_process:
        print(
            f"  {ok_mark} 图像识别    独立解释器  "
            f"{palette.dim}{settings.vision_python}{palette.reset}"
        )
    elif opencv is None:
        print(f"  {bad_mark} 图像识别    分发包内未找到 OpenCV")
    else:
        print(
            f"  {ok_mark} 图像识别    OpenCV {opencv}  "
            f"{palette.dim}进程内{palette.reset}"
        )

    print(
        f"  {ok_mark} 运行参数    超时 {settings.timeout:g}s，"
        f"置信度下限 {settings.minimum_confidence}"
    )
    return node is not None and (not in_process or opencv is not None)


# ==========================================================================
# 使用示例
# ==========================================================================


def _print_usage(base_url: str, palette: _Palette) -> None:
    """打印可直接复制粘贴的调用示例。"""

    solve_url = base_url + config.API_SOLVE_PATH
    scene = config.DEFAULT_SCENE_ID

    blocks: list[tuple[str, list[str]]] = [
        (
            "PowerShell",
            [
                f'Invoke-RestMethod -Uri "{solve_url}" -Method Post `',
                "  -ContentType \"application/json\" `",
                f"  -Body '{json.dumps({'SceneId': scene})}'",
            ],
        ),
        (
            "curl（Windows 10 以上自带）",
            [
                f'curl -X POST "{solve_url}" ^',
                '  -H "Content-Type: application/json" ^',
                f'  -d "{{\\"SceneId\\":\\"{scene}\\"}}"',
            ],
        ),
        (
            "Python",
            [
                "import requests",
                f'r = requests.post("{solve_url}", json={{"SceneId": "{scene}"}})',
                "print(r.json())",
            ],
        ),
        (
            "浏览器直接打开（GET 等价写法）",
            [f"{solve_url}?SceneId={scene}"],
        ),
    ]

    for title, lines in blocks:
        print(f"\n  {palette.bold}{title}{palette.reset}")
        for line in lines:
            print(f"    {palette.cyan}{line}{palette.reset}")

    print(
        f"\n  {palette.dim}可选参数：proxy（本轮全部出网走该代理）、"
        f"prefix（实例前缀）、AaduaneId（覆盖 RPC key id）{palette.reset}"
    )
    print(
        f"  {palette.dim}健康检查：{base_url}{config.API_HEALTH_PATH}"
        f"{palette.reset}"
    )


# ==========================================================================
# 结果渲染
# ==========================================================================


class _ConsoleReporter:
    """把每一轮的响应渲染成窗口里的一段可读输出。

    ``ThreadingHTTPServer`` 会并发回调，因此整段多行输出必须持锁写出，否则两轮
    结果会交错成一团。
    """

    def __init__(self, *, palette: _Palette, show_token: bool) -> None:
        self.palette = palette
        self.show_token = show_token
        self._lock = threading.Lock()
        self._served = 0

    def __call__(
        self, method: str, path: str, status: int, body: dict[str, Any]
    ) -> None:
        if path == config.API_HEALTH_PATH:
            return  # 健康检查每秒都可能来一次，不值得占窗口。

        palette = self.palette
        stamp = time.strftime("%H:%M:%S")
        lines = self._body_lines(status, body)

        with self._lock:
            self._served += 1
            colour = (
                palette.green
                if status == 200 and body.get("ok")
                else palette.yellow
                if status == 200
                else palette.red
            )
            print(
                f"\n{palette.dim}[{stamp}]{palette.reset} "
                f"{palette.bold}#{self._served}{palette.reset} "
                f"{method} {path} {colour}{status}{palette.reset}"
            )
            for label, value in lines:
                print(f"    {palette.dim}{label:<11}{palette.reset}{value}")
            sys.stdout.flush()

    def _body_lines(
        self, status: int, body: dict[str, Any]
    ) -> list[tuple[str, str]]:
        palette = self.palette
        if not body.get("ok", False):
            if "error" in body:
                return [
                    ("结果", f"{palette.red}失败{palette.reset}"),
                    ("类型", str(body.get("errorType", "-"))),
                    ("说明", str(body.get("error", "-"))),
                ]
            # 协议往返正常但验证码没过：VerifyCode 才是原因。
            return [
                ("结果", f"{palette.yellow}未通过{palette.reset}"),
                ("VerifyCode", str(body.get("VerifyCode", "-"))),
                ("certifyId", str(body.get("certifyId", "-"))),
                ("耗时", f"{body.get('elapsedMs', '-')} ms"),
            ]

        lines = [
            (
                "结果",
                f"{palette.green}通过{palette.reset} "
                f"（{body.get('VerifyCode', '-')}）",
            ),
            ("场景", str(body.get("sceneId", "-"))),
            ("耗时", f"{body.get('elapsedMs', '-')} ms"),
            ("出网", "代理" if body.get("proxied") else "直连"),
            ("certifyId", str(body.get("certifyId", "-"))),
            ("token", self._render_token(body.get("securityToken"))),
        ]
        return lines

    def _render_token(self, token: Any) -> str:
        if not isinstance(token, str) or not token:
            return "-"
        if self.show_token or len(token) <= 20:
            return token
        return (
            f"{token[:8]}…{token[-8:]} "
            f"{self.palette.dim}（共 {len(token)} 字符，"
            f"--show-token 显示完整值）{self.palette.reset}"
        )


# ==========================================================================
# 示例请求
# ==========================================================================


def _fire_demo_request(base_url: str, palette: _Palette) -> None:
    """向自己的接口发一次示例请求。

    走真实 HTTP 而不是直接调 :func:`~.api.solve_once`，是为了让这条示例与外部
    调用方完全同路：同一套解析、同一套并发控制、同一套结果回调。
    """

    url = base_url + config.API_SOLVE_PATH
    payload = json.dumps({"SceneId": config.DEFAULT_SCENE_ID}).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    print(
        f"\n{palette.dim}→ 正在发起示例请求（会真实消耗一次挑战，请稍候）……"
        f"{palette.reset}"
    )
    try:
        # 结果由 result_hook 打印，这里只负责把异常变成人话。
        with urllib.request.urlopen(request, timeout=120) as response:
            response.read()
    except urllib.error.HTTPError as exc:
        exc.read()
    except Exception as exc:
        print(f"{palette.red}示例请求发送失败：{exc}{palette.reset}")


# ==========================================================================
# 桌面窗口模式
# ==========================================================================

_BANNER = "阿里 V3 滑块 · 纯协议复现"
_RULE = "─" * 66


def _print_banner(palette: _Palette) -> None:
    print(f"\n{palette.bold}{palette.cyan}{_BANNER}  v{__version__}{palette.reset}")
    print(f"{palette.dim}{_RULE}{palette.reset}")
    print(
        f"{palette.dim}仅用于获得明确授权的本地研究与协议验证环境。"
        f"{palette.reset}"
    )


class _ConsoleHandler(SliderApiHandler):
    """桌面窗口用的 handler。

    存在的理由有两个：一是把访问日志静音——:class:`_ConsoleReporter` 已经带时间
    戳打印了方法、路径与状态码，再来一行 stderr 就是重复；二是让窗口模式的配置
    落在子类上，不去污染 :class:`~.api.SliderApiHandler` 的类状态。
    """

    def log_message(self, format: str, *args: Any) -> None:
        return None


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="AliSlider",
        description="阿里 V3 滑块纯协议复现 · 桌面窗口模式",
    )
    add_server_arguments(parser)
    add_runtime_arguments(parser)
    add_confidence_argument(parser)
    parser.add_argument(
        "--show-token",
        action="store_true",
        help="在窗口里显示完整 securityToken（默认只显示首尾各 8 字符）",
    )
    parser.add_argument(
        "--no-pause",
        action="store_true",
        help="退出时不等待按键（双击运行时默认会等，以免窗口一闪而过）",
    )
    return parser


def run_console(argv: list[str]) -> int:
    """桌面窗口模式的主流程。"""

    args = build_parser().parse_args(argv)
    if args.max_concurrency < 0:
        raise SystemExit("--max-concurrency 必须 >= 0")
    if not 0.0 <= args.min_confidence <= 1.0:
        raise SystemExit("--min-confidence 必须位于 0..1")

    palette = _prepare_console(f"{_BANNER} v{__version__}")
    should_pause = _owns_console() and not args.no_pause

    try:
        return _serve(args, palette)
    except KeyboardInterrupt:
        return 0
    finally:
        if should_pause:
            _pause(palette)


def _serve(args: argparse.Namespace, palette: _Palette) -> int:
    settings = RuntimeSettings.from_args(args)
    fast_mode = fast_mode_enabled(args)
    concurrency = effective_server_concurrency(args)
    fixed_profile = generate_device_profile() if fast_mode else None

    _print_banner(palette)
    print(f"\n{palette.bold}运行环境{palette.reset}")
    if not _print_environment(settings, palette):
        print(
            f"\n{palette.red}环境不完整，无法开始。"
            f"{palette.reset}请确认分发包是否完整解压（不要只复制单个 exe）。"
        )
        return 1

    _ConsoleHandler.settings = settings
    _ConsoleHandler.slots = (
        threading.BoundedSemaphore(concurrency)
        if concurrency > 0
        else _Unlimited()
    )
    _ConsoleHandler.device_profile = fixed_profile
    _ConsoleHandler.result_hook = _ConsoleReporter(
        palette=palette, show_token=args.show_token
    )
    device_pool = (
        DeviceSessionPool(enabled=True, capacity=max(concurrency, 1))
        if fast_mode
        else None
    )
    _ConsoleHandler.device_pool = device_pool

    try:
        server = ThreadingHTTPServer((args.host, args.port), _ConsoleHandler)
    except OSError as exc:
        print(
            f"\n{palette.red}端口 {args.port} 无法监听：{exc}{palette.reset}\n"
            f"换一个端口重试，例如：AliSlider.exe --port 8010"
        )
        if device_pool is not None:
            device_pool.close()
        return 1

    server.daemon_threads = True
    host, port = server.server_address[:2]
    base_url = f"http://{host}:{port}"
    prewarmed = 0
    if device_pool is not None and fixed_profile is not None:
        print(
            f"\n{palette.bold}快速模式预热{palette.reset}  "
            f"正在并行准备 {max(concurrency, 1)} 个设备会话……"
        )
        prewarmed = device_pool.prime(
            settings.build_device_runtime(
                prefix=config.DEFAULT_PREFIX,
                proxies=None,
                device_profile=fixed_profile,
            )
        )
        print(
            f"  {palette.green}已就绪 {prewarmed}/{max(concurrency, 1)}"
            f"{palette.reset}；后续按使用量补回，不做定时请求"
        )

    print(f"\n{palette.bold}调用示例{palette.reset}")
    _print_usage(base_url, palette)

    print(f"\n{palette.dim}{_RULE}{palette.reset}")
    print(
        f"{palette.green}接口已就绪{palette.reset}  {palette.bold}"
        f"{base_url}{config.API_SOLVE_PATH}{palette.reset}"
    )
    print(
        f"{palette.dim}模式：{'快速' if fast_mode else '标准'}；"
        f"并发：{concurrency if concurrency > 0 else '不限'}{palette.reset}"
    )
    print(
        f"{palette.dim}在本窗口直接按 Enter 发起一次示例请求，输入 q 退出"
        f"（Ctrl+C 同样退出）{palette.reset}"
    )

    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        _interactive_loop(base_url, palette)
    finally:
        print(f"\n{palette.dim}正在停止……{palette.reset}")
        server.shutdown()
        server.server_close()
        if device_pool is not None:
            device_pool.close()
    return 0


def _interactive_loop(base_url: str, palette: _Palette) -> None:
    """读窗口里的指令；stdin 不可用时退化为一直挂着提供服务。"""

    while True:
        try:
            command = input().strip().lower()
        except EOFError:
            # 没有交互式 stdin（例如被当作后台服务拉起），安静地继续服务。
            while True:
                time.sleep(3600)
        except KeyboardInterrupt:
            return

        if command in {"q", "quit", "exit"}:
            return
        if command == "":
            _fire_demo_request(base_url, palette)
        else:
            print(
                f"{palette.dim}未知指令 {command!r}；Enter 发起示例请求，"
                f"q 退出{palette.reset}"
            )


# ==========================================================================
# 自检报告
# ==========================================================================


def _pad(text: str, width: int) -> str:
    """按终端显示宽度右补空格。

    ``str.ljust`` 数的是字符个数，而中文在终端里占两列，直接用会让整列歪掉。
    """

    columns = sum(
        2 if unicodedata.east_asian_width(char) in "WF" else 1 for char in text
    )
    return text + " " * max(width - columns, 0)


def _module_version(name: str) -> str | None:
    """返回模块的版本号；导入不了时返回 ``None``。"""

    try:
        module = importlib.import_module(name)
    except Exception:
        return None
    return str(getattr(module, "__version__", "unknown"))


def run_doctor() -> int:
    """逐项打印运行形态与关键资源的解析结果，返回 0 表示全部就绪。

    分发包在别人机器上跑不起来时，这是第一手排障信息：它回答"文件到底在不在、
    Node 能不能起、OpenCV 有没有打进来"，而不是只丢一句"运行失败"。
    """

    palette = _prepare_console("AliSlider doctor")
    ok_mark = f"{palette.green}✅{palette.reset}"
    bad_mark = f"{palette.red}❌{palette.reset}"
    healthy = True

    def report(good: bool, label: str, detail: str, note: str = "") -> None:
        mark = ok_mark if good else bad_mark
        tail = f"  {palette.dim}{note}{palette.reset}" if note else ""
        print(f"  {mark} {_pad(label, 14)}{detail}{tail}")

    print(f"\n{palette.bold}AliSlider 自检 v{__version__}{palette.reset}")
    print(f"{palette.dim}{_RULE}{palette.reset}")

    print(f"\n{palette.bold}运行形态{palette.reset}")
    for label, detail in (
        ("形态", "冻结分发" if config.FROZEN else "源码运行"),
        ("可执行文件", sys.executable),
        ("资源根目录", str(config.BUNDLE_ROOT or config.PACKAGE_ROOT.parent)),
        ("Python", f"{sys.version.split()[0]}  {sys.platform}"),
    ):
        print(f"     {_pad(label, 14)}{detail}")

    print(f"\n{palette.bold}随包资源{palette.reset}")
    for label, path in (
        ("PE 桥", config.PE_DATA_BRIDGE),
        ("设备桥", config.SDK_DEVICE_BRIDGE),
        ("默认轨迹", config.DEFAULT_TOUCH_TRACK),
    ):
        exists = path.is_file()
        healthy = healthy and exists
        detail = f"{path.stat().st_size:>9,} 字节" if exists else "缺失"
        report(exists, label, detail, str(path))

    print(f"\n{palette.bold}运行时{palette.reset}")
    node_binary = config.DEFAULT_NODE_BINARY
    node = _node_version(node_binary)
    healthy = healthy and node is not None
    report(node is not None, "Node", node or "无法运行", node_binary)

    # OpenCV/NumPy 在源码运行下本来就可以装在另一个解释器里，不算故障；
    # 冻结分发里它们必须在包内，缺了就是包不完整。
    for label, module in (("OpenCV", "cv2"), ("NumPy", "numpy")):
        version = _module_version(module)
        if version is not None:
            report(True, label, version)
        elif config.FROZEN:
            healthy = False
            report(False, label, "缺失")
        else:
            report(True, label, "不在本解释器", "源码运行下正常")

    for module in ("requests", "cryptography"):
        version = _module_version(module)
        healthy = healthy and version is not None
        report(version is not None, module, version or "缺失")

    print(f"\n{palette.dim}{_RULE}{palette.reset}")
    if healthy:
        print(f"{palette.green}全部就绪{palette.reset}")
    else:
        print(
            f"{palette.red}存在缺失项{palette.reset}"
            "：分发包可能没有完整解压，请重新解压整个文件夹。"
        )
    return 0 if healthy else 1


# ==========================================================================
# 总入口
# ==========================================================================

_TOP_HELP = f"""\
阿里 V3 滑块纯协议复现 v{__version__}

用法：
  AliSlider.exe                    启动快速 HTTP API 并打开本地接口文档
  AliSlider.exe api   [参数...]    纯 HTTP 接口，不打印横幅，适合当后台服务
  AliSlider.exe solve [参数...]    只 Init/下载/识别，不发 Verify，不消耗挑战
  AliSlider.exe run   [参数...]    完整一轮，只发送一次 Verify
  AliSlider.exe doctor             环境自检：跑不起来时先看它

各模式的完整参数：
  AliSlider.exe api --help
  AliSlider.exe solve --help
  AliSlider.exe run --help

HTTP API 参数可直接跟在 exe 后，也可使用 api 子命令。
"""


def _select_startup_arguments(
    *, input_fn: Callable[[str], str] = input
) -> list[str] | None:
    """无参数打开时让用户选择模式；EOF 安全回落到推荐快速模式。"""

    print(
        "\n请选择启动模式：\n"
        "  1. 快速模式（推荐）  5 并发 + 5 个预热会话，兼顾单包与吞吐\n"
        "  2. 标准模式          1 并发 + 逐轮设备画像，不做投机预热\n"
        "  3. 自定义模式        自己选择并发数、端口与是否预热\n"
        "  4. 环境自检\n"
        "  q. 退出"
    )

    while True:
        try:
            choice = input_fn("\n选择 [1]：").strip().lower()
        except EOFError:
            choice = ""
        except KeyboardInterrupt:
            return None

        if choice in {"", "1"}:
            return ["--runtime-mode", "fast", "--max-concurrency", "5"]
        if choice == "2":
            return ["--runtime-mode", "standard", "--max-concurrency", "1"]
        if choice == "4":
            return ["doctor"]
        if choice in {"q", "quit", "exit"}:
            return None
        if choice != "3":
            print("请输入 1、2、3、4 或 q。")
            continue

        concurrency = _read_menu_integer(
            "并发数 [5]：", default=5, minimum=1, maximum=32, input_fn=input_fn
        )
        port = _read_menu_integer(
            f"监听端口 [{config.API_PORT}]：",
            default=config.API_PORT,
            minimum=1,
            maximum=65535,
            input_fn=input_fn,
        )
        while True:
            try:
                warm = input_fn("启用快速预热 [Y/n]：").strip().lower()
            except EOFError:
                warm = ""
            if warm in {"", "y", "yes"}:
                mode = "fast"
                break
            if warm in {"n", "no"}:
                mode = "standard"
                break
            print("请输入 y 或 n。")
        return [
            "--runtime-mode",
            mode,
            "--max-concurrency",
            str(concurrency),
            "--port",
            str(port),
        ]


def _read_menu_integer(
    prompt: str,
    *,
    default: int,
    minimum: int,
    maximum: int,
    input_fn: Callable[[str], str],
) -> int:
    """读取菜单里的有界整数，空值/EOF 使用默认值。"""

    while True:
        try:
            raw = input_fn(prompt).strip()
        except EOFError:
            return default
        if not raw:
            return default
        try:
            value = int(raw)
        except ValueError:
            value = minimum - 1
        if minimum <= value <= maximum:
            return value
        print(f"请输入 {minimum}..{maximum} 的整数。")


def main(argv: list[str] | None = None) -> int:
    """打包后可执行文件的总入口，按首个位置参数分发到各模式。"""

    arguments = list(sys.argv[1:] if argv is None else argv)
    if not arguments:
        # Windows 双击启动时仍是本地代码页，先准备 UTF-8 再让
        # 纯 API 入口打印可点击的文档链接。
        _prepare_console(f"{_BANNER} v{__version__}")
        return int(
            api_main(
                [
                    "--runtime-mode",
                    "fast",
                    "--max-concurrency",
                    "5",
                    "--open-docs",
                ]
            )
        )
    command = arguments[0].lower() if arguments else ""

    if command == "api":
        return int(api_main(arguments[1:]))
    if command in {"solve", "run"}:
        return int(cli_main(arguments))
    if command == "cli":
        return int(cli_main(arguments[1:]))
    if command == "doctor":
        return run_doctor()
    if command == "console":
        # 仅保留给旧脚本显式调用；双击和普通服务参数不再走交互循环。
        return run_console(arguments[1:])
    if command in {"help", "-h", "--help"}:
        # 先给出模式总览，再接上 API 参数说明。
        print(_TOP_HELP)
        return int(api_main(["--help"]))
    return int(api_main(arguments))


__all__ = ["build_parser", "main", "run_console", "run_doctor"]


if __name__ == "__main__":
    raise SystemExit(main())
