package server

func openAPIDocument() map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "AliSlider Go API",
			"version": "1.0.0",
		},
		"paths": map[string]any{
			SolvePath: map[string]any{
				"post": map[string]any{
					"summary": "执行一轮滑块验证",
					"requestBody": map[string]any{
						"required": false,
						"content": map[string]any{
							"application/json": map[string]any{"schema": requestSchema()},
						},
					},
					"responses": map[string]any{
						"200": responseSchema(
							"一次完整协议往返；业务失败仍返回 200 + ok=false",
							successSchema(),
						),
						"400": responseSchema("请求参数无效", errorSchema()),
						"500": responseSchema("协议或运行错误", errorSchema()),
					},
				},
			},
			HealthPath: map[string]any{
				"get": map[string]any{
					"summary": "健康检查",
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
					"summary": "OpenAPI 合同",
					"responses": map[string]any{
						"200": map[string]any{"description": "OpenAPI 3.0 JSON"},
					},
				},
			},
		},
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
