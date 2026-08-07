"""``CaptchaVerifyParam.data`` 的纯 Python 正向封装与离线解包。

data 的封装层次（自内向外）：

```text
32-hex 前缀 + 紧凑 JSON
  → zlib/Deflate
  → Base64
  → 64-state 变换
  → Base64
```

31 个当前 PE 分片的受控差分已经证明：data key 一致，32-hex 前缀是 15 个随机
字节的十六进制加固定 ``01``，与 JSON 内容无关。因此不再下载或执行动态 PE。
"""

from __future__ import annotations

import base64
import binascii
import json
import re
import secrets
import zlib
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from ..errors import DataCodecError

PE091_DATA_KEY = "3e627e1b4c63f913"
"""当前分片 data 层 64-state 变换的密钥。"""

INITIAL_64_STATE: tuple[int, ...] = (
    32, 50, 10, 51,  6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
    60, 33, 53, 34,  7, 26, 12, 48,  5,  2, 20,  4, 61, 13, 47, 49,
    18, 29, 27, 22,  1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
     8, 57, 45, 35, 59, 36, 42, 54, 63,  3, 24, 28, 14,  9,  0, 21,
)
"""64-state 变换在进入 KSA 前使用的初始置换。

这是公开脚本中的固定常量，不包含会话标识、签名密钥或服务端秘密。
"""

DATA_PAYLOAD_FIELDS: tuple[str, ...] = (
    "TrackList",
    "TrackStartTime",
    "VerifyTime",
    "xPos",
    "slidePos",
    "arg",
)
"""data 明文顶层字段的固定顺序；序列化结果不可重排。"""

TRACK_LIST_FIELDS: tuple[str, ...] = (
    "mc",
    "tc",
    "mu",
    "te",
    "mp",
    "tmv",
    "mm",
    "ks",
    "fi",
    "startTime",
    "si",
)
"""``TrackList`` 的字段顺序，来自 e5 初始化链。

各事件流已由前端压平成字符串，``startTime`` 是唯一的数值成员。注意这个顺序
与 SDK 内部 tracker 对象的枚举顺序不同——以 ``JSON.stringify`` 前的顺序为准。
"""

_HEX32_RE = re.compile(r"^[0-9a-fA-F]{32}$")
_BASE64_RE = re.compile(r"^[A-Za-z0-9+/]*={0,2}$")


@dataclass(frozen=True, slots=True)
class DecodedData:
    """``unpack_data`` 的结构化结果。"""

    checksum_prefix: str
    """data 明文最前面的 32-hex 前缀（小写）。"""

    json_text: str
    """前缀之后的原样 JSON 文本。"""

    payload: dict[str, Any]
    """解析后的 JSON 对象，保留原始字段顺序。"""

    compressed_base64: str
    """zlib 层之外、64-state 层之内的 Base64 文本，便于逐层比对调试。"""


def keyed_state64(key: str) -> list[int]:
    """执行 64 项 KSA，返回可变状态副本。"""

    if not isinstance(key, str) or not key:
        raise DataCodecError("64-state key 必须是非空字符串")

    state = list(INITIAL_64_STATE)
    swap_index = 0
    for index in range(64):
        # 所有中间值都很小，Python 的 >> 与此处 JS 的有符号 >> 结果一致。
        swap_index = (
            ((index + swap_index + state[index] + state[swap_index]) >> 1)
            + ord(key[index % len(key)])
        ) & 63
        if index != swap_index:
            state[index], state[swap_index] = state[swap_index], state[index]
    return state


def transform64(
    data: bytes | bytearray | memoryview,
    key: str,
    *,
    decrypt: bool = False,
) -> bytes:
    """运行逐字节 64-state 变换。

    **这不是标准 RC4**。状态推进不依赖明/密文字节，但每字节混入了加减法与两次
    XOR，因此解密必须走显式逆公式——把加密函数再调用一次是还原不出明文的。
    """

    if not isinstance(key, str) or not key:
        raise DataCodecError("64-state key 必须是非空字符串")

    raw = bytes(data)
    state = keyed_state64(key)
    output = bytearray(len(raw))
    state_index = 0
    swap_index = 0

    for offset, source_byte in enumerate(raw):
        swap_index = (
            (state_index ^ swap_index) + (state[state_index] ^ state[swap_index])
        ) & 63
        if state_index != swap_index:
            state[state_index], state[swap_index] = (
                state[swap_index],
                state[state_index],
            )

        mix_a = state[state_index] + state[swap_index]
        mix_b = state[mix_a & 63]
        if decrypt:
            value = (
                (source_byte ^ mix_b ^ mix_a)
                - state_index
                - state[state_index]
                + swap_index
                + state[swap_index]
            ) & 0xFF
        else:
            value = (
                (
                    source_byte
                    + state_index
                    + state[state_index]
                    - swap_index
                    - state[swap_index]
                )
                ^ mix_a
                ^ mix_b
            ) & 0xFF
        output[offset] = value
        state_index = (state_index + 1) & 63

    return bytes(output)


def validate_checksum_prefix(value: str) -> str:
    """校验 32-hex 前缀的形状，返回其小写形式。

    只验形状：该前缀含随机性，重跑不会得到相同结果，因此没有可比对的
    期望值。
    """

    if not isinstance(value, str) or not _HEX32_RE.fullmatch(value):
        raise DataCodecError("data 前缀必须是 32 个十六进制字符")
    return value.lower()


def validate_payload_schema(payload: Mapping[str, Any]) -> None:
    """校验 data 明文对象的字段集合与各字段类型。

    只校验字段**集合**，不校验顺序：顺序由调用方按需单独断言（纯 Python PE
    会在 :mod:`ali_slider_reverse.runtime.pe` 中做严格顺序比对）。
    """

    if not isinstance(payload, Mapping):
        raise DataCodecError("data 明文必须是 JSON object")
    if set(payload) != set(DATA_PAYLOAD_FIELDS):
        raise DataCodecError(
            "data 明文字段必须恰好为 " + ", ".join(DATA_PAYLOAD_FIELDS)
        )

    track_list = payload["TrackList"]
    if not isinstance(track_list, Mapping):
        raise DataCodecError("TrackList 必须是 object")
    if set(track_list) != set(TRACK_LIST_FIELDS):
        raise DataCodecError(
            "TrackList 字段必须恰好为 " + ", ".join(TRACK_LIST_FIELDS)
        )

    for name in TRACK_LIST_FIELDS:
        value = track_list[name]
        if name == "startTime":
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise DataCodecError("TrackList.startTime 必须是数值")
        elif not isinstance(value, str):
            raise DataCodecError(f"TrackList.{name} 必须是压平字符串")

    for name in ("TrackStartTime", "VerifyTime"):
        value = payload[name]
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise DataCodecError(f"{name} 必须是数值")
    for name in ("xPos", "slidePos", "arg"):
        if not isinstance(payload[name], str):
            raise DataCodecError(f"{name} 必须是字符串")


def generate_data_prefix() -> str:
    """生成当前 PE 使用的 32-hex 前缀：``15 random bytes + 0x01``。"""

    return secrets.token_bytes(15).hex() + "01"


def pack_data(
    payload: Mapping[str, Any],
    *,
    key: str = PE091_DATA_KEY,
    prefix: str | None = None,
) -> str:
    """按当前 PE 的字节顺序正向封装 ``CaptchaVerifyParam.data``。

    ``payload`` 与 ``TrackList`` 的插入顺序均参与最终字节；调用方必须按模块常量
    给出的顺序构造。这里同时验证集合和顺序，防止生成能解包但服务端语义错误的串。
    """

    validate_payload_schema(payload)
    if tuple(payload) != DATA_PAYLOAD_FIELDS:
        raise DataCodecError("data 明文字段顺序异常")
    track_list = payload["TrackList"]
    if tuple(track_list) != TRACK_LIST_FIELDS:
        raise DataCodecError("TrackList 字段顺序异常")

    checked_prefix = validate_checksum_prefix(
        generate_data_prefix() if prefix is None else prefix
    )
    json_text = json.dumps(
        payload,
        ensure_ascii=False,
        separators=(",", ":"),
        allow_nan=False,
    )
    compressed = zlib.compress((checked_prefix + json_text).encode("utf-8"), 6)
    compressed_base64 = base64.b64encode(compressed)
    return base64.b64encode(transform64(compressed_base64, key)).decode("ascii")


def unpack_data(data: str, *, key: str = PE091_DATA_KEY) -> DecodedData:
    """逐层剥开 ``CaptchaVerifyParam.data``，不依赖浏览器或 Node。

    每一层都严格校验：外层 Base64 字母表、64-state 输出必须是 ASCII Base64、
    zlib 明文必须是 UTF-8、前缀必须是 32-hex、其后必须是符合 schema 的 JSON
    object。任何一层不符都抛 :class:`~ali_slider_reverse.errors.DataCodecError`。
    """

    if not isinstance(data, str):
        raise TypeError("data 必须是字符串")

    try:
        ciphertext = base64.b64decode(data, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise DataCodecError("data 外层不是有效 Base64") from exc

    try:
        compressed_base64 = transform64(ciphertext, key, decrypt=True).decode(
            "ascii"
        )
    except UnicodeDecodeError as exc:
        raise DataCodecError("64-state 解密结果不是 ASCII Base64") from exc
    if not _BASE64_RE.fullmatch(compressed_base64):
        raise DataCodecError("64-state 解密结果不符合 Base64 字母表")

    try:
        serialized = zlib.decompress(
            base64.b64decode(compressed_base64, validate=True)
        )
    except (binascii.Error, zlib.error, ValueError) as exc:
        raise DataCodecError("data 的 Base64/zlib 层无效") from exc
    try:
        serialized_text = serialized.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise DataCodecError("zlib 明文不是有效 UTF-8") from exc
    if len(serialized_text) < 32:
        raise DataCodecError("zlib 明文短于 32 字节前缀")

    checksum_prefix = validate_checksum_prefix(serialized_text[:32])
    json_text = serialized_text[32:]
    try:
        payload = json.loads(json_text)
    except json.JSONDecodeError as exc:
        raise DataCodecError("32-hex 前缀之后不是有效 JSON") from exc
    if not isinstance(payload, dict):
        raise DataCodecError("data JSON 顶层不是 object")
    validate_payload_schema(payload)

    return DecodedData(
        checksum_prefix=checksum_prefix,
        json_text=json_text,
        payload=payload,
        compressed_base64=compressed_base64,
    )


__all__ = [
    "DATA_PAYLOAD_FIELDS",
    "INITIAL_64_STATE",
    "PE091_DATA_KEY",
    "TRACK_LIST_FIELDS",
    "DecodedData",
    "keyed_state64",
    "generate_data_prefix",
    "pack_data",
    "transform64",
    "unpack_data",
    "validate_checksum_prefix",
    "validate_payload_schema",
]
