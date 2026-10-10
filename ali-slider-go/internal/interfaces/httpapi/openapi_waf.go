package httpapi

func wafOperation() map[string]any {
	return map[string]any{
		"operationId": "solveWAF",
		"summary":     "执行一轮 WAF / ESA 验证",
		"description": "提交保护页面的 pageUrl 和可选 proxy，由服务器读取页面并自动提取本轮挑战，创建设备画像并验证一次。当前支持 verifyType1.0 的 InitCaptchaV2 / SLIDING 协议。返回 u_atoken、u_asig 与同一设备的 uaHeaders；不回访原页面或提交登录业务，也不自动重试。保护页读取、本轮验证和后续请求须保持同一出口。只读取 JSON body，不合并 query。",
		"requestBody": map[string]any{
			"required": true,
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": wafRequestSchema(),
					"examples": map[string]any{
						"automatic": map[string]any{
							"summary": "自动读取保护页中的挑战",
							"value":   map[string]any{"pageUrl": "https://example.com/pc/index.html?orgId=your-org"},
						},
					},
				},
			},
		},
		"responses": map[string]any{
			"200": responseSchema("一轮验证完成；调用方须检查 ok 与验证签名", wafSuccessSchema()),
			"400": responseSchema("JSON、页面 URL 或代理无效", errorSchema()),
			"403": responseSchema("浏览器跨源请求被拒绝", errorSchema()),
			"500": responseSchema("页面或 SDK 获取、挑战适配、运行或超时失败", errorSchema()),
		},
	}
}

func wafRequestSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"pageUrl"},
		"additionalProperties": true,
		"description":          "字段名区分大小写，只需必填 pageUrl 和可选代理；服务器从保护页自动提取挑战。proxy 优先于 Proxy，即使规范字段为空或 null。文本去除首尾空白；未知字段忽略，body 最大 65536 字节。",
		"properties": map[string]any{
			"pageUrl": map[string]any{
				"type": "string", "format": "uri", "minLength": 1, "maxLength": 8192,
				"description": "保护页面的完整 HTTPS URL，保留 query；服务器读取此页面并提取挑战。去除首尾空白后最多 8192 个 UTF-8 字节。仅允许默认端口或显式 443 的公网 ASCII 域名，无用户名/密码和 fragment，不接受 IP、localhost 或本地域名。",
			},
			"proxy": map[string]any{
				"type": "string", "nullable": true,
				"description": "用于本轮页面、SDK 与验证的全部网络请求；后续业务请求须使用同一出口。与 /api/slider 相同：支持 http/https/socks4/socks5/socks5h，省略 scheme 补 http://；为空或 null 时直连。",
			},
			"Proxy": map[string]any{"type": "string", "nullable": true, "description": "proxy 的兼容别名"},
		},
	}
}

func wafSuccessSchema() map[string]any {
	text := func() map[string]any { return map[string]any{"type": "string"} }
	return map[string]any{
		"type": "object",
		"required": []string{
			"ok", "sceneId", "captchaType", "verifyCode", "u_atoken", "u_asig", "uaHeaders", "proxied", "elapsedMs", "traceId",
		},
		"properties": map[string]any{
			"ok":          map[string]any{"type": "boolean"},
			"sceneId":     text(),
			"captchaType": map[string]any{"type": "string", "description": "本轮挑战类型，当前支持 SLIDING"},
			"verifyCode":  map[string]any{"type": "string", "description": "上游验证结果码；业务拒绝时用于判断本轮结果"},
			"u_atoken":    map[string]any{"type": "string", "description": "WAF 验证 token；敏感字段，业务拒绝时可能为空"},
			"u_asig":      map[string]any{"type": "string", "description": "WAF 验证签名；敏感字段，业务拒绝时可能为空"},
			"proxied":     map[string]any{"type": "boolean"},
			"elapsedMs":   map[string]any{"type": "integer", "minimum": 0},
			"traceId":     text(),
			"uaHeaders": map[string]any{
				"type": "object", "description": "与本轮验证设备画像一致的请求头；后续请求应使用同一画像。",
				"required": []string{"User-Agent", "Accept-Language", "Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"},
				"properties": map[string]any{
					"User-Agent": text(), "Accept-Language": text(), "Sec-CH-UA": text(),
					"Sec-CH-UA-Mobile": text(), "Sec-CH-UA-Platform": text(),
				},
			},
		},
	}
}
