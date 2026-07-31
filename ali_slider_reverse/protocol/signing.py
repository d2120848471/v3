"""与浏览器行为等价的编码工具与阿里 RPC v1 签名。

本模块只做纯计算，不涉及任何 IO。这里的每个函数都在复刻一段具体的前端
JavaScript 语义——包括那些"不太标准"的边界行为。协议复现的正确性恰恰依赖
这些边界，所以宁可保留看起来奇怪的实现，也不要换成"更规范"的等价写法。

密钥不内置：``rpc_v1_signature`` 的 ``secret`` 必须由调用方针对当前前端版本
显式传入，本模块不保存任何抓包或动态脚本中的真实常量。
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import uuid
from collections.abc import Iterable, Mapping
from datetime import datetime, timezone
from typing import Any
from urllib.parse import quote


def compact_json(value: Any, *, ensure_ascii: bool = False) -> str:
    """返回与浏览器 ``JSON.stringify`` 外形一致的紧凑 JSON。

    Python 3.7+ 的 ``dict`` 保留插入顺序，因此调用方传入的字段顺序会原样保留
    ——这对参与签名或 checksum 的 payload 是硬要求。``allow_nan=False`` 可避免
    产生 JavaScript 与标准 JSON 无法可靠互通的 ``NaN``/``Infinity``。
    """

    return json.dumps(
        value,
        ensure_ascii=ensure_ascii,
        separators=(",", ":"),
        allow_nan=False,
    )


def js_encode_uri_component(value: str | bytes) -> str:
    """复刻 JavaScript ``encodeURIComponent`` 的 UTF-8 字符集合。

    保留字符集 ``-_.!~*'()`` 与 ECMAScript 规范一致，比 Python 默认的
    ``quote`` 少转义若干符号。
    """

    if isinstance(value, bytes):
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("encodeURIComponent 输入必须是 UTF-8") from exc
    if not isinstance(value, str):
        raise TypeError("encodeURIComponent 输入必须是 str 或 bytes")
    return quote(value, safe="-_.!~*'()", encoding="utf-8", errors="strict")


def js_form_urlencode(
    fields: Mapping[str, Any] | Iterable[tuple[str, Any]],
) -> str:
    """按插入顺序复刻前端 ``Qe`` 构造的 form body。

    它逐项执行 ``encodeURIComponent(key)`` 和 ``encodeURIComponent(value)``，
    因此空格是 ``%20`` 而不是 HTML 表单的 ``+``。字段顺序必须与签名时一致，
    所以这里不排序。
    """

    items = list(fields.items()) if isinstance(fields, Mapping) else list(fields)
    return "&".join(
        f"{js_encode_uri_component(str(key))}="
        f"{js_encode_uri_component(str(value))}"
        for key, value in items
    )


def rpc_percent_encode(value: str | bytes) -> str:
    """精确复刻当前前端 ``Ae`` 的 percent-encoding。

    动态包先执行 ``encodeURIComponent(value)``，再依次调用三次**非正则**的
    ``replace``：``"+"→"%20"``、``"*"→"%2A"``、``"%7E"→"~"``。

    两处与"严格 RFC 3986"的差异必须保留，否则签名对不上：

    1. ``!'()`` 保持原样，不做二次转义；
    2. 非正则 ``replace`` 只替换**第一个**匹配项——字符串中第二个及以后的
       ``*`` 不会变成 ``%2A``。
    """

    if isinstance(value, bytes):
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("RPC 参数必须是有效 UTF-8") from exc
    if not isinstance(value, str):
        raise TypeError("RPC 参数键和值必须是 str 或 UTF-8 bytes")
    encoded = quote(value, safe="-_.!~*'()", encoding="utf-8", errors="strict")
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

    # 排序发生在 percent-encode **之前**，按原始 key 的码点序。
    items.sort(key=lambda item: item[0])
    return "&".join(
        f"{rpc_percent_encode(key)}={rpc_percent_encode(value)}"
        for key, value in items
    )


def rpc_v1_string_to_sign(params: Mapping[str, str | bytes]) -> str:
    """构造固定 ``POST /`` 的阿里 RPC v1 StringToSign。"""

    return "POST&%2F&" + rpc_percent_encode(rpc_v1_canonical_query(params))


def rpc_v1_signature(
    params: Mapping[str, str | bytes],
    *,
    secret: str | bytes,
) -> str:
    """计算阿里 RPC v1 的 HMAC-SHA1 + 标准 Base64 签名。

    签名密钥严格为 ``secret + "&"``——尾部这个 ``&`` 是 RPC v1 的规定，漏掉会
    得到一个格式合法但服务端拒绝的签名。
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


def utc_timestamp(value: datetime | None = None) -> str:
    """生成阿里 OpenAPI 的秒级 UTC 时间戳，例如 ``2026-07-30T02:09:28Z``。

    未传值时使用当前 UTC 时间；无时区的 ``datetime`` 按 UTC 解释，避免结果
    随运行机器的本地时区漂移。
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
    """生成带连字符的小写 UUID v4，用于 RPC ``SignatureNonce``。"""

    return str(uuid.uuid4())


def uuid4_hex_nonce() -> str:
    """生成业务签名使用的 32 位、无连字符 UUID v4 nonce。"""

    return uuid.uuid4().hex


__all__ = [
    "compact_json",
    "js_encode_uri_component",
    "js_form_urlencode",
    "rpc_percent_encode",
    "rpc_v1_canonical_query",
    "rpc_v1_signature",
    "rpc_v1_string_to_sign",
    "utc_timestamp",
    "uuid4_hex_nonce",
    "uuid4_nonce",
]
