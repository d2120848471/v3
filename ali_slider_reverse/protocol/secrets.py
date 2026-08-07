"""当前公开前端版本中的静态密文与运行时解密入口。

这些值来自浏览器已经加载的公开 JavaScript 资源，不是本地后端配置。模块只保存
前端**原样公开的密文**，明文在调用时才恢复，避免把二次解出的常量散落在源码、
日志或异常信息中。

安全约定：不要打印或持久化 :func:`resolve_frontend_secrets` 的返回值。
:class:`FrontendSecrets` 的所有字段都设了 ``repr=False``，误打印整个对象时
不会泄漏内容，但单独取字段再输出仍然会。
"""

from __future__ import annotations

import base64
from dataclasses import dataclass, field
from functools import lru_cache

from .device_token import aes_cbc_decrypt_base64

# 前端的 IV 构造链是 Hex.parse → Base64.stringify → Utf8.parse，
# 绕一圈之后恰好等于 ASCII 串 "0123456789ABCDEF"。
_FRONTEND_IV_HEX = "d35db7e39ebbf3d001083105"
_FRONTEND_IV = base64.b64encode(bytes.fromhex(_FRONTEND_IV_HEX))
_FRONTEND_ACCESS_KEY = b"FqJB6iRNVYdEGpwb"

if _FRONTEND_IV != b"0123456789ABCDEF":  # pragma: no cover - 常量自检。
    raise RuntimeError("前端 IV 构造链与预期不一致")

# 以下均为公开 JavaScript 中原样出现的 AES-CBC Base64 密文。
_MAIN_RPC_KEY_ID_CIPHERTEXT = "MQECT2fv9RPHHlaKkKNWZP0vS4BJe7J1XY8H7v7s27M="
_MAIN_RPC_KEY_SECRET_CIPHERTEXT = "EYKxcWbB1W70XKzyYLOt12LQ9EFXzV0OvPKeOPWhABg="
_DEVICE_TOKEN_SALT_CIPHERTEXT = "NLAoqT6K03oLbQXW2VS3zA=="
_DEVICE_RPC_KEY_ID_CIPHERTEXT = (
    "tK5X1r3E65gcLeL+GmivL6RuHUa/6AyrEEc+S59NPns="
)
_DEVICE_RPC_KEY_SECRET_CIPHERTEXT = (
    "kvqU4wg9JqUyoXqN1pdq9XZ2gkf1oPpJCVdG8Hvkkq4="
)
_DEVICE_REQUEST_KEY_CIPHERTEXT = (
    "8KmHIQsc5+LZJA7uYex3WaHdkjgCtS6epbG/bc9xss0="
)
_DEVICE_RESPONSE_KEY_CIPHERTEXT = (
    "9NhnQQ+LRrKCkAuxwZaUWGBtSzaFtFlNb/ksJCrCgrM="
)
_DEVICE_FLAG_KEY_CIPHERTEXT = (
    "k+1RW0cz3iDi2RAbC/c3QKzTPiVwNmNO1910DXe6Gas="
)
_DEVICE_UPLOAD_KEY_CIPHERTEXT = (
    "+fR9tYzlKFr07pEbumd7+KnO3xLOkphCS+qKUbJiMfA="
)
_DEVICE_PREID_KEY_CIPHERTEXT = (
    "xLLw/t15vkI7QQBX/1scBbcb9fKx+ymxF0tJ3ds42B0="
)


@dataclass(frozen=True, slots=True)
class FrontendSecrets:
    """由当前公开前端密文恢复出的协议常量。

    所有值均来自公开脚本中的静态密文，并且只在进程内按需解密。字段均禁止进入
    ``repr``，错误信息也不得包含其值。
    """

    main_rpc_key_id: str = field(repr=False)
    """验证码 RPC 的 ``AaduaneId``，可被调用方按实例覆盖。"""

    main_rpc_key_secret: str = field(repr=False)
    """验证码 RPC 的 HMAC-SHA1 签名密钥。"""

    device_token_salt: str = field(repr=False)
    """deviceToken 尾部 MD5 的盐。"""

    device_rpc_key_id: str = field(repr=False)
    """设备日志 RPC 的 ``AaduaneId``。"""

    device_rpc_key_secret: str = field(repr=False)
    """设备日志 RPC 的 HMAC-SHA1 密钥。"""

    device_request_key: str = field(repr=False)
    device_response_key: str = field(repr=False)
    device_flag_key: str = field(repr=False)
    device_upload_key: str = field(repr=False)
    device_preid_key: str = field(repr=False)
    """Log1/DeviceConfig/Log2/Log3 对应的公开前端 AES key。"""


def _decrypt(ciphertext: str) -> str:
    """按前端 AES-128-CBC/PKCS7 逻辑解开一项公开静态密文。"""

    plaintext = aes_cbc_decrypt_base64(
        ciphertext,
        key=_FRONTEND_ACCESS_KEY,
        iv=_FRONTEND_IV,
    )
    try:
        return plaintext.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("前端静态密文解密结果不是有效 UTF-8") from exc


@lru_cache(maxsize=1)
def resolve_frontend_secrets() -> FrontendSecrets:
    """恢复当前前端版本使用的 RPC 与 deviceToken 常量。

    密文是静态的，解密结果在进程生命周期内不变，因此缓存单例——设备 runtime
    每轮校验 token 都要取一次 salt，没必要重复做 AES。
    """

    return FrontendSecrets(
        main_rpc_key_id=_decrypt(_MAIN_RPC_KEY_ID_CIPHERTEXT),
        main_rpc_key_secret=_decrypt(_MAIN_RPC_KEY_SECRET_CIPHERTEXT),
        device_token_salt=_decrypt(_DEVICE_TOKEN_SALT_CIPHERTEXT),
        device_rpc_key_id=_decrypt(_DEVICE_RPC_KEY_ID_CIPHERTEXT),
        device_rpc_key_secret=_decrypt(_DEVICE_RPC_KEY_SECRET_CIPHERTEXT),
        device_request_key=_decrypt(_DEVICE_REQUEST_KEY_CIPHERTEXT),
        device_response_key=_decrypt(_DEVICE_RESPONSE_KEY_CIPHERTEXT),
        device_flag_key=_decrypt(_DEVICE_FLAG_KEY_CIPHERTEXT),
        device_upload_key=_decrypt(_DEVICE_UPLOAD_KEY_CIPHERTEXT),
        device_preid_key=_decrypt(_DEVICE_PREID_KEY_CIPHERTEXT),
    )


__all__ = ["FrontendSecrets", "resolve_frontend_secrets"]
