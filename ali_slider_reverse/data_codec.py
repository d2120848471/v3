"""Ali Captcha V3 ``CaptchaVerifyParam.data`` 的离线编解码。

本模块复现 ``dynamicJS/3.29.0/pe.091`` 中已经静态确认的几层：

``prefix32 + compact JSON -> zlib/Deflate -> Base64 -> 64-state -> Base64``

其中 64-state、``arg``、zlib 与 Base64 都可以纯 Python 完成。32 字节
十六进制前缀是另一个、随前端分片变化的 VM 程序；请通过同目录的
``checksum_bridge.mjs`` 对明确指定的公开 ``pe.*.js`` 文件求值，不能把
pe.091 的 prefix VM 结果硬套到 pe.094 等其他分片。

注意：进一步的受控执行证明该 VM 输出含随机性；固定 ``Math.random`` 后，
输出不随 JSON 改变。因此这里把参考资料中的“checksum”保留为兼容 API 名，
但不再把它描述成 MD5/完整性校验。它是 32-hex 前缀，是否包含版本或环境位
必须按当前 ``pe`` 分片判断。
"""

from __future__ import annotations

import base64
import binascii
import json
import re
import subprocess
import zlib
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any


PE091_DATA_KEY = "3e627e1b4c63f913"
PE091_ARG_KEY = "vlhktejv0epzft2o"
PREFIX_VM_SEED = "0000"
# 兼容最初按参考笔记命名的调用方；语义以 PREFIX_VM_SEED 为准。
CHECKSUM_VM_SEED = PREFIX_VM_SEED

# L/F VM 在进入 KSA 前压入的 64 项初始置换。它是 pe.091 公开脚本中的
# 固定常量，不包含会话标识、签名密钥或服务端秘密。
INITIAL_64_STATE: tuple[int, ...] = (
    32,
    50,
    10,
    51,
    6,
    44,
    37,
    16,
    46,
    11,
    62,
    19,
    43,
    25,
    23,
    30,
    60,
    33,
    53,
    34,
    7,
    26,
    12,
    48,
    5,
    2,
    20,
    4,
    61,
    13,
    47,
    49,
    18,
    29,
    27,
    22,
    1,
    17,
    39,
    56,
    41,
    38,
    55,
    31,
    15,
    58,
    52,
    40,
    8,
    57,
    45,
    35,
    59,
    36,
    42,
    54,
    63,
    3,
    24,
    28,
    14,
    9,
    0,
    21,
)

DATA_PAYLOAD_FIELDS: tuple[str, ...] = (
    "TrackList",
    "TrackStartTime",
    "VerifyTime",
    "xPos",
    "slidePos",
    "arg",
)

# TrackList 的插入顺序来自 e5 初始化链。各事件流已经由前端压平成字符串；
# startTime 是唯一的数值成员。
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

TRACK_EVENT_FIELDS: dict[str, tuple[str, ...]] = {
    "mc": ("x", "y", "time", "button", "isTrusted"),
    "tc": ("x", "y", "time", "isTrusted"),
    "mu": ("x", "y", "time", "button", "isTrusted"),
    "te": ("x", "y", "time", "isTrusted"),
    "mp": ("x", "y", "time", "isTrusted"),
    "tmv": ("x", "y", "time", "isTrusted"),
    "mm": ("x", "y", "time", "isTrusted"),
    "ks": ("time", "key", "isTrusted"),
    "fi": ("time", "type", "isTrusted"),
    "si": (
        "innerWidth",
        "screenWidth",
        "innerHeight",
        "clientWidth",
        "clientWidthHeight",
        "outerHeight",
        "screenHeight",
        "fps",
        "outerWidth",
    ),
}

_CHECKSUM_RE = re.compile(r"^[0-9a-fA-F]{32}$")
_BASE64_RE = re.compile(r"^[A-Za-z0-9+/]*={0,2}$")


class DataCodecError(ValueError):
    """data 层格式、编码或 schema 不符合当前协议。"""


@dataclass(frozen=True, slots=True)
class DecodedData:
    """``unpack_data`` 的结构化结果。

    ``checksum_valid`` 是历史兼容字段：只有在调用方提供期望前缀或版本化
    prefix 函数时才有
    布尔值；未提供时为 ``None``，避免把“格式正确”误报成“校验正确”。
    """

    checksum_prefix: str
    json_text: str
    payload: dict[str, Any]
    compressed_base64: str
    checksum_valid: bool | None


def _require_key(key: str) -> None:
    if not isinstance(key, str) or not key:
        raise DataCodecError("64-state key 必须是非空字符串")


def keyed_state64(key: str) -> list[int]:
    """执行 pe.091 的 64 项 KSA，返回可变状态副本。"""

    _require_key(key)
    state = list(INITIAL_64_STATE)
    swap_index = 0
    for index in range(64):
        # 所有中间值均很小；Python 的 >> 与这里的 JS 有符号 >> 结果一致。
        swap_index = (
            (
                (
                    index
                    + swap_index
                    + state[index]
                    + state[swap_index]
                )
                >> 1
            )
            + ord(key[index % len(key)])
        ) & 63
        if index != swap_index:
            state[index], state[swap_index] = (
                state[swap_index],
                state[index],
            )
    return state


def transform64(
    data: bytes | bytearray | memoryview,
    key: str,
    *,
    decrypt: bool = False,
) -> bytes:
    """运行 pe.091 的逐字节 64-state 变换。

    该变换不是标准 RC4。状态推进不依赖明/密文字节，但每字节包含加减法与
    两次 XOR，因此解密必须使用显式逆公式，不能把加密函数再调用一次。
    """

    _require_key(key)
    raw = bytes(data)
    state = keyed_state64(key)
    output = bytearray(len(raw))
    state_index = 0
    swap_index = 0

    for offset, source_byte in enumerate(raw):
        swap_index = (
            (state_index ^ swap_index)
            + (state[state_index] ^ state[swap_index])
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


def encode64_text(text: str, key: str) -> str:
    """按前端 UTF-8 -> 64-state -> Base64 的顺序编码文本。"""

    if not isinstance(text, str):
        raise TypeError("text 必须是字符串")
    transformed = transform64(text.encode("utf-8"), key)
    return base64.b64encode(transformed).decode("ascii")


def decode64_text(encoded: str, key: str) -> str:
    """逆向 ``encode64_text``，并严格校验外层 Base64 与 UTF-8。"""

    try:
        ciphertext = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise DataCodecError("64-state 外层不是有效 Base64") from exc
    plaintext = transform64(ciphertext, key, decrypt=True)
    try:
        return plaintext.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise DataCodecError("64-state 解密结果不是有效 UTF-8") from exc


def build_arg(
    plaintext: str,
    *,
    key: str,
) -> str:
    """生成最终 JSON 的 ``arg``。

    当前动态 PE 的调用点是
    ``tx(config.ACCESS_SEC, config.SESSION_ID_SALT)``，而 ``tx`` 的参数顺序
    是 ``key, plaintext``。两个值都必须来自本轮加载的公开 SDK，不能把
    CertifyId 或旧分片的合成向量常量硬套到当前版本。
    """

    if not isinstance(plaintext, str) or not plaintext:
        raise DataCodecError("arg plaintext 必须是非空字符串")
    return encode64_text(plaintext, key)


def compact_json(payload: Mapping[str, Any]) -> str:
    """模拟 ``JSON.stringify`` 的紧凑输出，并保留 mapping 插入顺序。"""

    return json.dumps(
        payload,
        ensure_ascii=False,
        separators=(",", ":"),
        allow_nan=False,
    )


def build_plain_payload(
    *,
    track_list: Mapping[str, Any],
    track_start_time: int | float,
    verify_time: int | float,
    x_pos: int | str,
    slide_pos: int | str,
    arg: str,
) -> dict[str, Any]:
    """按前端字段顺序构造 checksum 之前的最终 JSON 对象。"""

    if not isinstance(track_list, Mapping):
        raise DataCodecError("track_list 必须是 mapping")
    if set(track_list) != set(TRACK_LIST_FIELDS):
        raise DataCodecError(
            "track_list 字段必须恰好为 "
            + ", ".join(TRACK_LIST_FIELDS)
        )
    # nV(rr) 自身的对象顺序与 data 最终 TrackList 顺序不同；这里按动态分片
    # 真正 JSON.stringify 前的顺序重建，避免调用方传入合法字段却在 pack 时失败。
    ordered_track_list = {
        field: track_list[field]
        for field in TRACK_LIST_FIELDS
    }
    payload: dict[str, Any] = {
        "TrackList": ordered_track_list,
        "TrackStartTime": track_start_time,
        "VerifyTime": verify_time,
        "xPos": str(x_pos),
        "slidePos": str(slide_pos),
        "arg": arg,
    }
    validate_payload_schema(payload, require_order=True)
    return payload


def validate_payload_schema(
    payload: Mapping[str, Any],
    *,
    require_order: bool = False,
) -> None:
    """验证已进入 data 层的最终对象 schema。

    ``require_order`` 适合 checksum 前的生产路径；分析旧抓包时可以关闭，
    因为对象成员顺序不影响解压和 JSON 解析，但会影响 checksum。
    """

    if not isinstance(payload, Mapping):
        raise DataCodecError("data 明文必须是 JSON object")
    keys = tuple(payload)
    if set(keys) != set(DATA_PAYLOAD_FIELDS):
        raise DataCodecError(
            "data 明文字段必须恰好为 "
            + ", ".join(DATA_PAYLOAD_FIELDS)
        )
    if require_order and keys != DATA_PAYLOAD_FIELDS:
        raise DataCodecError("data 明文字段顺序与 pe.091 不一致")

    track_list = payload["TrackList"]
    if not isinstance(track_list, Mapping):
        raise DataCodecError("TrackList 必须是 object")
    track_keys = tuple(track_list)
    if set(track_keys) != set(TRACK_LIST_FIELDS):
        raise DataCodecError(
            "TrackList 字段必须恰好为 "
            + ", ".join(TRACK_LIST_FIELDS)
        )
    if require_order and track_keys != TRACK_LIST_FIELDS:
        raise DataCodecError("TrackList 字段顺序与 e5 初始化链不一致")

    for field in TRACK_LIST_FIELDS:
        value = track_list[field]
        if field == "startTime":
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise DataCodecError("TrackList.startTime 必须是数值")
        elif not isinstance(value, str):
            raise DataCodecError(f"TrackList.{field} 必须是压平字符串")

    for field in ("TrackStartTime", "VerifyTime"):
        value = payload[field]
        if not isinstance(value, (int, float)) or isinstance(value, bool):
            raise DataCodecError(f"{field} 必须是数值")
    for field in ("xPos", "slidePos", "arg"):
        if not isinstance(payload[field], str):
            raise DataCodecError(f"{field} 必须是字符串")


def validate_checksum_prefix(
    checksum_prefix: str,
    *,
    expected: str | None = None,
) -> bool:
    """验证 checksum 的 32-hex 形状，并可做常量时间比较。"""

    if not isinstance(checksum_prefix, str) or not _CHECKSUM_RE.fullmatch(
        checksum_prefix
    ):
        raise DataCodecError("checksum 必须是 32 个十六进制字符")
    if expected is None:
        return True
    if not isinstance(expected, str) or not _CHECKSUM_RE.fullmatch(expected):
        raise DataCodecError("expected checksum 必须是 32 个十六进制字符")
    # 两者均为固定 32 字节 ASCII，compare_digest 可避免普通 == 的早停比较。
    import hmac

    return hmac.compare_digest(
        checksum_prefix.lower(),
        expected.lower(),
    )


def checksum_with_node_bridge(
    json_text: str,
    pe_script: str | Path,
    *,
    node_binary: str = "node",
    bridge_script: str | Path | None = None,
    seed: str = CHECKSUM_VM_SEED,
    timeout: float = 30.0,
) -> str:
    """从指定 ``pe.js`` 提取该版本 prefix VM，并生成 32-hex 前缀。

    JSON 通过 stdin 传给 Node，避免把轨迹、CertifyId 派生值等放进命令行。
    bridge 只抽取并执行解释器/program/constants，不执行整个公开前端 bundle。

    函数名为兼容早期分析保留；运行态已证明输出含随机性，不能把它当作普通
    ``checksum(json)``。同一输入多次运行得到不同前缀是正常行为。
    """

    if not isinstance(json_text, str):
        raise TypeError("json_text 必须是字符串")
    pe_path = Path(pe_script)
    if not pe_path.is_file():
        raise DataCodecError(f"pe.js 不存在：{pe_path}")
    if bridge_script is None:
        bridge_path = Path(__file__).with_name("checksum_bridge.mjs")
    else:
        bridge_path = Path(bridge_script)
    if not bridge_path.is_file():
        raise DataCodecError(f"checksum bridge 不存在：{bridge_path}")

    try:
        completed = subprocess.run(
            [
                node_binary,
                str(bridge_path),
                "--pe",
                str(pe_path),
                "--seed",
                seed,
            ],
            input=json_text,
            text=True,
            capture_output=True,
            check=True,
            timeout=timeout,
        )
    except FileNotFoundError as exc:
        raise DataCodecError(f"找不到 Node：{node_binary}") from exc
    except subprocess.TimeoutExpired as exc:
        raise DataCodecError("checksum Node bridge 执行超时") from exc
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.strip() or "未知错误"
        raise DataCodecError(f"checksum Node bridge 失败：{detail}") from exc

    checksum = completed.stdout.strip()
    validate_checksum_prefix(checksum)
    return checksum.lower()


# 新代码建议使用能准确表达语义的名称。
prefix_with_node_bridge = checksum_with_node_bridge


def pack_data_json(
    json_text: str,
    checksum_prefix: str,
    *,
    key: str = PE091_DATA_KEY,
    compression_level: int = 6,
) -> str:
    """封装已经按正确顺序序列化的 JSON 文本和当前分片生成的 32-hex 前缀。"""

    validate_checksum_prefix(checksum_prefix)
    if not isinstance(json_text, str):
        raise TypeError("json_text 必须是字符串")
    if not -1 <= compression_level <= 9:
        raise DataCodecError("compression_level 必须位于 -1..9")

    serialized = (checksum_prefix.lower() + json_text).encode("utf-8")
    compressed = zlib.compress(serialized, level=compression_level)
    compressed_base64 = base64.b64encode(compressed)
    ciphertext = transform64(compressed_base64, key)
    return base64.b64encode(ciphertext).decode("ascii")


def pack_data_payload(
    payload: Mapping[str, Any],
    checksum_prefix: str,
    *,
    key: str = PE091_DATA_KEY,
    compression_level: int = 6,
    require_order: bool = True,
) -> str:
    """验证 schema、紧凑序列化并生成 ``CaptchaVerifyParam.data``。"""

    validate_payload_schema(payload, require_order=require_order)
    return pack_data_json(
        compact_json(payload),
        checksum_prefix,
        key=key,
        compression_level=compression_level,
    )


def unpack_data(
    data: str,
    *,
    key: str = PE091_DATA_KEY,
    expected_checksum: str | None = None,
    checksum_func: Callable[[str], str] | None = None,
    validate_schema: bool = True,
) -> DecodedData:
    """逆解 ``CaptchaVerifyParam.data``，不依赖浏览器或 Node。

    若提供 ``checksum_func``，它接收原样 JSON 文本并返回一个期望的 32-hex
    前缀；这只适合离线固定向量。真实 prefix VM 含随机性，重新运行不会等于
    抓包前缀，因此分析抓包时通常不传该回调。
    """

    if not isinstance(data, str):
        raise TypeError("data 必须是字符串")
    try:
        ciphertext = base64.b64decode(data, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise DataCodecError("data 外层不是有效 Base64") from exc

    compressed_base64_bytes = transform64(
        ciphertext,
        key,
        decrypt=True,
    )
    try:
        compressed_base64 = compressed_base64_bytes.decode("ascii")
    except UnicodeDecodeError as exc:
        raise DataCodecError("64-state 解密结果不是 ASCII Base64") from exc
    if not _BASE64_RE.fullmatch(compressed_base64):
        raise DataCodecError("64-state 解密结果不符合 Base64 字母表")

    try:
        compressed = base64.b64decode(
            compressed_base64,
            validate=True,
        )
        serialized = zlib.decompress(compressed)
    except (binascii.Error, zlib.error, ValueError) as exc:
        raise DataCodecError("data 的 Base64/zlib 层无效") from exc
    try:
        serialized_text = serialized.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise DataCodecError("zlib 明文不是有效 UTF-8") from exc
    if len(serialized_text) < 32:
        raise DataCodecError("zlib 明文短于 32 字节 checksum")

    checksum_prefix = serialized_text[:32]
    validate_checksum_prefix(checksum_prefix)
    json_text = serialized_text[32:]
    try:
        payload = json.loads(json_text)
    except json.JSONDecodeError as exc:
        raise DataCodecError("checksum 后不是有效 JSON") from exc
    if not isinstance(payload, dict):
        raise DataCodecError("data JSON 顶层不是 object")
    if validate_schema:
        validate_payload_schema(payload)

    computed: str | None = None
    if checksum_func is not None:
        computed = checksum_func(json_text)
        validate_checksum_prefix(computed)
    if expected_checksum is not None:
        validate_checksum_prefix(expected_checksum)
        if computed is not None and computed.lower() != expected_checksum.lower():
            raise DataCodecError("checksum_func 与 expected_checksum 结果冲突")
        computed = expected_checksum
    checksum_valid = (
        None
        if computed is None
        else validate_checksum_prefix(
            checksum_prefix,
            expected=computed,
        )
    )

    return DecodedData(
        checksum_prefix=checksum_prefix.lower(),
        json_text=json_text,
        payload=payload,
        compressed_base64=compressed_base64,
        checksum_valid=checksum_valid,
    )


__all__ = [
    "CHECKSUM_VM_SEED",
    "PREFIX_VM_SEED",
    "DATA_PAYLOAD_FIELDS",
    "INITIAL_64_STATE",
    "PE091_ARG_KEY",
    "PE091_DATA_KEY",
    "TRACK_EVENT_FIELDS",
    "TRACK_LIST_FIELDS",
    "DataCodecError",
    "DecodedData",
    "build_arg",
    "build_plain_payload",
    "checksum_with_node_bridge",
    "compact_json",
    "decode64_text",
    "encode64_text",
    "keyed_state64",
    "pack_data_json",
    "pack_data_payload",
    "prefix_with_node_bridge",
    "transform64",
    "unpack_data",
    "validate_checksum_prefix",
    "validate_payload_schema",
]
