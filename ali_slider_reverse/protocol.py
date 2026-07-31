"""阿里 V3 滑块外围协议的纯 Python 编解码工具。

本模块只负责抓包中已经确认的明文封装层：

* 有序 ``application/x-www-form-urlencoded``；
* 紧凑 JSON；
* ``VerifyCaptchaV3`` 的 ``CaptchaVerifyParam`` JSON；
* 业务接口的 ``captchaVerifyParam`` Base64(JSON)；
* UTC 时间戳和 UUID v4 nonce；
* 当前前端 SDK 的 RPC v1 canonicalization 与 HMAC-SHA1 签名。

动态脚本生成的 ``data`` 不在此处猜测；``deviceToken`` 由 :mod:`device` 负责。
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import uuid
from collections.abc import Iterable, Mapping, Sequence
from datetime import datetime, timezone
from typing import Any
from urllib.parse import parse_qsl, quote, quote_plus, urlencode


FormScalar = str | bytes | int | float | bool | None
FormValue = FormScalar | Sequence[FormScalar]
FormFields = Mapping[str, FormValue] | Iterable[tuple[str, FormValue]]

VERIFY_PARAM_KEYS = ("sceneId", "certifyId", "deviceToken", "data")
BUSINESS_PARAM_KEYS = ("certifyId", "sceneId", "isSign", "securityToken")


def compact_json(value: Any, *, ensure_ascii: bool = False) -> str:
    """返回与浏览器 ``JSON.stringify`` 外形一致的紧凑 JSON。

    Python 3.7+ 的 ``dict`` 保留插入顺序，因此调用方传入的字段顺序会原样保留。
    ``allow_nan=False`` 可避免产生 JavaScript/标准 JSON 无法可靠互通的 NaN/Infinity。
    """

    return json.dumps(
        value,
        ensure_ascii=ensure_ascii,
        separators=(",", ":"),
        allow_nan=False,
    )


def form_urlencode(
    fields: FormFields,
    *,
    doseq: bool = False,
    space_as_plus: bool = True,
    safe: str = "",
) -> str:
    """按输入顺序编码 form-urlencoded，不擅自排序字段。

    ``Mapping`` 使用其插入顺序；需要重复键时传入二元组序列。默认采用 HTML 表单的
    ``+`` 空格规则；若签名前的前端证据显示使用 ``encodeURIComponent`` 风格，可设置
    ``space_as_plus=False`` 得到 ``%20``。
    """

    items = list(fields.items()) if isinstance(fields, Mapping) else list(fields)
    quote_via = quote_plus if space_as_plus else quote
    return urlencode(items, doseq=doseq, safe=safe, quote_via=quote_via)


def js_encode_uri_component(value: str | bytes) -> str:
    """复刻 JavaScript ``encodeURIComponent`` 的 UTF-8 字符集合。"""

    if isinstance(value, bytes):
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("encodeURIComponent 输入必须是 UTF-8") from exc
    if not isinstance(value, str):
        raise TypeError("encodeURIComponent 输入必须是 str 或 bytes")
    return quote(value, safe="-_.!~*'()", encoding="utf-8", errors="strict")


def js_form_urlencode(fields: Mapping[str, Any] | Iterable[tuple[str, Any]]) -> str:
    """按插入顺序复刻前端 ``Qe`` 的 form body。

    它逐项执行 ``encodeURIComponent(key/value)``，不会把空格编码成 ``+``。
    """

    items = list(fields.items()) if isinstance(fields, Mapping) else list(fields)
    return "&".join(
        f"{js_encode_uri_component(str(key))}="
        f"{js_encode_uri_component(str(value))}"
        for key, value in items
    )


def parse_form_urlencoded(value: str | bytes) -> list[tuple[str, str]]:
    """解析 form-urlencoded，同时保留字段顺序、重复键和空值。"""

    if isinstance(value, bytes):
        try:
            value = value.decode("ascii")
        except UnicodeDecodeError as exc:
            raise ValueError("form-urlencoded 原文必须是 ASCII") from exc
    if not isinstance(value, str):
        raise TypeError("form-urlencoded 原文必须是 str 或 bytes")
    return parse_qsl(value, keep_blank_values=True, strict_parsing=False)


# 名称直接体现“有序”，便于协议层调用者发现该接口。
ordered_form_urlencode = form_urlencode


def rpc_percent_encode(value: str | bytes) -> str:
    """精确复刻当前前端 ``Ae`` 的 percent-encoding。

    动态包执行 ``encodeURIComponent(value)``，然后依次调用三次非正则
    ``replace("+", "%20")``、``replace("*", "%2A")``、
    ``replace("%7E", "~")``。因此 ``!'()`` 保持原样，而且字符串中只有
    第一个 ``*`` 会被替换；这个边界行为与“严格 RFC3986”并不完全相同。
    """

    if isinstance(value, bytes):
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("RPC 参数必须是有效 UTF-8") from exc
    if not isinstance(value, str):
        raise TypeError("RPC 参数键和值必须是 str 或 UTF-8 bytes")
    encoded = quote(
        value,
        safe="-_.!~*'()",
        encoding="utf-8",
        errors="strict",
    )
    return (
        encoded.replace("+", "%20", 1)
        .replace("*", "%2A", 1)
        .replace("%7E", "~", 1)
    )


def rpc_v1_canonical_query(params: Mapping[str, str | bytes]) -> str:
    """排除旧 ``Signature``、按原始 key 排序并生成 canonical query。"""

    if not isinstance(params, Mapping):
        raise TypeError("params 必须是 mapping")
    items: list[tuple[str, str | bytes]] = []
    for key, value in params.items():
        if not isinstance(key, str):
            raise TypeError("RPC 参数键必须是 str")
        if key == "Signature":
            continue
        if not isinstance(value, (str, bytes)):
            raise TypeError(f"RPC 参数 {key!r} 的值必须是 str 或 bytes")
        items.append((key, value))

    items.sort(key=lambda item: item[0])
    return "&".join(
        f"{rpc_percent_encode(key)}={rpc_percent_encode(value)}"
        for key, value in items
    )


def rpc_v1_string_to_sign(params: Mapping[str, str | bytes]) -> str:
    """构造固定 ``POST /`` 的阿里 RPC v1 StringToSign。"""

    canonical = rpc_v1_canonical_query(params)
    return "POST&%2F&" + rpc_percent_encode(canonical)


def rpc_v1_signature(
    params: Mapping[str, str | bytes],
    *,
    secret: str | bytes,
) -> str:
    """计算阿里 RPC v1 的 HMAC-SHA1 + 标准 Base64 签名。

    签名密钥严格为 ``secret + '&'``。``secret`` 必须由调用方针对当前前端版本
    显式传入，本模块不内置任何抓包或动态脚本中的真实常量。
    """

    if isinstance(secret, str):
        secret_bytes = secret.encode("utf-8")
    elif isinstance(secret, bytes):
        secret_bytes = secret
    else:
        raise TypeError("secret 必须是 str 或 bytes")
    if not secret_bytes:
        raise ValueError("secret 不能为空")

    string_to_sign = rpc_v1_string_to_sign(params).encode("utf-8")
    digest = hmac.new(secret_bytes + b"&", string_to_sign, hashlib.sha1).digest()
    return base64.b64encode(digest).decode("ascii")


# 显式别名，强调这是阿里 RPC v1，而非业务侧自定义签名。
aliyun_rpc_v1_signature = rpc_v1_signature


def _decode_json_object(value: str | bytes, *, label: str) -> dict[str, Any]:
    if isinstance(value, bytes):
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError(f"{label} 不是有效的 UTF-8") from exc
    if not isinstance(value, str):
        raise TypeError(f"{label} 必须是 str 或 bytes")

    try:
        decoded = json.loads(value)
    except json.JSONDecodeError as exc:
        raise ValueError(f"{label} 不是有效 JSON: {exc.msg}") from exc
    if not isinstance(decoded, dict):
        raise ValueError(f"{label} 顶层必须是 JSON object")
    return decoded


def _require_exact_keys(
    value: Mapping[str, Any],
    expected: tuple[str, ...],
    *,
    label: str,
) -> None:
    missing = [key for key in expected if key not in value]
    extras = [key for key in value if key not in expected]
    if missing or extras:
        details: list[str] = []
        if missing:
            details.append(f"缺少字段 {missing}")
        if extras:
            details.append(f"存在未知字段 {extras}")
        raise ValueError(f"{label} 字段不匹配：" + "；".join(details))


def _require_nonempty_string(value: Mapping[str, Any], key: str, *, label: str) -> str:
    item = value[key]
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
    """组装 ``VerifyCaptchaV3.CaptchaVerifyParam`` 的紧凑 JSON 字符串。

    字段顺序来自本地前端抓包：``sceneId``、``certifyId``、``deviceToken``、``data``。
    """

    values = {
        "sceneId": scene_id,
        "certifyId": certify_id,
        "deviceToken": device_token,
        "data": data,
    }
    for key in VERIFY_PARAM_KEYS:
        _require_nonempty_string(values, key, label="CaptchaVerifyParam")
    return compact_json(values)


def parse_verify_captcha_param(value: str | bytes) -> dict[str, str]:
    """解析并校验 ``VerifyCaptchaV3.CaptchaVerifyParam``。

    返回值重新按抓包确认的字段顺序构造，适合再次紧凑序列化或参与后续调试比对。
    """

    decoded = _decode_json_object(value, label="CaptchaVerifyParam")
    _require_exact_keys(decoded, VERIFY_PARAM_KEYS, label="CaptchaVerifyParam")
    return {
        key: _require_nonempty_string(decoded, key, label="CaptchaVerifyParam")
        for key in VERIFY_PARAM_KEYS
    }


def build_business_captcha_verify_param(
    *,
    certify_id: str,
    scene_id: str,
    security_token: str,
    is_sign: bool = True,
) -> str:
    """组装业务请求 ``captchaVerifyParam`` 的标准 Base64(JSON) 字符串。"""

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

    payload = compact_json(values).encode("utf-8")
    return base64.b64encode(payload).decode("ascii")


def parse_business_captcha_verify_param(value: str | bytes) -> dict[str, Any]:
    """解析并严格校验业务请求中的标准 Base64(JSON) 参数。"""

    if isinstance(value, str):
        try:
            encoded = value.encode("ascii")
        except UnicodeEncodeError as exc:
            raise ValueError("captchaVerifyParam Base64 必须是 ASCII") from exc
    elif isinstance(value, bytes):
        encoded = value
    else:
        raise TypeError("captchaVerifyParam 必须是 str 或 bytes")

    # 抓包使用标准 Base64；允许传入省略尾部 padding 的等价文本，仍严格拒绝脏字符。
    encoded += b"=" * (-len(encoded) % 4)
    try:
        payload = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise ValueError("captchaVerifyParam 不是有效的标准 Base64") from exc

    decoded = _decode_json_object(payload, label="captchaVerifyParam")
    _require_exact_keys(decoded, BUSINESS_PARAM_KEYS, label="captchaVerifyParam")
    result: dict[str, Any] = {
        "certifyId": _require_nonempty_string(
            decoded, "certifyId", label="captchaVerifyParam"
        ),
        "sceneId": _require_nonempty_string(
            decoded, "sceneId", label="captchaVerifyParam"
        ),
        "isSign": decoded["isSign"],
        "securityToken": _require_nonempty_string(
            decoded, "securityToken", label="captchaVerifyParam"
        ),
    }
    if not isinstance(result["isSign"], bool):
        raise ValueError("captchaVerifyParam.isSign 必须是 bool")
    return result


def utc_timestamp(value: datetime | None = None) -> str:
    """生成阿里 OpenAPI 常见的秒级 UTC 时间戳，例如 ``2026-07-30T02:09:28Z``。

    未传值时使用当前 UTC 时间；无时区的 ``datetime`` 按 UTC 解释，避免受本机时区影响。
    """

    moment = datetime.now(timezone.utc) if value is None else value
    if not isinstance(moment, datetime):
        raise TypeError("value 必须是 datetime 或 None")
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    else:
        moment = moment.astimezone(timezone.utc)
    return moment.replace(microsecond=0).isoformat().replace("+00:00", "Z")


def uuid4_nonce() -> str:
    """生成带连字符、小写的 RFC 4122 UUID v4，可用于 ``SignatureNonce``。"""

    return str(uuid.uuid4())


def uuid4_hex_nonce() -> str:
    """生成前端业务签名使用的 32 位、无连字符 UUID v4 nonce。"""

    return uuid.uuid4().hex


def business_sha512_signature(
    *,
    canonical_query: str,
    body_text: str,
    raw_token: str,
    salt: str,
) -> str:
    """复刻业务端 ``SHA512(query + body + token + salt)``。"""

    for label, value in (
        ("canonical_query", canonical_query),
        ("body_text", body_text),
        ("raw_token", raw_token),
        ("salt", salt),
    ):
        if not isinstance(value, str):
            raise TypeError(f"{label} 必须是字符串")
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
    """返回 ``canonical query`` 与包含 ``lgsign`` 的最终 query。"""

    if isinstance(lgtime, bool) or not isinstance(lgtime, int) or lgtime < 0:
        raise ValueError("lgtime 必须是非负整数毫秒")
    if (
        not isinstance(lgnonce, str)
        or len(lgnonce) != 32
        or any(character not in "0123456789abcdef" for character in lgnonce)
    ):
        raise ValueError("lgnonce 必须是 32 位小写十六进制")
    canonical = f"lgnonce={lgnonce}&lgtime={lgtime}"
    signature = business_sha512_signature(
        canonical_query=canonical,
        body_text=body_text,
        raw_token=raw_token,
        salt=salt,
    )
    return canonical, f"{canonical}&lgsign={signature}"


__all__ = [
    "BUSINESS_PARAM_KEYS",
    "VERIFY_PARAM_KEYS",
    "aliyun_rpc_v1_signature",
    "build_business_captcha_verify_param",
    "build_verify_captcha_param",
    "compact_json",
    "form_urlencode",
    "js_encode_uri_component",
    "js_form_urlencode",
    "ordered_form_urlencode",
    "parse_business_captcha_verify_param",
    "parse_form_urlencoded",
    "parse_verify_captcha_param",
    "rpc_percent_encode",
    "rpc_v1_canonical_query",
    "rpc_v1_signature",
    "rpc_v1_string_to_sign",
    "utc_timestamp",
    "uuid4_nonce",
    "uuid4_hex_nonce",
    "business_sha512_signature",
    "build_business_signed_query",
]
