package httpapi

func openAPIDocument() map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "AliSlider Go API",
			"version": "1.0.0",
		},
		"paths": map[string]any{
			BaxiaPath: map[string]any{"post": baxiaOperation()},
			TestPagePath: map[string]any{
				"get": map[string]any{
					"operationId": "getTestPage",
					"summary":     "本机 API 测试页",
					"responses": map[string]any{
						"200": map[string]any{
							"description": "内嵌 HTML 测试页",
							"content": map[string]any{
								"text/html": map[string]any{
									"schema": map[string]any{"type": "string"},
								},
							},
						},
						"500": map[string]any{
							"description": "随机 nonce 初始化失败",
							"content": map[string]any{
								"text/plain": map[string]any{
									"schema": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			},
			SolvePath: map[string]any{
				"get": map[string]any{
					"operationId": "solveSliderLegacy",
					"summary":     "执行一轮滑块验证（旧 query 兼容）",
					"description": "仅为兼容旧 Python 客户端；新接入请使用 POST JSON，且不要把敏感参数放入 URL。",
					"deprecated":  true,
					"parameters":  legacyQueryParameters(),
					"responses":   solveResponses(),
				},
				"post": map[string]any{
					"operationId": "solveSlider",
					"summary":     "执行一轮滑块验证",
					"description": "每次调用执行一轮独立挑战，不自动重试。业务拒绝返回 HTTP 200，调用方须检查 ok 与 VerifyResult；技术失败返回错误响应。只读取 JSON body，不合并 query。",
					"requestBody": map[string]any{
						"required": false,
						"content": map[string]any{
							"application/json": map[string]any{"schema": requestSchema()},
						},
					},
					"responses": solveResponses(),
				},
			},
			HealthPath: map[string]any{
				"get": map[string]any{
					"operationId": "getHealth",
					"summary":     "健康检查",
					"responses": map[string]any{
						"200": map[string]any{
							"description": "ready",
							"content": map[string]any{
								"application/json": map[string]any{"schema": healthSchema()},
							},
						},
					},
				},
			},
			OpenAPIPath: map[string]any{
				"get": map[string]any{
					"operationId": "getOpenAPI",
					"summary":     "OpenAPI 合同",
					"responses": map[string]any{
						"200": map[string]any{"description": "OpenAPI 3.0 JSON"},
					},
				},
			},
		},
	}
}

func legacyQueryParameters() []map[string]any {
	parameter := func(name string, maxLength int) map[string]any {
		schema := map[string]any{"type": "string"}
		if maxLength > 0 {
			schema["maxLength"] = maxLength
		}
		return map[string]any{
			"name": name, "in": "query", "required": false, "schema": schema,
		}
	}
	return []map[string]any{
		parameter("SceneId", 64), parameter("sceneId", 64),
		parameter("prefix", 32), parameter("Prefix", 32),
		parameter("AaduaneId", 128), parameter("aaduaneId", 128),
		parameter("proxy", 0), parameter("Proxy", 0),
	}
}

func solveResponses() map[string]any {
	return map[string]any{
		"200": responseSchema(
			"一次完整协议往返；业务失败仍返回 200 + ok=false",
			successSchema(),
		),
		"400": responseSchema("请求参数无效", errorSchema()),
		"403": responseSchema("浏览器跨源请求被拒绝", errorSchema()),
		"500": responseSchema("协议或运行错误", errorSchema()),
	}
}

func requestSchema() map[string]any {
	optionalText := func(maxLength int) map[string]any {
		schema := map[string]any{"type": "string", "nullable": true}
		if maxLength > 0 {
			schema["maxLength"] = maxLength
		}
		return schema
	}
	return map[string]any{
		"type":                 "object",
		"description":          "字段名大小写精确匹配；SceneId、prefix、AaduaneId、proxy 优先于各自别名。文本去除首尾空白，null 或空字符串回退默认值，未知字段忽略。body 最大 65536 字节。",
		"additionalProperties": true,
		"properties": map[string]any{
			"SceneId":   optionalText(64),
			"sceneId":   optionalText(64),
			"prefix":    optionalText(32),
			"Prefix":    optionalText(32),
			"AaduaneId": optionalText(128),
			"aaduaneId": optionalText(128),
			"proxy":     optionalText(0),
			"Proxy":     optionalText(0),
		},
	}
}

func responseSchema(description string, schema map[string]any) map[string]any {
	headers := map[string]any{
		"X-Trace-ID": map[string]any{
			"description": "本轮请求追踪 ID",
			"schema":      map[string]any{"type": "string"},
		},
	}
	return map[string]any{
		"description": description,
		"headers":     headers,
		"content": map[string]any{
			"application/json": map[string]any{"schema": schema},
		},
	}
}

func successSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"required": []string{
			"ok", "securityToken", "VerifyCode", "VerifyResult", "certifyId",
			"sceneId", "proxied", "elapsedMs", "timingsMs", "traceId",
		},
		"properties": map[string]any{
			"ok":            map[string]any{"type": "boolean"},
			"securityToken": map[string]any{"type": "string"},
			"VerifyCode":    map[string]any{"type": "string"},
			"VerifyResult":  map[string]any{"type": "boolean"},
			"certifyId":     map[string]any{"type": "string"},
			"sceneId":       map[string]any{"type": "string"},
			"proxied":       map[string]any{"type": "boolean"},
			"elapsedMs":     map[string]any{"type": "integer", "minimum": 0},
			"timingsMs": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
			},
			"traceId": map[string]any{"type": "string"},
		},
	}
}

func errorSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"ok", "errorType", "error", "traceId"},
		"properties": map[string]any{
			"ok":        map[string]any{"type": "boolean", "enum": []bool{false}},
			"errorType": map[string]any{"type": "string"},
			"error":     map[string]any{"type": "string"},
			"traceId":   map[string]any{"type": "string"},
			"timingsMs": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
			},
		},
	}
}

func healthSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"ok", "status"},
		"properties": map[string]any{
			"ok":     map[string]any{"type": "boolean", "enum": []bool{true}},
			"status": map[string]any{"type": "string", "enum": []string{"ready"}},
		},
	}
}
