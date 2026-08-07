"""可选的业务请求提交。

这是整条链路里唯一一处**产生外部副作用**的动作，因此门禁最严：只有在验证码
返回 ``T001`` + ``VerifyResult=true`` + 非空 ``securityToken`` 时才允许调用，
且每轮最多提交一次。

原始抓包与当前 app bundle 都不属于源码，也不保留在仓库里——它们由使用者从受控
路径显式提供，读取后只在内存中流转。
"""

from __future__ import annotations

import hashlib
import json
import re
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit, urlunsplit

from .. import config
from ..errors import AliSliderError
from ..protocol.params import (
    build_business_captcha_verify_param,
    build_business_signed_query,
)
from ..protocol.signing import compact_json, uuid4_hex_nonce

# 抓包里需要还原的请求头，映射到规范大小写。
_CANONICAL_HEADER_NAMES = {
    "accept": "Accept",
    "accept-language": "Accept-Language",
    "authorization": "Authorization",
    "clientid": "clientid",
    "content-type": "Content-Type",
    "origin": "Origin",
    "referer": "Referer",
    "user-agent": "User-Agent",
}


@dataclass(frozen=True, slots=True)
class BusinessRequestTemplate:
    """从本地抓包还原出的一次业务请求模板。"""

    base_url: str
    body: dict[str, Any] = field(repr=False)
    raw_token: str = field(repr=False)
    headers: dict[str, str] = field(repr=False)
    cookie: str = field(repr=False)


@dataclass(frozen=True, slots=True)
class BusinessResponse:
    """业务接口的响应；``body`` 可能是已解析的 JSON 或原始文本。"""

    status_code: int
    body: Any = field(repr=False)


def extract_business_sign_salt(bundle_path: str | Path) -> str:
    """从当前公开 app bundle 中提取业务 SHA-512 签名尾串。

    用**预先离线算好的 SHA-256** 来锁定目标字面量，而不是把明文常量写进源码。
    正则先按任意短字面量成对匹配再做长度过滤：如果把最小长度直接写进正则，
    相邻的两个短字符串会被错误地从前一个右引号一路匹配到后一个左引号。
    """

    try:
        source = Path(bundle_path).read_text(encoding="utf-8")
    except OSError as exc:
        raise AliSliderError(f"无法读取 app bundle：{bundle_path}") from exc

    for match in re.finditer(r"""(["'])([^"'\\\r\n]{0,256})\1""", source):
        candidate = match.group(2)
        if not 16 <= len(candidate) <= 64:
            continue
        digest = hashlib.sha256(candidate.encode("utf-8")).hexdigest()
        if digest == config.BUSINESS_SALT_SHA256:
            return candidate
    raise AliSliderError("未在当前 app bundle 找到已确认的业务签名尾串")


def load_business_template_from_capture(
    capture_path: str | Path,
) -> BusinessRequestTemplate:
    """从本地原始 CDP 抓包中读取一次业务请求模板。

    CDP 把请求头拆在两个事件里：``Network.requestWillBeSent`` 带常规头，
    ``requestWillBeSentExtraInfo`` 才带 Cookie 等受限头，因此需要按 ``requestId``
    关联后合并。
    """

    try:
        document = json.loads(Path(capture_path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AliSliderError(f"无法读取抓包文件：{capture_path}") from exc

    events = document.get("postSlider", {}).get("events", [])
    request_event = next(
        (
            event
            for event in events
            if event.get("method") == "Network.requestWillBeSent"
            and "/mall-toc/order/gopay"
            in str(event.get("params", {}).get("request", {}).get("url", ""))
        ),
        None,
    )
    if request_event is None:
        raise AliSliderError("抓包中没有 gopay 请求")

    params = request_event["params"]
    request = params["request"]
    request_id = params.get("requestId")

    parsed_url = urlsplit(request["url"])
    if parsed_url.hostname not in {"localhost", "127.0.0.1"}:
        raise AliSliderError("业务模板不是本地 CTF 地址")
    base_url = urlunsplit(
        (parsed_url.scheme, parsed_url.netloc, parsed_url.path, "", "")
    )

    try:
        body = json.loads(request["postData"])
    except (KeyError, json.JSONDecodeError) as exc:
        raise AliSliderError("gopay 模板 body 不是 JSON") from exc
    if not isinstance(body, dict):
        raise AliSliderError("gopay 模板 body 顶层必须是 object")

    raw_headers = request.get("headers", {})
    normalized_headers = {
        str(key).lower(): str(value)
        for key, value in (raw_headers if isinstance(raw_headers, dict) else {}).items()
    }
    authorization = normalized_headers.get("authorization", "")
    if not authorization.startswith("Bearer "):
        raise AliSliderError("gopay 模板缺少 Bearer Authorization")

    extra_headers, cookie = _extra_headers_for(events, request_id)
    merged = extra_headers | normalized_headers
    return BusinessRequestTemplate(
        base_url=base_url,
        body=body,
        raw_token=authorization[len("Bearer ") :],
        headers={
            canonical: merged[key]
            for key, canonical in _CANONICAL_HEADER_NAMES.items()
            if key in merged
        },
        cookie=cookie,
    )


def _extra_headers_for(
    events: list[dict[str, Any]],
    request_id: Any,
) -> tuple[dict[str, str], str]:
    """取出同一 ``requestId`` 的受限请求头与 Cookie。"""

    for event in events:
        if event.get("method") != "Network.requestWillBeSentExtraInfo":
            continue
        event_params = event.get("params", {})
        if event_params.get("requestId") != request_id:
            continue
        values = event_params.get("headers", {})
        if not isinstance(values, dict):
            break
        headers = {
            str(key).lower(): str(value) for key, value in values.items()
        }
        return headers, headers.get("cookie", "")
    return {}, ""


def submit_business(
    http_session: Any,
    template: BusinessRequestTemplate,
    *,
    certify_id: str,
    scene_id: str,
    security_token: str,
    app_bundle: str | Path,
    timeout: float,
) -> BusinessResponse:
    """按模板重算签名并提交一次业务请求。

    调用方必须先确认验证码已通过——本函数不重复做该判断，只负责把 body、时间、
    nonce 与 SHA-512 签名组装正确。
    """

    body = dict(template.body)
    body["captchaVerifyParam"] = build_business_captcha_verify_param(
        certify_id=certify_id,
        scene_id=scene_id,
        security_token=security_token,
        is_sign=True,
    )
    body_text = compact_json(body)

    _, query = build_business_signed_query(
        body_text=body_text,
        raw_token=template.raw_token,
        salt=extract_business_sign_salt(app_bundle),
        lgtime=int(time.time() * 1000),
        lgnonce=uuid4_hex_nonce(),
    )

    headers = dict(template.headers)
    headers["Content-Type"] = "application/json"
    if template.cookie:
        headers["Cookie"] = template.cookie

    try:
        response = http_session.post(
            template.base_url + "?" + query,
            data=body_text.encode("utf-8"),
            headers=headers,
            timeout=timeout,
        )
    except Exception as exc:
        raise AliSliderError("本地业务请求失败") from exc

    try:
        response_body: Any = response.json()
    except ValueError:
        response_body = response.text
    return BusinessResponse(
        status_code=response.status_code, body=response_body
    )


__all__ = [
    "BusinessRequestTemplate",
    "BusinessResponse",
    "extract_business_sign_salt",
    "load_business_template_from_capture",
    "submit_business",
]
