package httpapi

func baxiaOperation() map[string]any {
	return map[string]any{
		"operationId": "generateBXUA",
		"summary":     "生成 Baxia / Fireye bx-ua",
		"description": "每次调用通过可选代理获取当前 AWSC 与 Fireye，使用公共设备画像在本地 V8 中生成一次 bx-ua。pageUrl 和 requestUrl 只用于计算，不请求这些目标地址；ok=true 只表示生成成功，不代表站点验证通过。只读取 JSON body，不合并 query。",
		"requestBody": map[string]any{
			"required": true,
			"content": map[string]any{
				"application/json": map[string]any{"schema": baxiaRequestSchema()},
			},
		},
		"responses": map[string]any{
			"200": responseSchema("本地生成成功；使用 bx-ua 与配套 uaHeaders 发起调用方自己的业务请求", baxiaSuccessSchema()),
			"400": responseSchema("JSON、必填 URL 或代理参数无效", errorSchema()),
			"403": responseSchema("浏览器跨源请求被拒绝", errorSchema()),
			"500": responseSchema("脚本下载、SDK 适配、V8 运行或超时失败", errorSchema()),
		},
	}
}

func baxiaRequestSchema() map[string]any {
	urlField := func(description string) map[string]any {
		return map[string]any{
			"type": "string", "format": "uri", "minLength": 1, "maxLength": 8192,
			"description": description + "；去除首尾空白后最多 8192 个 UTF-8 字节，必须是带主机且无用户名/密码的绝对 HTTP(S) URL。",
		}
	}
	return map[string]any{
		"type":                 "object",
		"required":             []string{"pageUrl", "requestUrl"},
		"additionalProperties": true,
		"description":          "字段名区分大小写；pageUrl、requestUrl 必填。proxy 优先于 Proxy，即使为空或 null。未知字段忽略，body 最大 65536 字节。设备画像由服务为本次请求生成。",
		"properties": map[string]any{
			"pageUrl":    urlField("SDK 所在页面的完整地址，可包含 hash 路由"),
			"requestUrl": urlField("需要生成 bx-ua 的业务请求完整地址，应保留实际 query"),
			"proxy": map[string]any{
				"type": "string", "nullable": true,
				"description": "仅用于本次公开脚本下载。与 /api/slider 相同：支持 http/https/socks4/socks5/socks5h，省略 scheme 补 http://；为空或 null 时直连。",
			},
			"Proxy": map[string]any{"type": "string", "nullable": true, "description": "proxy 的兼容别名"},
		},
	}
}

func baxiaSuccessSchema() map[string]any {
	text := func() map[string]any { return map[string]any{"type": "string", "minLength": 1} }
	hash := func() map[string]any { return map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"} }
	version := func() map[string]any { return map[string]any{"type": "integer", "minimum": 1} }
	return map[string]any{
		"type": "object",
		"required": []string{
			"ok", "bx-ua", "version", "sdkURL", "sdkSha256", "awscSha256",
			"profile", "uaHeaders", "proxied", "elapsedMs", "traceId",
		},
		"properties": map[string]any{
			"ok":         map[string]any{"type": "boolean", "enum": []bool{true}},
			"bx-ua":      text(),
			"version":    version(),
			"sdkURL":     map[string]any{"type": "string", "format": "uri"},
			"sdkSha256":  hash(),
			"awscSha256": hash(),
			"proxied":    map[string]any{"type": "boolean"},
			"elapsedMs":  map[string]any{"type": "integer", "minimum": 0},
			"traceId":    text(),
			"profile": map[string]any{
				"type": "object", "description": "本次 SDK 的输出编码信息，非设备画像，也不包含内部加密密钥。",
				"required": []string{"version", "prefix", "alphabet", "sdkHash"},
				"properties": map[string]any{
					"version": version(), "prefix": text(), "sdkHash": hash(),
					"alphabet": map[string]any{"type": "string", "minLength": 65, "maxLength": 65},
				},
			},
			"uaHeaders": map[string]any{
				"type": "object", "description": "与本次 token 设备画像一致的请求头；后续业务请求应复用这些值。",
				"required": []string{"User-Agent", "Accept-Language", "Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"},
				"properties": map[string]any{
					"User-Agent": text(), "Accept-Language": text(), "Sec-CH-UA": text(),
					"Sec-CH-UA-Mobile": text(), "Sec-CH-UA-Platform": text(),
				},
			},
		},
	}
}
