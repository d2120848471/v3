package slider

import "context"

// Request 描述单轮挑战。SceneID 和 Prefix 为空时使用 Client 的默认配置。
type Request struct {
	SceneID  string
	Prefix   string
	RPCKeyID string
	Proxy    string
}

// String/GoString 禁止误用通用格式化把代理凭据或 RPC key 写入日志。
func (Request) String() string   { return "slider.Request{redacted}" }
func (Request) GoString() string { return "slider.Request{redacted}" }

// Result 保持 HTTP 成功响应的业务字段；TRACELESS 与 PUZZLE 使用同一字段合同。
// TimingsMS 的 key 是稳定阶段名。
type Result struct {
	OK            bool           `json:"ok"`
	SecurityToken string         `json:"securityToken"`
	VerifyCode    string         `json:"VerifyCode"`
	VerifyResult  bool           `json:"VerifyResult"`
	CertifyID     string         `json:"certifyId"`
	SceneID       string         `json:"sceneId"`
	Proxied       bool           `json:"proxied"`
	ElapsedMS     int64          `json:"elapsedMs"`
	TimingsMS     map[string]int `json:"timingsMs"`
	TraceID       string         `json:"traceId,omitempty"`
}

// String/GoString 不打印 securityToken 或 CertifyId。需要业务字段时必须显式访问。
func (Result) String() string   { return "slider.Result{redacted}" }
func (Result) GoString() string { return "slider.Result{redacted}" }

// Solver 便于宿主服务替换网络实现并进行完全离线的集成测试。
type Solver interface {
	Solve(context.Context, Request) (Result, error)
}
