"""DeviceConfig 与 deviceToken 的可验证封装层。

本模块只实现已经由前端运行时确认的容器格式：

* DeviceConfig：标准 Base64 → AES-128-CBC → PKCS7 → 九段 ``#`` 文本；
* deviceToken：标准 Base64 包裹的
  ``WEB#session#fingerprintCipher#GatherCost#md5``。

AES key、deviceToken salt 和已经加密的 fingerprint cipher 均必须由调用方显式
提供；模块不内置任何动态脚本版本常量或真实挑战数据。
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import math
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any


DEFAULT_DEVICE_CONFIG_IV = b"0123456789ABCDEF"
DEVICE_CONFIG_FIELDS = (
    "key",
    "switch",
    "sessionId",
    "version",
    "pluginElements",
    "pluginResource",
    "globalVariable",
    "timestamp",
    "ip",
)
_DEVICE_CONFIG_BASE64_INDEXES = frozenset({0, 1, 4, 5, 6})


@dataclass(frozen=True, slots=True)
class DeviceConfig:
    """解密后的九段设备配置。"""

    key: str = field(repr=False)
    switch: int
    session_id: str = field(repr=False)
    version: str
    plugin_elements: str = field(repr=False)
    plugin_resource: str = field(repr=False)
    global_variable: str = field(repr=False)
    timestamp: str = field(repr=False)
    ip: str = field(repr=False)
    extra_segments: tuple[str, ...] = field(
        default=(),
        repr=False,
    )

    # 以下属性保留前端字段拼写，便于与运行时闭包逐项核对。
    @property
    def sessionId(self) -> str:  # noqa: N802 - 刻意保留 JS 字段名
        return self.session_id

    @property
    def pluginElements(self) -> str:  # noqa: N802
        return self.plugin_elements

    @property
    def pluginResource(self) -> str:  # noqa: N802
        return self.plugin_resource

    @property
    def globalVariable(self) -> str:  # noqa: N802
        return self.global_variable

    def as_dict(self) -> dict[str, str | int | list[str]]:
        """以抓包/前端字段名返回可直接序列化的普通字典。"""

        result: dict[str, str | int | list[str]] = {
            "key": self.key,
            "switch": self.switch,
            "sessionId": self.session_id,
            "version": self.version,
            "pluginElements": self.plugin_elements,
            "pluginResource": self.plugin_resource,
            "globalVariable": self.global_variable,
            "timestamp": self.timestamp,
            "ip": self.ip,
        }
        if self.extra_segments:
            # 当前 FeiLin 1.5.1 会在九段旧 schema 后追加一个状态位；公开 SDK
            # 仍只消费前九段，这里保留但不擅自赋予未确认语义。
            result["extraSegments"] = list(self.extra_segments)
        return result


@dataclass(frozen=True, slots=True)
class DeviceToken:
    """校验通过后的 deviceToken 五段内容。"""

    platform: str = field(repr=False)
    session_id: str = field(repr=False)
    fingerprint_cipher: str = field(repr=False)
    gather_cost: str = field(repr=False)
    checksum: str = field(repr=False)

    @property
    def session(self) -> str:
        return self.session_id

    @property
    def cipher(self) -> str:
        return self.fingerprint_cipher

    @property
    def GatherCost(self) -> str:  # noqa: N802 - 与已确认的协议段名一致
        return self.gather_cost

    def as_dict(self) -> dict[str, str]:
        return {
            "platform": self.platform,
            "session": self.session_id,
            "cipher": self.fingerprint_cipher,
            "GatherCost": self.gather_cost,
            "md5": self.checksum,
        }


def _as_bytes(value: str | bytes, *, label: str) -> bytes:
    if isinstance(value, str):
        return value.encode("utf-8")
    if isinstance(value, bytes):
        return value
    raise TypeError(f"{label} 必须是 str 或 bytes")


def _decode_standard_base64(value: str | bytes, *, label: str) -> bytes:
    raw = _as_bytes(value, label=label)
    try:
        return base64.b64decode(raw, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise ValueError(f"{label} 不是有效的标准 Base64") from exc


def _decode_base64_utf8(value: str, *, label: str) -> str:
    decoded = _decode_standard_base64(value, label=label)
    try:
        return decoded.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError(f"{label} Base64 解码结果不是有效 UTF-8") from exc


def _load_cryptography() -> tuple[Any, Any, Any, Any]:
    """按需导入 AES 依赖，避免标准库路径被可选依赖阻断。"""

    try:
        from cryptography.hazmat.primitives import padding
        from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
    except ImportError as exc:  # pragma: no cover - 由无依赖解释器单独覆盖。
        raise ImportError(
            "AES-CBC 功能需要 cryptography；请切换到已安装该包的 Python 环境"
        ) from exc
    return padding, Cipher, algorithms, modes


def aes_cbc_decrypt_base64(
    encrypted: str | bytes,
    *,
    key: str | bytes,
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> bytes:
    """标准 Base64 → AES-CBC → PKCS7，返回原始明文字节。"""

    key_bytes = _as_bytes(key, label="AES key")
    iv_bytes = _as_bytes(iv, label="AES IV")
    if len(key_bytes) != 16:
        raise ValueError("AES-128 key 必须恰好为 16 字节")
    if len(iv_bytes) != 16:
        raise ValueError("CBC IV 必须恰好为 16 字节")
    ciphertext = _decode_standard_base64(encrypted, label="AES ciphertext")
    if not ciphertext or len(ciphertext) % 16:
        raise ValueError("AES ciphertext 长度必须是非零的 block 倍数")

    padding, Cipher, algorithms, modes = _load_cryptography()
    try:
        decryptor = Cipher(
            algorithms.AES(key_bytes),
            modes.CBC(iv_bytes),
        ).decryptor()
        padded = decryptor.update(ciphertext) + decryptor.finalize()
        unpadder = padding.PKCS7(128).unpadder()
        return unpadder.update(padded) + unpadder.finalize()
    except ValueError as exc:
        raise ValueError("AES 解密失败或 PKCS7 padding 无效") from exc


def aes_cbc_encrypt_base64(
    plaintext: str | bytes,
    *,
    key: str | bytes,
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> str:
    """UTF-8/bytes → PKCS7 → AES-CBC → 标准 Base64。"""

    key_bytes = _as_bytes(key, label="AES key")
    iv_bytes = _as_bytes(iv, label="AES IV")
    if len(key_bytes) != 16:
        raise ValueError("AES-128 key 必须恰好为 16 字节")
    if len(iv_bytes) != 16:
        raise ValueError("CBC IV 必须恰好为 16 字节")
    raw = _as_bytes(plaintext, label="AES plaintext")

    padding, Cipher, algorithms, modes = _load_cryptography()
    padder = padding.PKCS7(128).padder()
    padded = padder.update(raw) + padder.finalize()
    encryptor = Cipher(
        algorithms.AES(key_bytes),
        modes.CBC(iv_bytes),
    ).encryptor()
    ciphertext = encryptor.update(padded) + encryptor.finalize()
    return base64.b64encode(ciphertext).decode("ascii")


def parse_device_config_plaintext(value: str | bytes) -> DeviceConfig:
    """解析 AES 解密后的九段 DeviceConfig 明文。

    前端证据表明 D0、D1、D4、D5、D6 还需要标准 Base64 + UTF-8 解码；
    D2、D3、D7、D8 保持明文。
    """

    raw = _as_bytes(value, label="DeviceConfig 明文")
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("DeviceConfig 明文不是有效 UTF-8") from exc

    segments = text.split("#")
    if len(segments) < 9:
        raise ValueError(f"DeviceConfig 至少应为 9 段，实际为 {len(segments)} 段")
    decoded = [
        _decode_base64_utf8(segment, label=f"DeviceConfig.D{index}")
        if index in _DEVICE_CONFIG_BASE64_INDEXES
        else segment
        for index, segment in enumerate(segments)
    ]
    try:
        switch_number = int(decoded[1], 10)
    except ValueError as exc:
        raise ValueError("DeviceConfig.D1 解码后必须是十进制整数") from exc

    return DeviceConfig(
        key=decoded[0],
        switch=switch_number,
        session_id=decoded[2],
        version=decoded[3],
        plugin_elements=decoded[4],
        plugin_resource=decoded[5],
        global_variable=decoded[6],
        timestamp=decoded[7],
        ip=decoded[8],
        extra_segments=tuple(decoded[9:]),
    )


def decrypt_device_config(
    encrypted: str | bytes,
    *,
    key: str | bytes,
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> DeviceConfig:
    """解密 Base64 编码的 AES-128-CBC/PKCS7 DeviceConfig。

    ``cryptography`` 仅在调用本函数时按需导入；环境缺失时会给出明确安装提示，
    不影响同模块中只依赖标准库的 deviceToken 编解码。
    """

    try:
        plaintext = aes_cbc_decrypt_base64(encrypted, key=key, iv=iv)
    except ValueError as exc:
        raise ValueError(f"DeviceConfig {exc}") from exc
    return parse_device_config_plaintext(plaintext)


def build_device_data(
    *,
    scene_id: str,
    prefix: str,
    region: str,
    app_key: str,
    request_key: str | bytes,
    flag_key: str | bytes,
    app_name: str = "saf-captcha",
    platform: str = "W.10001.c",
    mode: str = "captcha-normal",
    app_version: str = "W20220202",
    deployment: str = "CLOUD",
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> str:
    """构造首次 V3 Init 使用的确定性 ``DeviceData``。

    公开 SDK 在还没有 FeiLin session 时先用 FLAG key 加密设备场景串，再把
    结果放入由 REQ key 加密的外层。该值不包含时间、随机数或浏览器指纹；
    Init 返回的 ``DeviceConfig`` 才是后续 fresh DeviceToken 的会话根。
    """

    text_values = {
        "scene_id": scene_id,
        "prefix": prefix,
        "region": region,
        "app_key": app_key,
        "app_name": app_name,
        "platform": platform,
        "mode": mode,
        "app_version": app_version,
        "deployment": deployment,
    }
    for label, value in text_values.items():
        if not isinstance(value, str) or not value or "#" in value:
            raise ValueError(f"{label} 必须是非空且不含 '#' 的字符串")

    inner_plaintext = "#".join(
        (platform, app_name, scene_id, mode, prefix, region)
    )
    inner_ciphertext = aes_cbc_encrypt_base64(
        inner_plaintext,
        key=flag_key,
        iv=iv,
    )
    outer_plaintext = (
        f"{app_key}#W#{inner_ciphertext}#"
        f"{app_version}#{deployment}#"
    )
    return aes_cbc_encrypt_base64(
        outer_plaintext,
        key=request_key,
        iv=iv,
    )


def _normalize_json_value(value: Any) -> Any:
    """把 Python 值归一化到 JSON.stringify 可表达的值域。"""

    if value is None or isinstance(value, (str, bool, int)):
        return value
    if isinstance(value, float):
        return value if math.isfinite(value) else None
    if isinstance(value, Mapping):
        normalized_items: list[tuple[str, Any]] = []
        seen: set[str] = set()
        for key, item in value.items():
            text_key = str(key)
            if text_key in seen:
                raise ValueError(
                    f"fingerprint key 归一化后重复：{text_key!r}"
                )
            seen.add(text_key)
            normalized_items.append(
                (text_key, _normalize_json_value(item))
            )

        # Object.keys/Object.entries 会把数组索引属性先按数值升序列举，随后才是
        # 其他字符串属性的插入顺序；FeiLin 的 ip() 直接依赖这个顺序。
        index_items = sorted(
            (
                (int(key), key, item)
                for key, item in normalized_items
                if _is_js_array_index(key)
            ),
            key=lambda entry: entry[0],
        )
        normal_items = [
            (key, item)
            for key, item in normalized_items
            if not _is_js_array_index(key)
        ]
        return {
            key: item
            for _, key, item in index_items
        } | {
            key: item
            for key, item in normal_items
        }
    if isinstance(value, (list, tuple)):
        return [_normalize_json_value(item) for item in value]
    raise TypeError(
        "fingerprint 只支持 JSON 值；"
        f"收到 {type(value).__name__}"
    )


def _is_js_array_index(key: str) -> bool:
    """判断字符串是否属于 JavaScript OrdinaryOwnPropertyKeys 的数组索引。"""

    if not key or not key.isascii() or not key.isdigit():
        return False
    # "00"/"01" 不是 canonical numeric string；2**32-1 也不是数组索引。
    number = int(key, 10)
    return 0 <= number < 2**32 - 1 and str(number) == key


def serialize_fingerprint(features: Mapping[str, Any]) -> str:
    """复刻 FeiLin ``ip(featureObject, 501)`` 的指纹明文序列化。

    运行态证明第二个参数 ``501`` 不影响输出。算法先做一次
    ``JSON.parse(JSON.stringify(obj))`` 归一化，再按对象枚举顺序取 value；
    每个 value 单独 ``JSON.stringify``，全局删除双引号和 ``#``，最后以
    ``#`` 连接。调用方应按当前 FeiLin 版本构造有序 mapping。
    """

    if not isinstance(features, Mapping):
        raise TypeError("features 必须是 mapping")
    normalized = _normalize_json_value(features)
    assert isinstance(normalized, dict)

    pieces: list[str] = []
    for value in normalized.values():
        encoded = json.dumps(
            value,
            ensure_ascii=False,
            separators=(",", ":"),
            allow_nan=False,
        )
        pieces.append(encoded.replace('"', "").replace("#", ""))
    return "#".join(pieces)


def encrypt_fingerprint(
    features: Mapping[str, Any],
    *,
    fp_key: str | bytes,
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> str:
    """序列化当前有序指纹对象，并以 Log1 下发的 fp_key 做 AES-CBC。"""

    return aes_cbc_encrypt_base64(
        serialize_fingerprint(features),
        key=fp_key,
        iv=iv,
    )


def _validate_token_part(value: str, *, label: str) -> str:
    if not isinstance(value, str):
        raise TypeError(f"{label} 必须是 str")
    if "#" in value:
        raise ValueError(f"{label} 不能包含 '#' 分隔符")
    if not value:
        raise ValueError(f"{label} 不能为空")
    return value


def _gather_cost_text(value: int | str) -> str:
    if isinstance(value, bool):
        raise TypeError("GatherCost 必须是非负整数或其十进制字符串")
    if isinstance(value, int):
        if value < 0:
            raise ValueError("GatherCost 不能为负数")
        return str(value)
    if isinstance(value, str) and value.isdigit():
        return value
    raise ValueError("GatherCost 必须是非负整数或其十进制字符串")


def _device_token_prefix(
    *,
    session_id: str,
    fingerprint_cipher: str,
    gather_cost: int | str,
) -> tuple[str, str]:
    session = _validate_token_part(session_id, label="deviceToken session")
    cipher = _validate_token_part(
        fingerprint_cipher,
        label="deviceToken fingerprint cipher",
    )
    cost = _gather_cost_text(gather_cost)
    return f"WEB#{session}#{cipher}#{cost}#", cost


def build_device_token(
    *,
    session_id: str,
    fingerprint_cipher: str,
    gather_cost: int | str,
    salt: str | bytes,
) -> str:
    """从已经加密的 fingerprint cipher 生成 deviceToken。

    MD5 原文严格为 ``WEB#session#cipher#GatherCost#`` 后直接拼接调用方 salt；
    随后把五段文本整体做标准 Base64。这里不负责生成或加密 fingerprint。
    """

    prefix, _ = _device_token_prefix(
        session_id=session_id,
        fingerprint_cipher=fingerprint_cipher,
        gather_cost=gather_cost,
    )
    salt_bytes = _as_bytes(salt, label="deviceToken salt")
    if not salt_bytes:
        raise ValueError("deviceToken salt 不能为空")
    checksum = hashlib.md5(prefix.encode("utf-8") + salt_bytes).hexdigest()
    return base64.b64encode((prefix + checksum).encode("utf-8")).decode("ascii")


def build_device_token_from_features(
    *,
    features: Mapping[str, Any],
    session_id: str,
    fp_key: str | bytes,
    salt: str | bytes,
    gather_cost_field: str = "GatherCost",
    iv: str | bytes = DEFAULT_DEVICE_CONFIG_IV,
) -> str:
    """从 FeiLin 特征对象直接生成 deviceToken。

    ``GatherCost`` 在加密前从特征对象中删除，作为 deviceToken 第四段单独使用；
    其余字段保持调用方提供的枚举顺序参与 :func:`serialize_fingerprint`。
    """

    if not isinstance(features, Mapping):
        raise TypeError("features 必须是 mapping")
    if gather_cost_field not in features:
        raise ValueError(f"features 缺少 {gather_cost_field}")
    remaining = dict(features)
    gather_cost = remaining.pop(gather_cost_field)
    cipher = encrypt_fingerprint(remaining, fp_key=fp_key, iv=iv)
    return build_device_token(
        session_id=session_id,
        fingerprint_cipher=cipher,
        gather_cost=gather_cost,
        salt=salt,
    )


def parse_device_token(
    token: str | bytes,
    *,
    salt: str | bytes,
) -> DeviceToken:
    """Base64 解包 deviceToken，并以显式 salt 验证尾部 MD5。"""

    decoded = _decode_standard_base64(token, label="deviceToken")
    try:
        text = decoded.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("deviceToken 解包结果不是有效 UTF-8") from exc
    segments = text.split("#")
    if len(segments) != 5:
        raise ValueError(f"deviceToken 应为 5 段，实际为 {len(segments)} 段")
    platform, session, cipher, cost, checksum = segments
    if platform != "WEB":
        raise ValueError("deviceToken 第一段必须是 'WEB'")
    _validate_token_part(session, label="deviceToken session")
    _validate_token_part(cipher, label="deviceToken fingerprint cipher")
    cost = _gather_cost_text(cost)
    if len(checksum) != 32 or any(char not in "0123456789abcdef" for char in checksum):
        raise ValueError("deviceToken md5 必须是 32 位小写十六进制")

    salt_bytes = _as_bytes(salt, label="deviceToken salt")
    if not salt_bytes:
        raise ValueError("deviceToken salt 不能为空")
    prefix = f"WEB#{session}#{cipher}#{cost}#"
    expected = hashlib.md5(prefix.encode("utf-8") + salt_bytes).hexdigest()
    if not hmac.compare_digest(checksum, expected):
        raise ValueError("deviceToken MD5 校验失败")
    return DeviceToken(
        platform=platform,
        session_id=session,
        fingerprint_cipher=cipher,
        gather_cost=cost,
        checksum=checksum,
    )


def verify_device_token(token: str | bytes, *, salt: str | bytes) -> bool:
    """仅返回 deviceToken 的结构与 MD5 是否有效。"""

    try:
        parse_device_token(token, salt=salt)
    except (TypeError, ValueError):
        return False
    return True


# 与抓包分析术语一致的别名。
generate_device_token = build_device_token
unpack_device_token = parse_device_token


__all__ = [
    "DEFAULT_DEVICE_CONFIG_IV",
    "DEVICE_CONFIG_FIELDS",
    "DeviceConfig",
    "DeviceToken",
    "aes_cbc_decrypt_base64",
    "aes_cbc_encrypt_base64",
    "build_device_token",
    "build_device_token_from_features",
    "build_device_data",
    "decrypt_device_config",
    "encrypt_fingerprint",
    "generate_device_token",
    "parse_device_config_plaintext",
    "parse_device_token",
    "serialize_fingerprint",
    "unpack_device_token",
    "verify_device_token",
]
