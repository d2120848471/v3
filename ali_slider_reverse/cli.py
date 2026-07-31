"""阿里 V3 滑块纯协议命令行入口。

``solve`` 只执行设备链、Init、资源下载与缺口识别；``run`` 才会为新 CertifyId
发送一次 Verify。业务 gopay 还需要显式 ``--submit-business``，避免误下单。
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path
from typing import Any

from .client import (
    DEFAULT_SCENE_ID,
    AliSliderClient,
    AliSliderError,
    BusinessResponse,
    load_business_template_from_capture,
)
from .device_runtime import (
    DEFAULT_FIRST_TOUCH_AGE_RANGE,
    DeviceRuntimeClient,
    DeviceRuntimeError,
)
from .pe_runtime import PeRuntimeError


def _json_print(value: Any, *, stream: Any = sys.stdout) -> None:
    print(
        json.dumps(
            value,
            ensure_ascii=False,
            separators=(",", ":"),
        ),
        file=stream,
    )


def _runtime_from_args(args: argparse.Namespace) -> DeviceRuntimeClient:
    return DeviceRuntimeClient(
        node_binary=args.node,
        sdk_path=args.sdk_js,
        prefix="fsgtmi",
        region="cn",
        timeout=args.timeout,
        gather_cost_range=(
            args.gather_cost_min,
            args.gather_cost_max,
        ),
        first_touch_age_range=(
            args.first_touch_age_min,
            args.first_touch_age_max,
        ),
    )


def _client_from_args(args: argparse.Namespace) -> AliSliderClient:
    client = AliSliderClient(
        scene_id=args.scene_id,
        node_binary=args.node,
        vision_python=args.vision_python,
        timeout=args.timeout,
    )
    if args.x_pos is None:
        # OpenCV/NumPy 冷导入与后续 DeviceToken、Init 和资源下载重叠；图片到齐时
        # solve_assets 只需执行热算法与一次本地 IPC。
        client._prewarm_vision()
    return client


def _device_summary(device: Any) -> dict[str, Any]:
    """只输出结构与验证状态，不输出 token、session 或指纹密文。"""

    return {
        "source": device.token_source,
        "requests": list(device.request_actions),
        "requestCount": device.request_count,
        "sameSession": (
            device.init_parsed.session_id
            == device.verify_parsed.session_id
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


def _vision_summary(vision: Any) -> dict[str, Any]:
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
        "dataMousemoveEventCount": (
            build.native_mousemove_event_count
        ),
        "replayedBeforeGetterCount": len(
            build.feilin_interaction_events
        ),
        "postGetterDataEventCount": (
            build.post_getter_mousemove_event_count
        ),
        "timingValidated": True,
    }


def _business_response_summary(response: BusinessResponse) -> dict[str, Any]:
    """从业务响应提取状态字段，避免把订单、地址等响应数据写到终端。"""

    summary: dict[str, Any] = {
        "statusCode": response.status_code,
        "bodyType": type(response.body).__name__,
    }
    if isinstance(response.body, dict):
        summary["bodyKeys"] = list(response.body)[:30]
        for key in (
            "Code",
            "code",
            "Success",
            "success",
            "status",
        ):
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


def _verify_result_output(verify: Any) -> dict[str, Any]:
    """白名单输出校验包；不序列化 request_id、raw 或运行诊断。"""

    return {
        "securityToken": verify.security_token,
        "VerifyCode": verify.verify_code,
        "VerifyResult": verify.verify_result,
        "certifyId": verify.certify_id,
    }


def _solve_command(args: argparse.Namespace) -> int:
    client = _client_from_args(args)
    runtime = _runtime_from_args(args)
    with runtime.challenge_session() as device_session:
        challenge = client.init_challenge(device_session.init_token)

        if args.artifacts_dir is not None:
            directory = Path(args.artifacts_dir).expanduser().resolve()
            assets = client.download_assets(challenge, directory)
            upload_log_succeeded = client.upload_initialization_log(
                challenge,
                assets,
            )
            vision = client.solve_assets(
                assets,
                x_pos_override=args.x_pos,
            )
            artifacts_value: str | None = str(directory)
        else:
            with tempfile.TemporaryDirectory(
                prefix="ali-slider-preflight-"
            ) as temporary:
                assets = client.download_assets(challenge, temporary)
                upload_log_succeeded = client.upload_initialization_log(
                    challenge,
                    assets,
                )
                vision = client.solve_assets(
                    assets,
                    x_pos_override=args.x_pos,
                )
            artifacts_value = None
        # solve 模式不执行动态 PE 轨迹，因而也不猜测最终 getter 参数；
        # 只返回已完成容器与 session 校验的 Init 运行态。
        device = device_session.initial_result

    _json_print(
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
    client = _client_from_args(args)
    runtime = _runtime_from_args(args)
    device, challenge, vision, build, verify = client.run_captcha(
        runtime,
        artifacts_dir=args.artifacts_dir,
        fixture_path=args.fixture,
        x_pos_override=args.x_pos,
        minimum_confidence=args.min_confidence,
    )
    if not args.submit_business:
        _json_print(_verify_result_output(verify), stream=sys.stdout)
        return 0 if verify.succeeded else 2

    output: dict[str, Any] = {
        "ok": verify.succeeded,
        "mode": "captcha-verify",
        "device": _device_summary(device),
        "challenge": {
            "captchaType": challenge.captcha_type,
            "staticPath": challenge.static_path,
        },
        "vision": _vision_summary(vision),
        "track": _track_summary(build),
        "initializationLog": {
            "attempted": True,
            "succeeded": build.upload_log_succeeded,
        },
        "verify": {
            "code": verify.verify_code,
            "result": verify.verify_result,
            "hasSecurityToken": bool(verify.security_token),
        },
    }

    if args.submit_business:
        if args.capture is None or args.app_bundle is None:
            raise AliSliderError(
                "--submit-business 需要同时提供 --capture 和 --app-bundle"
            )
        if verify.succeeded:
            template = load_business_template_from_capture(args.capture)
            business = client.submit_business(
                template,
                verify,
                app_bundle=args.app_bundle,
            )
            output["business"] = _business_response_summary(business)
        else:
            output["business"] = {
                "submitted": False,
                "reason": "captcha-not-T001",
            }

    _json_print(output, stream=sys.stdout)
    if not verify.succeeded:
        return 2
    if args.submit_business and "statusCode" in output["business"]:
        status = int(output["business"]["statusCode"])
        return 0 if 200 <= status < 400 else 3
    return 0


def _add_common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--scene-id",
        default=DEFAULT_SCENE_ID,
        help=f"验证码场景 ID（默认 {DEFAULT_SCENE_ID}）",
    )
    parser.add_argument(
        "--sdk-js",
        help="可选：本地公开 AliyunCaptcha.js；省略则从官方 CDN 下载",
    )
    parser.add_argument(
        "--node",
        default="node",
        help="Node 可执行文件（默认 node）",
    )
    parser.add_argument(
        "--vision-python",
        default="/opt/homebrew/bin/python3",
        help="带 OpenCV/NumPy 的 Python（默认 /opt/homebrew/bin/python3）",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=25.0,
        help="单步网络/子进程超时秒数（默认 25）",
    )
    parser.add_argument(
        "--gather-cost-min",
        type=int,
        default=180,
        help="Node 过快时 GatherCost 正常化下界（默认 180）",
    )
    parser.add_argument(
        "--gather-cost-max",
        type=int,
        default=260,
        help="Node 过快时 GatherCost 正常化上界（默认 260）",
    )
    parser.add_argument(
        "--first-touch-age-min",
        type=int,
        default=DEFAULT_FIRST_TOUCH_AGE_RANGE[0],
        help=(
            "首 touch 相对 TrackStartTime 的下界毫秒"
            f"（默认 {DEFAULT_FIRST_TOUCH_AGE_RANGE[0]}）"
        ),
    )
    parser.add_argument(
        "--first-touch-age-max",
        type=int,
        default=DEFAULT_FIRST_TOUCH_AGE_RANGE[1],
        help=(
            "首 touch 相对 TrackStartTime 的上界毫秒"
            f"（默认 {DEFAULT_FIRST_TOUCH_AGE_RANGE[1]}）"
        ),
    )
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
        prog="python -m ali_slider_reverse.cli",
        description="阿里 V3 滑块纯协议复现（不操作或刷新浏览器页面）",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    solve = subparsers.add_parser(
        "solve",
        help="设备链 + Init + 下载资源 + 识别，不发送 Verify",
    )
    _add_common_arguments(solve)
    solve.set_defaults(handler=_solve_command)

    run = subparsers.add_parser(
        "run",
        help="执行一轮 Init/识别/轨迹/data，并只发送一次 Verify",
    )
    _add_common_arguments(run)
    run.add_argument(
        "--fixture",
        help="可选：用户授权的 touch 轨迹 fixture",
    )
    run.add_argument(
        "--min-confidence",
        type=float,
        default=0.45,
        help="低于此图像置信度时停止且不发 Verify（默认 0.45）",
    )
    run.add_argument(
        "--submit-business",
        action="store_true",
        help="T001 后再签名并提交一次本地 gopay",
    )
    run.add_argument(
        "--capture",
        help="本地原始 CDP 抓包，用于读取 gopay body/会话模板",
    )
    run.add_argument(
        "--app-bundle",
        help="当前公开 app bundle，用于按哈希锁定业务签名尾串",
    )
    run.set_defaults(handler=_run_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except (
        AliSliderError,
        DeviceRuntimeError,
        PeRuntimeError,
        ValueError,
    ) as exc:
        _json_print(
            {
                "ok": False,
                "errorType": type(exc).__name__,
                "error": str(exc),
            },
            stream=sys.stderr,
        )
        return 1
    except Exception:  # pragma: no cover - CLI 的安全兜底。
        _json_print(
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
