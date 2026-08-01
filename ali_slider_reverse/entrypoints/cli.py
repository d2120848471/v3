"""命令行入口。

两个子命令的副作用边界差别很大，不要混用：

```text
solve   设备链 + Init + 下载 + 识别            不发送 Verify，不消耗挑战
run     上述全部 + PE 构造 + 一次 VerifyCaptchaV3
run --submit-business   在 T001 之后再提交一次业务请求
```

``solve`` 用于确认环境是否正常；``run`` 会真实消耗一次挑战；业务提交还需要显式
传入本地抓包与 app bundle，避免误触发下单一类的外部副作用。

退出码：

```text
0  成功（业务模式下还要求业务 HTTP 2xx/3xx）
1  协议、参数或运行错误
2  验证码未通过
3  验证码通过但业务 HTTP 返回非成功状态
```
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from contextlib import ExitStack, closing
from typing import Any

from .. import config
from ..challenge.business import BusinessResponse, load_business_template_from_capture
from ..challenge.session import (
    AliSliderClient,
    ChallengeOutcome,
    VerifyBuild,
)
from ..errors import AliSliderError
from ..runtime.node_device import DeviceRuntimeResult
from ..runtime.vision import VisionResult
from .options import (
    RuntimeSettings,
    add_confidence_argument,
    add_runtime_arguments,
)


def _emit(value: Any, *, stream: Any = sys.stdout) -> None:
    print(
        json.dumps(value, ensure_ascii=False, separators=(",", ":")),
        file=stream,
    )


def _prepared_client(
    args: argparse.Namespace,
    settings: RuntimeSettings,
) -> AliSliderClient:
    """装配客户端，并把固定成本提前到设备链之前。"""

    client = settings.build_client(scene_id=args.scene_id)
    if args.x_pos is None:
        # OpenCV 冷导入与 DeviceToken、Init 和资源下载重叠。
        client.prewarm_vision()
    # 本轮五个出网主机的 TLS 握手与设备链重叠。
    client.prewarm_connections()
    return client


# ==========================================================================
# 输出摘要
#
# 所有摘要都是**白名单**输出：只列出明确安全的字段，不做"排除敏感字段"式的黑
# 名单。token、session、指纹密文、响应正文都不进 stdout。
# ==========================================================================


def _device_summary(device: DeviceRuntimeResult) -> dict[str, Any]:
    """只输出结构与校验状态，不输出 token、session 或指纹密文。"""

    return {
        "source": device.token_source,
        "requests": list(device.request_actions),
        "requestCount": device.request_count,
        "sameSession": (
            device.init_parsed.session_id == device.verify_parsed.session_id
        ),
        "getterArgumentCount": device.getter_argument_count,
        "interactionEventCount": device.interaction_event_count,
        "fingerprintFieldCount": device.fingerprint_field_count,
        "gatherCost": {
            "init": int(device.init_parsed.gather_cost),
            "verify": int(device.verify_parsed.gather_cost),
            "normalized": device.gather_cost_normalized,
        },
        "cipherLength": {
            "init": len(device.init_parsed.fingerprint_cipher),
            "verify": len(device.verify_parsed.fingerprint_cipher),
        },
    }


def _vision_summary(vision: VisionResult) -> dict[str, Any]:
    return {
        "xPos": vision.x_pos,
        "slidePos": vision.slide_pos,
        "confidence": round(vision.confidence, 6),
        "candidates": list(vision.candidates[:8]),
    }


def _track_summary(build: VerifyBuild) -> dict[str, Any]:
    """只输出原生轨迹结构，不输出本轮绝对时间或事件派生值。"""

    return {
        "eventCount": build.track_event_count,
        "dataMousemoveEventCount": build.native_mousemove_event_count,
        "replayedBeforeGetterCount": len(build.feilin_interaction_events),
        "postGetterDataEventCount": build.post_getter_mousemove_event_count,
        "timingValidated": True,
    }


def _business_summary(response: BusinessResponse) -> dict[str, Any]:
    """从业务响应提取状态字段，避免把订单、地址等数据写到终端。"""

    summary: dict[str, Any] = {
        "statusCode": response.status_code,
        "bodyType": type(response.body).__name__,
    }
    if isinstance(response.body, dict):
        summary["bodyKeys"] = list(response.body)[:30]
        for key in ("Code", "code", "Success", "success", "status"):
            value = response.body.get(key)
            if isinstance(value, (str, int, float, bool)) or value is None:
                summary[key] = value
        for key in ("Message", "message"):
            value = response.body.get(key)
            if isinstance(value, str):
                summary[key + "Length"] = len(value)
    elif isinstance(response.body, str):
        summary["bodyLength"] = len(response.body)
    return summary


def _verify_output(outcome: ChallengeOutcome) -> dict[str, Any]:
    """默认 ``run`` 的四字段输出。

    这四个字段包含本轮敏感结果，不要随意写入日志或提交到仓库。
    """

    verify = outcome.verify
    return {
        "securityToken": verify.security_token,
        "VerifyCode": verify.verify_code,
        "VerifyResult": verify.verify_result,
        "certifyId": verify.certify_id,
    }


# ==========================================================================
# 子命令
# ==========================================================================


def _solve_command(args: argparse.Namespace) -> int:
    """执行设备链、Init、下载与识别，但不构造也不发送 Verify。"""

    settings = RuntimeSettings.from_args(args)
    client = _prepared_client(args, settings)

    with ExitStack() as stack:
        stack.callback(client.close)
        device_session = stack.enter_context(
            settings.build_device_runtime().challenge_session()
        )
        challenge = client.init_challenge(device_session.init_token)

        if args.artifacts_dir is None:
            directory: str = stack.enter_context(
                tempfile.TemporaryDirectory(prefix="ali-slider-preflight-")
            )
            artifacts_value: str | None = None
        else:
            directory = args.artifacts_dir
            artifacts_value = str(directory)

        assets = client.download_assets(challenge, directory)
        upload_log_succeeded = client.upload_initialization_log(
            challenge, assets
        )
        vision = client.solve_assets(assets, x_pos_override=args.x_pos)
        # solve 不执行动态 PE 轨迹，因此也不猜测最终 getter 参数；
        # 这里只报告已完成容器与 session 校验的 Init 运行态。
        device = device_session.initial_result

    _emit(
        {
            "ok": True,
            "mode": "solve-only-no-verify",
            "device": _device_summary(device),
            "challenge": {
                "captchaType": challenge.captcha_type,
                "staticPath": challenge.static_path,
            },
            "vision": _vision_summary(vision),
            "initializationLog": {
                "attempted": True,
                "succeeded": upload_log_succeeded,
                "cssLoaded": assets.stylesheet_loaded,
            },
            "artifactsDir": artifacts_value,
        }
    )
    return 0


def _run_command(args: argparse.Namespace) -> int:
    """执行完整一轮，并只发送一次 Verify。"""

    if args.submit_business and (args.capture is None or args.app_bundle is None):
        raise AliSliderError("--submit-business 需要同时提供 --capture 和 --app-bundle")

    settings = RuntimeSettings.from_args(args)
    client = _prepared_client(args, settings)
    with closing(client):
        outcome = client.run_captcha(
            settings.build_device_runtime(),
            artifacts_dir=args.artifacts_dir,
            fixture_path=args.fixture,
            x_pos_override=args.x_pos,
            minimum_confidence=settings.minimum_confidence,
        )

        if not args.submit_business:
            _emit(_verify_output(outcome))
            return 0 if outcome.verify.succeeded else 2

        output: dict[str, Any] = {
            "ok": outcome.verify.succeeded,
            "mode": "captcha-verify",
            "device": _device_summary(outcome.device),
            "challenge": {
                "captchaType": outcome.challenge.captcha_type,
                "staticPath": outcome.challenge.static_path,
            },
            "vision": _vision_summary(outcome.vision),
            "track": _track_summary(outcome.build),
            "initializationLog": {
                "attempted": True,
                "succeeded": outcome.upload_log_succeeded,
            },
            "verify": {
                "code": outcome.verify.verify_code,
                "result": outcome.verify.verify_result,
                "hasSecurityToken": bool(outcome.verify.security_token),
            },
        }

        if outcome.verify.succeeded:
            business = client.submit_business(
                load_business_template_from_capture(args.capture),
                outcome.verify,
                app_bundle=args.app_bundle,
            )
            output["business"] = _business_summary(business)
        else:
            output["business"] = {
                "submitted": False,
                "reason": "captcha-not-T001",
            }

    _emit(output)
    if not outcome.verify.succeeded:
        return 2
    status = int(output["business"]["statusCode"])
    return 0 if 200 <= status < 400 else 3


# ==========================================================================
# 参数
# ==========================================================================


def _add_shared_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--scene-id",
        default=config.DEFAULT_SCENE_ID,
        help=f"验证码场景 ID（默认 {config.DEFAULT_SCENE_ID}）",
    )
    add_runtime_arguments(parser)
    parser.add_argument(
        "--x-pos",
        type=int,
        help="人工覆盖识别出的 xPos；省略时使用 OpenCV",
    )
    parser.add_argument(
        "--artifacts-dir",
        help="可选：保存本轮 back.png、shadow.png 与 pe.js 的目录",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m ali_slider_reverse.entrypoints.cli",
        description="阿里 V3 滑块纯协议复现（不操作或刷新浏览器页面）",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    solve = subparsers.add_parser(
        "solve", help="设备链 + Init + 下载资源 + 识别，不发送 Verify"
    )
    _add_shared_arguments(solve)
    solve.set_defaults(handler=_solve_command)

    run = subparsers.add_parser(
        "run", help="执行一轮 Init/识别/轨迹/data，并只发送一次 Verify"
    )
    _add_shared_arguments(run)
    add_confidence_argument(run)
    run.add_argument("--fixture", help="可选：用户授权的 touch 轨迹 fixture")
    run.add_argument(
        "--submit-business",
        action="store_true",
        help="T001 后再签名并提交一次本地业务请求",
    )
    run.add_argument("--capture", help="本地原始 CDP 抓包，用于读取业务模板")
    run.add_argument(
        "--app-bundle", help="当前公开 app bundle，用于按哈希锁定业务签名尾串"
    )
    run.set_defaults(handler=_run_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.handler(args))
    except (AliSliderError, ValueError) as exc:
        _emit(
            {
                "ok": False,
                "errorType": type(exc).__name__,
                "error": str(exc),
            },
            stream=sys.stderr,
        )
        return 1
    except Exception:  # pragma: no cover - 兜底：不把未预期异常内容写出去。
        _emit(
            {
                "ok": False,
                "errorType": "UnhandledProtocolError",
                "error": "未处理的协议运行错误",
            },
            stream=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
