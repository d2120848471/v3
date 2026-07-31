"""当前公开前端版本中的静态密文与运行时解密入口。

这些值来自浏览器已加载的公开 JavaScript 资源，不是本地后端配置。模块只保存
前端原样公开的密文；RPC ID/secret 与 DeviceConfig 外层 key 在调用时才解密，
避免把二次恢复出的明文散落在源码、日志或测试输出中。
"""

from __future__ import annotations

import base64
from dataclasses import dataclass

from .device import aes_cbc_decrypt_base64


# 前端的 IV 构造链是 Hex.parse -> Base64.stringify -> Utf8.parse。
FRONTEND_IV_HEX = "d35db7e39ebbf3d001083105"
FRONTEND_IV = base64.b64encode(bytes.fromhex(FRONTEND_IV_HEX))
FRONTEND_ACCESS_KEY = b"FqJB6iRNVYdEGpwb"

# 以下均为公开 JavaScript 中原样出现的 AES-CBC Base64 密文。
MAIN_RPC_KEY_ID_CIPHERTEXT = (
    "MQECT2fv9RPHHlaKkKNWZP0vS4BJe7J1XY8H7v7s27M="
)
MAIN_RPC_KEY_SECRET_CIPHERTEXT = (
    "EYKxcWbB1W70XKzyYLOt12LQ9EFXzV0OvPKeOPWhABg="
)
DEVICE_CONFIG_KEY_CIPHERTEXT = (
    "9NhnQQ+LRrKCkAuxwZaUWGBtSzaFtFlNb/ksJCrCgrM="
)
DEVICE_DATA_REQUEST_KEY_CIPHERTEXT = (
    "8KmHIQsc5+LZJA7uYex3WaHdkjgCtS6epbG/bc9xss0="
)
DEVICE_DATA_FLAG_KEY_CIPHERTEXT = (
    "k+1RW0cz3iDi2RAbC/c3QKzTPiVwNmNO1910DXe6Gas="
)
FEILIN_RPC_KEY_ID_CIPHERTEXT = (
    "tBwmiXXwEdGaRtkcLvIoA/F6r4qktDu7lFi23FmDCbo="
)
FEILIN_RPC_KEY_SECRET_CIPHERTEXT = (
    "9U6Kg6ljgm9PhHvSsihjy1wJVo71uzWkRDfGiyiMVBg="
)
DEVICE_TOKEN_SALT_CIPHERTEXT = "NLAoqT6K03oLbQXW2VS3zA=="

# V3 的公开 appKey 不是鉴权 secret；它同时出现在 AliyunCaptcha.js 的
# verifyType=2.0/3.0 配置和 DeviceData 明文中。
CAPTCHA_DEVICE_APP_KEY = "ab034ec0643f91399eb33e062dc7fae1"


@dataclass(frozen=True, slots=True)
class FrontendSecrets:
    """由当前公开前端密文恢复出的协议常量。

    调用方不应打印或持久化此对象；它只用于本地、获授权的协议复现。
    """

    main_rpc_key_id: str
    main_rpc_key_secret: str
    device_config_key: str
    device_data_request_key: str
    device_data_flag_key: str
    feilin_rpc_key_id: str
    feilin_rpc_key_secret: str
    device_token_salt: str


def decrypt_frontend_constant(ciphertext: str) -> str:
    """按前端 AES-128-CBC/PKCS7 逻辑解开一项公开静态密文。"""

    plaintext = aes_cbc_decrypt_base64(
        ciphertext,
        key=FRONTEND_ACCESS_KEY,
        iv=FRONTEND_IV,
    )
    try:
        return plaintext.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("前端静态密文解密结果不是有效 UTF-8") from exc


def resolve_frontend_secrets() -> FrontendSecrets:
    """一次性恢复当前前端版本使用的 RPC 与 DeviceConfig 常量。"""

    return FrontendSecrets(
        main_rpc_key_id=decrypt_frontend_constant(
            MAIN_RPC_KEY_ID_CIPHERTEXT
        ),
        main_rpc_key_secret=decrypt_frontend_constant(
            MAIN_RPC_KEY_SECRET_CIPHERTEXT
        ),
        device_config_key=decrypt_frontend_constant(
            DEVICE_CONFIG_KEY_CIPHERTEXT
        ),
        device_data_request_key=decrypt_frontend_constant(
            DEVICE_DATA_REQUEST_KEY_CIPHERTEXT
        ),
        device_data_flag_key=decrypt_frontend_constant(
            DEVICE_DATA_FLAG_KEY_CIPHERTEXT
        ),
        feilin_rpc_key_id=decrypt_frontend_constant(
            FEILIN_RPC_KEY_ID_CIPHERTEXT
        ),
        feilin_rpc_key_secret=decrypt_frontend_constant(
            FEILIN_RPC_KEY_SECRET_CIPHERTEXT
        ),
        device_token_salt=decrypt_frontend_constant(
            DEVICE_TOKEN_SALT_CIPHERTEXT
        ),
    )


if FRONTEND_IV != b"0123456789ABCDEF":  # pragma: no cover - 常量自检。
    raise RuntimeError("前端 IV 构造链与预期不一致")


__all__ = [
    "CAPTCHA_DEVICE_APP_KEY",
    "DEVICE_CONFIG_KEY_CIPHERTEXT",
    "DEVICE_DATA_FLAG_KEY_CIPHERTEXT",
    "DEVICE_DATA_REQUEST_KEY_CIPHERTEXT",
    "DEVICE_TOKEN_SALT_CIPHERTEXT",
    "FEILIN_RPC_KEY_ID_CIPHERTEXT",
    "FEILIN_RPC_KEY_SECRET_CIPHERTEXT",
    "FRONTEND_ACCESS_KEY",
    "FRONTEND_IV",
    "FRONTEND_IV_HEX",
    "FrontendSecrets",
    "MAIN_RPC_KEY_ID_CIPHERTEXT",
    "MAIN_RPC_KEY_SECRET_CIPHERTEXT",
    "decrypt_frontend_constant",
    "resolve_frontend_secrets",
]
