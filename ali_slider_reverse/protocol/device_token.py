"""DeviceToken 与 DeviceConfig 的可验证容器层。

只实现已由前端运行时确认的两种容器：

```text
DeviceConfig  Base64 → AES-128-CBC → PKCS7 → 九段 '#' 文本
DeviceToken   Base64( "WEB#session#fingerprintCipher#GatherCost#md5" )
```

**职责边界**：本模块负责容器的拆装与校验，不负责生成指纹。真实的
fingerprint cipher 由公开 FeiLin 在 Node VM 中原生产生（见
:mod:`ali_slider_reverse.runtime.node_device`）；这里只做四件事——解开
容器、验证 MD5、在 GatherCost 需要正常化时重建容器、解密指纹密文以读取
FeiLin 时钟字段。

AES key 与 deviceToken salt 均必须由调用方显式传入，模块不内置任何动态脚本
版本常量或真实挑战数据。
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
from dataclasses import dataclass, field
from typing import Any


DEFAULT_DEVICE_CONFIG_IV = b"0123456789ABCDEF"
"""前端固定的 AES-CBC 初始向量。"""


@dataclass(frozen=True, slots=True)
class DeviceConfig:
    """Init 阶段由 Log1 下发、经 Node VM 解密后的九段设备配置。

    ``key`` 是本轮 session 的指纹加密密钥，``session_id`` 把 DeviceConfig 与
    两枚 DeviceToken 绑定在同一次 FeiLin 采集上。除 ``switch``/``version``
    外的字段都标了 ``repr=False``，避免调试打印时泄漏会话材料。
    """

    key: str = field(repr=False)
    switch: int
    session_id: str = field(repr=False)
    version: str
    plugin_elements: str = field(repr=False)
    plugin_resource: str = field(repr=False)
    global_variable: str = field(repr=False)
    timestamp: str = field(repr=False)
    ip: str = field(repr=False)
    extra_segments: tuple[str, ...] = field(default=(), repr=False)

    def as_dict(self) -> dict[str, str | int | list[str]]:
        """以前端字段名返回普通字典，供 Node PE 桥消费。"""

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


def _load_cryptography() -> tuple[Any, Any, Any, Any]:
    """按需导入 AES 依赖。

    ``cryptography`` 只在真正解密时才需要，延迟导入可让仅依赖标准库的
    deviceToken 容器操作在没装该包的环境里照常工作。
    """

    try:
        from cryptography.hazmat.primitives import padding
        from cryptography.hazmat.primitives.ciphers import (
            Cipher,
            algorithms,
            modes,
        )
    except ImportError as exc:  # pragma: no cover - 由调用环境决定。
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
    """标准 Base64 → AES-128-CBC → 去 PKCS7，返回原始明文字节。"""

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


def _validate_token_part(value: str, *, label: str) -> str:
    if not isinstance(value, str):
        raise TypeError(f"{label} 必须是 str")
    if not value:
        raise ValueError(f"{label} 不能为空")
    if "#" in value:
        raise ValueError(f"{label} 不能包含 '#' 分隔符")
    return value


def _gather_cost_text(value: int | str) -> str:
    """把 GatherCost 归一化为十进制字符串。"""

    if isinstance(value, bool):
        raise TypeError("GatherCost 必须是非负整数或其十进制字符串")
    if isinstance(value, int):
        if value < 0:
            raise ValueError("GatherCost 不能为负数")
        return str(value)
    if isinstance(value, str) and value.isdigit():
        return value
    raise ValueError("GatherCost 必须是非负整数或其十进制字符串")


def _token_prefix(
    *,
    session_id: str,
    fingerprint_cipher: str,
    gather_cost: int | str,
) -> str:
    """拼出参与 MD5 的前四段（含尾部 ``#``）。"""

    session = _validate_token_part(session_id, label="deviceToken session")
    cipher = _validate_token_part(
        fingerprint_cipher,
        label="deviceToken fingerprint cipher",
    )
    return f"WEB#{session}#{cipher}#{_gather_cost_text(gather_cost)}#"


def build_device_token(
    *,
    session_id: str,
    fingerprint_cipher: str,
    gather_cost: int | str,
    salt: str | bytes,
) -> str:
    """从已加密的 fingerprint cipher 组装 deviceToken。

    MD5 原文严格为 ``WEB#session#cipher#GatherCost#`` 后**直接拼接** salt，
    随后把五段文本整体做标准 Base64。本函数不生成也不加密 fingerprint。
    """

    prefix = _token_prefix(
        session_id=session_id,
        fingerprint_cipher=fingerprint_cipher,
        gather_cost=gather_cost,
    )
    salt_bytes = _as_bytes(salt, label="deviceToken salt")
    if not salt_bytes:
        raise ValueError("deviceToken salt 不能为空")
    checksum = hashlib.md5(prefix.encode("utf-8") + salt_bytes).hexdigest()
    return base64.b64encode((prefix + checksum).encode("utf-8")).decode("ascii")


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
    if len(checksum) != 32 or any(
        char not in "0123456789abcdef" for char in checksum
    ):
        raise ValueError("deviceToken md5 必须是 32 位小写十六进制")

    salt_bytes = _as_bytes(salt, label="deviceToken salt")
    if not salt_bytes:
        raise ValueError("deviceToken salt 不能为空")
    expected = hashlib.md5(
        f"WEB#{session}#{cipher}#{cost}#".encode("utf-8") + salt_bytes
    ).hexdigest()
    if not hmac.compare_digest(checksum, expected):
        raise ValueError("deviceToken MD5 校验失败")

    return DeviceToken(
        platform=platform,
        session_id=session,
        fingerprint_cipher=cipher,
        gather_cost=cost,
        checksum=checksum,
    )


__all__ = [
    "DEFAULT_DEVICE_CONFIG_IV",
    "DeviceConfig",
    "DeviceToken",
    "aes_cbc_decrypt_base64",
    "build_device_token",
    "parse_device_token",
]
