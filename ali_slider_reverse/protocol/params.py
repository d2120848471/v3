"""验证码与业务请求的参数封装。

对应抓包中已经确认的两层明文封装：

* ``VerifyCaptchaV3.CaptchaVerifyParam`` —— 紧凑 JSON；
* 业务接口的 ``captchaVerifyParam`` —— 标准 Base64(紧凑 JSON)；
* 业务接口的 ``lgsign`` —— ``SHA512(query + body + token + salt)``。

字段顺序全部来自抓包，不可重排：这些串会直接参与后续签名与服务端校验。
动态脚本生成的 ``data`` 不在此处构造，见 :mod:`.data_codec`。
"""

from __future__ import annotations

import base64
import hashlib
from collections.abc import Mapping
from typing import Any

from .signing import compact_json


def _require_nonempty_string(
    values: Mapping[str, Any],
    key: str,
    *,
    label: str,
) -> str:
    item = values[key]
    if not isinstance(item, str) or not item:
        raise ValueError(f"{label}.{key} 必须是非空字符串")
    return item


def build_verify_captcha_param(
    *,
    scene_id: str,
    certify_id: str,
    device_token: str,
    data: str,
) -> str:
    """组装 ``VerifyCaptchaV3.CaptchaVerifyParam`` 的紧凑 JSON。

    字段顺序来自本地抓包：``sceneId``、``certifyId``、``deviceToken``、``data``。
    """

    values = {
        "sceneId": scene_id,
        "certifyId": certify_id,
        "deviceToken": device_token,
        "data": data,
    }
    for key in values:
        _require_nonempty_string(values, key, label="CaptchaVerifyParam")
    return compact_json(values)


def build_business_captcha_verify_param(
    *,
    certify_id: str,
    scene_id: str,
    security_token: str,
    is_sign: bool = True,
) -> str:
    """组装业务请求 ``captchaVerifyParam`` 的标准 Base64(JSON)。"""

    values: dict[str, Any] = {
        "certifyId": certify_id,
        "sceneId": scene_id,
        "isSign": is_sign,
        "securityToken": security_token,
    }
    for key in ("certifyId", "sceneId", "securityToken"):
        _require_nonempty_string(values, key, label="captchaVerifyParam")
    if not isinstance(is_sign, bool):
        raise TypeError("captchaVerifyParam.isSign 必须是 bool")

    return base64.b64encode(compact_json(values).encode("utf-8")).decode("ascii")


def _business_sha512_signature(
    *,
    canonical_query: str,
    body_text: str,
    raw_token: str,
    salt: str,
) -> str:
    """复刻业务端 ``SHA512(query + body + token + salt)``。

    四段按此顺序**直接拼接**，中间没有任何分隔符。
    """

    preimage = canonical_query + body_text + raw_token + salt
    return hashlib.sha512(preimage.encode("utf-8")).hexdigest()


def build_business_signed_query(
    *,
    body_text: str,
    raw_token: str,
    salt: str,
    lgtime: int,
    lgnonce: str,
) -> tuple[str, str]:
    """返回 ``(canonical query, 带 lgsign 的最终 query)``。

    canonical query 固定为 ``lgnonce=...&lgtime=...``（按 key 字典序），它既是
    签名原文的第一段，也是最终 URL query 的前缀。
    """

    if isinstance(lgtime, bool) or not isinstance(lgtime, int) or lgtime < 0:
        raise ValueError("lgtime 必须是非负整数毫秒")
    if (
        not isinstance(lgnonce, str)
        or len(lgnonce) != 32
        or any(character not in "0123456789abcdef" for character in lgnonce)
    ):
        raise ValueError("lgnonce 必须是 32 位小写十六进制")
    for label, value in (
        ("body_text", body_text),
        ("raw_token", raw_token),
        ("salt", salt),
    ):
        if not isinstance(value, str):
            raise TypeError(f"{label} 必须是字符串")

    canonical = f"lgnonce={lgnonce}&lgtime={lgtime}"
    signature = _business_sha512_signature(
        canonical_query=canonical,
        body_text=body_text,
        raw_token=raw_token,
        salt=salt,
    )
    return canonical, f"{canonical}&lgsign={signature}"


__all__ = [
    "build_business_captcha_verify_param",
    "build_business_signed_query",
    "build_verify_captcha_param",
]
