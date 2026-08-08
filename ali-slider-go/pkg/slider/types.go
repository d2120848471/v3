// Package slider 提供可嵌入其他 Go 服务的一轮滑块验证 library 合同。
package slider

import (
	"context"
	"fmt"
)

// Request 描述单轮挑战。空字段由 Client 的默认配置补齐。
type Request struct {
	SceneID  string
	Prefix   string
	RPCKeyID string
	Proxy    string
}

// String/GoString 禁止误用通用格式化把代理凭据或 RPC key 写入日志。
func (Request) String() string   { return "slider.Request{redacted}" }
func (Request) GoString() string { return "slider.Request{redacted}" }

// Result 保持 HTTP 成功响应的业务字段；TimingsMs 的 key 是稳定阶段名。
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

// ErrorKind 是可稳定映射到 HTTP 状态的错误类别。
type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "InvalidRequest"
	ErrorProtocol       ErrorKind = "ProtocolError"
	ErrorNetwork        ErrorKind = "NetworkError"
	ErrorVision         ErrorKind = "VisionError"
	ErrorInternal       ErrorKind = "InternalError"
)

// Error 不包含 token、CertifyId 或上游原始正文，因而可安全写入普通日志。
type Error struct {
	Kind    ErrorKind
	Stage   string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Stage == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Kind, e.Stage, e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
