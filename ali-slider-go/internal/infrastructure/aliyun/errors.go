package aliyun

import (
	"errors"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
)

func dependencyError(kind solve.FailureKind, message string, cause error) error {
	return &solve.DependencyError{Kind: kind, Message: message, Cause: cause}
}

type networkFailure interface{ NetworkFailure() bool }

// HTTP 2xx schema 与本地构造错误属于协议；只有明确标记的交通失败属于网络。
func networkOrProtocolError(cause error) error {
	if cause == nil {
		return nil
	}
	kind := solve.FailureProtocol
	var marked networkFailure
	if errors.As(cause, &marked) && marked.NetworkFailure() {
		kind = solve.FailureNetwork
	}
	return dependencyError(kind, "", cause)
}

// SDK 的本地运行失败与网络失败在适配器翻译，不泄漏给应用层识别。
func runtimeError(cause error, classifyFallback bool) error {
	if cause == nil {
		return nil
	}
	switch {
	case errors.Is(cause, pe.ErrKeyNetwork):
		return dependencyError(solve.FailureNetwork, "", cause)
	case errors.Is(cause, pe.ErrKeyRuntime):
		return dependencyError(solve.FailureInternal, "", cause)
	default:
		if classifyFallback {
			return networkOrProtocolError(cause)
		}
		return dependencyError(solve.FailureProtocol, "", cause)
	}
}
