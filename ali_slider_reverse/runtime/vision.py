"""OpenCV 缺口求解的运行时封装。

## 两种执行策略

```text
子进程   源码运行：主协议解释器不必装 OpenCV，识别环境也不必装协议依赖
进程内   冻结分发：OpenCV 已随包打进同一个可执行文件，没有"第二个解释器"可言
```

由 ``python_executable`` 选择：等于 :data:`~ali_slider_reverse.config.VISION_IN_PROCESS`
时走进程内，否则拉起该解释器。两条路径的对外行为完全一致。

## 为什么要预热

OpenCV/NumPy 的冷导入约 200ms，其后**首次** ``solve_gap`` 还要再付约 77ms 的算子
初始化（热调用只要约 17ms）。两笔加起来占单轮总耗时相当一部分。:meth:`prewarm`
会在**收到图片路径之前**就把它们全部做完，调用方可以在 DeviceToken 生成、Init
请求和资源下载期间把这段固定成本重叠掉，等图片到齐时只剩热算法的耗时。

预热资源如果最终没被消费（比如识别前就异常退出），必须显式 :meth:`close`
回收——CLI 进程退出即可，但常驻 API 服务每轮都会新建 worker。
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .. import config
from ..errors import VisionError
from ..vision.geometry import pe_slide_pos_from_puzzle_x


_WORKER_MODULE = "ali_slider_reverse.entrypoints.vision_worker"

_SolveOutput = tuple[int, float, tuple[dict[str, Any], ...]]
"""一次底层求解的原始结果：``xPos``、置信度与候选清单。"""


@dataclass(frozen=True, slots=True)
class VisionResult:
    """一次缺口识别的结果。"""

    x_pos: int
    """缺口在背景图原始像素坐标系中的画布左坐标。"""

    slide_pos: int
    """由 ``x_pos`` 反解出的手柄位移。"""

    confidence: float
    """0..1 的置信度；低于阈值时调用方应停止且不发送 Verify。"""

    candidates: tuple[dict[str, Any], ...] = field(repr=False)
    """各检测方法给出的候选，按得分降序，仅供调试与人工复核。"""


# ==========================================================================
# 进程内策略：冻结分发
# ==========================================================================


class _InProcessSolver:
    """在本进程内直接调用 OpenCV 求解。

    预热放在后台线程里：冷导入是 CPU 密集的，但调用方此时正阻塞在网络与 Node
    子进程上（两者都释放 GIL），因此这段时间足够把导入与算子初始化跑完。
    """

    def __init__(self, *, timeout: float) -> None:
        self.timeout = timeout
        self._warm: threading.Thread | None = None

    def prewarm(self) -> None:
        if self._warm is not None:
            return
        thread = threading.Thread(
            target=self._warm_quietly, name="ali-vision-warm", daemon=True
        )
        self._warm = thread
        thread.start()

    @staticmethod
    def _warm_quietly() -> None:
        from ..vision.gap_solver import warm_up

        warm_up()

    def solve(self, background: Path, shadow: Path) -> _SolveOutput:
        # 预热若仍在进行，先等它跑完：此刻网络已经无事可做，让两个线程同时做同
        # 一份冷初始化只会互相拖慢。超时则直接自己算，最多退回未预热的耗时。
        thread, self._warm = self._warm, None
        if thread is not None:
            thread.join(timeout=self.timeout)

        from ..vision.gap_solver import solve_gap

        estimate = solve_gap(background, shadow)
        candidates = tuple(
            {
                "xPos": candidate.canvas_left,
                "score": candidate.score,
                "method": candidate.method,
            }
            for candidate in estimate.candidates
        )
        return estimate.x_pos, float(estimate.confidence), candidates

    def close(self) -> None:
        # 预热线程是 daemon，且只写临时目录，放着不管即可；这里只丢掉引用，
        # 让下一次 prewarm 能重新开始。
        self._warm = None


# ==========================================================================
# 子进程策略：源码运行
# ==========================================================================


class _SubprocessSolver:
    """把求解隔离到独立解释器进程。

    典型用法：构造后立刻 :meth:`prewarm`，图片就绪后 :meth:`solve`。``solve``
    会消费掉预热进程（一个进程只服务一轮）。
    """

    def __init__(self, *, python_executable: str, timeout: float) -> None:
        self.python_executable = python_executable
        self.timeout = timeout
        self._process: subprocess.Popen[str] | None = None

    @staticmethod
    def _environment() -> dict[str, str]:
        """让子进程能以 ``-m`` 方式导入本包。

        视觉解释器通常不会安装本项目，所以把包的父目录追加到 PYTHONPATH，
        而不是覆盖调用方原有的设置。
        """

        environment = os.environ.copy()
        package_parent = str(config.PACKAGE_ROOT.parent)
        prior = environment.get("PYTHONPATH")
        environment["PYTHONPATH"] = (
            package_parent if not prior else package_parent + os.pathsep + prior
        )
        return environment

    def prewarm(self) -> None:
        """启动 worker 并让它提前完成 OpenCV/NumPy 的冷导入。

        重复调用是安全的：已有存活进程时直接返回。
        """

        if self._process is not None and self._process.poll() is None:
            return
        self.close()
        try:
            self._process = subprocess.Popen(
                [self.python_executable, "-m", _WORKER_MODULE, "--worker"],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                bufsize=1,
                env=self._environment(),
            )
        except FileNotFoundError as exc:
            raise VisionError("OpenCV vision bridge 不可用") from exc

    def solve(self, background: Path, shadow: Path) -> _SolveOutput:
        """把图片路径交给子进程，取回识别结果。

        预热进程走 stdin/stdout 的 JSON 行协议；没有预热时退化为一次性
        命令行调用，行为等价、只是多付一次冷导入成本。
        """

        process = self._process
        self._process = None  # 无论成败，这个 worker 都已被消费。
        try:
            if process is None:
                completed = subprocess.run(
                    [
                        self.python_executable,
                        "-m",
                        _WORKER_MODULE,
                        "--background",
                        str(background),
                        "--shadow",
                        str(shadow),
                    ],
                    capture_output=True,
                    text=True,
                    check=True,
                    timeout=self.timeout,
                    env=self._environment(),
                )
            else:
                request = (
                    json.dumps(
                        {"background": str(background), "shadow": str(shadow)},
                        ensure_ascii=False,
                        separators=(",", ":"),
                    )
                    + "\n"
                )
                try:
                    stdout, stderr = process.communicate(
                        request, timeout=self.timeout
                    )
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.communicate()
                    raise
                completed = subprocess.CompletedProcess(
                    args=process.args,
                    returncode=process.returncode,
                    stdout=stdout,
                    stderr=stderr,
                )
                completed.check_returncode()
        except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
            raise VisionError("OpenCV vision bridge 不可用") from exc
        except subprocess.CalledProcessError as exc:
            # 只回传 stderr 最后一行，避免把整段 traceback 或图片路径写进日志。
            detail = exc.stderr.strip().splitlines()[-1:]
            raise VisionError(
                "OpenCV 缺口求解失败："
                + (detail[0][:500] if detail else "未知错误")
            ) from exc

        try:
            output = json.loads(completed.stdout.splitlines()[-1])
            return (
                int(output["xPos"]),
                float(output["confidence"]),
                tuple(output.get("candidates", [])),
            )
        except (IndexError, KeyError, TypeError, ValueError) as exc:
            raise VisionError("vision bridge 输出无效") from exc

    def close(self) -> None:
        """回收尚未被 :meth:`solve` 消费的预热进程。"""

        process = self._process
        self._process = None
        if process is None:
            return
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=1.0)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                stream.close()


# ==========================================================================
# 对外门面
# ==========================================================================


class VisionWorker:
    """缺口识别的统一入口，按运行形态选择底层策略。"""

    def __init__(
        self,
        *,
        python_executable: str,
        timeout: float = config.DEFAULT_TIMEOUT,
    ) -> None:
        if not python_executable:
            raise ValueError("python_executable 不能为空")
        if timeout <= 0:
            raise ValueError("timeout 必须为正数")
        self.python_executable = python_executable
        self.timeout = float(timeout)
        self._solver: _InProcessSolver | _SubprocessSolver = (
            _InProcessSolver(timeout=self.timeout)
            if python_executable == config.VISION_IN_PROCESS
            else _SubprocessSolver(
                python_executable=python_executable, timeout=self.timeout
            )
        )

    @property
    def in_process(self) -> bool:
        """当前是否走进程内求解。"""

        return isinstance(self._solver, _InProcessSolver)

    def prewarm(self) -> None:
        """提前付掉 OpenCV 冷导入与算子初始化的固定成本。"""

        self._solver.prewarm()

    def solve(
        self,
        background: Path,
        shadow: Path,
        *,
        x_pos_override: int | None = None,
        rendered_width: int = config.SLIDER_RENDERED_WIDTH,
        handle_width: int = config.SLIDER_HANDLE_WIDTH,
    ) -> VisionResult:
        """求解缺口位置。

        传入 ``x_pos_override`` 时跳过整个识别流程，直接采信人工坐标并把置信度
        记为 1.0——该通道仅用于获授权的研究与人工复核。
        """

        if x_pos_override is not None:
            if isinstance(x_pos_override, bool) or x_pos_override < 0:
                raise ValueError("x_pos_override 必须是非负整数")
            x_pos = int(x_pos_override)
            confidence = 1.0
            candidates: tuple[dict[str, Any], ...] = ()
        else:
            x_pos, confidence, candidates = self._solver.solve(background, shadow)

        slide_pos = int(
            pe_slide_pos_from_puzzle_x(
                x_pos,
                rendered_width=rendered_width,
                handle_width=handle_width,
                rounded=True,
            )
        )
        return VisionResult(
            x_pos=x_pos,
            slide_pos=slide_pos,
            confidence=confidence,
            candidates=candidates,
        )

    def close(self) -> None:
        """回收尚未被 :meth:`solve` 消费的预热资源。"""

        self._solver.close()

    def __del__(self) -> None:  # pragma: no cover - 异常退出时的兜底回收。
        try:
            self.close()
        except Exception:
            pass


__all__ = ["VisionResult", "VisionWorker"]
