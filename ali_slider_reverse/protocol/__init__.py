"""协议算法层：纯计算，不做任何网络或子进程 IO。

```text
signing.py       浏览器等价编码 + 阿里 RPC v1 HMAC-SHA1 签名
params.py        Verify 与业务请求的参数封装
device_token.py  DeviceToken/DeviceConfig 容器的拆装与校验
data_codec.py    CaptchaVerifyParam.data 的逐层解包与 schema 校验
secrets.py       公开前端静态密文的运行时恢复
```

这一层可以脱离网络单独测试，也不依赖 Node 或 OpenCV。
"""

from __future__ import annotations

from .data_codec import (
    DATA_PAYLOAD_FIELDS,
    TRACK_LIST_FIELDS,
    DecodedData,
    pack_data,
    unpack_data,
)
from .device_token import (
    DeviceConfig,
    DeviceToken,
    aes_cbc_decrypt_base64,
    aes_cbc_encrypt_base64,
    build_device_token,
    parse_device_token,
)
from .params import (
    build_business_captcha_verify_param,
    build_business_signed_query,
    build_verify_captcha_param,
)
from .secrets import FrontendSecrets, resolve_frontend_secrets
from .signing import (
    compact_json,
    js_form_urlencode,
    rpc_v1_signature,
    utc_timestamp,
    uuid4_hex_nonce,
    uuid4_nonce,
)

__all__ = [
    "DATA_PAYLOAD_FIELDS",
    "TRACK_LIST_FIELDS",
    "DecodedData",
    "DeviceConfig",
    "DeviceToken",
    "FrontendSecrets",
    "aes_cbc_decrypt_base64",
    "aes_cbc_encrypt_base64",
    "build_business_captcha_verify_param",
    "build_business_signed_query",
    "build_device_token",
    "build_verify_captcha_param",
    "compact_json",
    "js_form_urlencode",
    "parse_device_token",
    "pack_data",
    "resolve_frontend_secrets",
    "rpc_v1_signature",
    "unpack_data",
    "utc_timestamp",
    "uuid4_hex_nonce",
    "uuid4_nonce",
]
