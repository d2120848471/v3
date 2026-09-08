package slider

import (
	"errors"
	"fmt"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

// ErrorKind 是可稳定映射到 HTTP 状态的错误类别。
type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "InvalidRequest"
	ErrorProtocol       ErrorKind = "ProtocolError"
	ErrorNetwork        ErrorKind = "NetworkError"
	ErrorVision         ErrorKind = "VisionError"
	ErrorInternal       ErrorKind = "InternalError"
)

// Error 的 Error 方法只输出稳定的类别、阶段和消息；Cause 用于 errors.Is/As。
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

func publicError(err error) error {
	var failure *solve.Failure
	if !errors.As(err, &failure) {
		return &Error{Kind: ErrorInternal, Stage: "solver", Message: "未处理的协议运行错误", Cause: err}
	}
	kind := ErrorInternal
	switch failure.Kind {
	case solve.FailureInvalidRequest:
		kind = ErrorInvalidRequest
	case solve.FailureProtocol:
		kind = ErrorProtocol
	case solve.FailureNetwork:
		kind = ErrorNetwork
	case solve.FailureVision:
		kind = ErrorVision
	}
	return &Error{Kind: kind, Stage: failure.Stage, Message: failure.Message, Cause: err}
}
