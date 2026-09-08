package solve

import (
	"context"
	"errors"
	"fmt"
)

// FailureKind 是编排层稳定、可脱敏映射的失败类别。
type FailureKind string

const (
	FailureInvalidRequest FailureKind = "invalid_request"
	FailureProtocol       FailureKind = "protocol"
	FailureNetwork        FailureKind = "network"
	FailureVision         FailureKind = "vision"
	FailureInternal       FailureKind = "internal"
)

// Failure 的 Message 只允许稳定文本；Cause 仅保留已由底层脱敏的诊断链。
type Failure struct {
	Kind    FailureKind
	Stage   string
	Message string
	Cause   error
}

func (failure *Failure) Error() string {
	if failure == nil {
		return ""
	}
	return fmt.Sprintf("%s at %s: %s", failure.Kind, failure.Stage, failure.Message)
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// DependencyError 由适配器将实现细节映射到稳定错误类别，保留 errors.Is 链。
// Message 只能使用固定文本；留空时使用当前阶段的默认消息。
type DependencyError struct {
	Kind    FailureKind
	Message string
	Cause   error
}

func (failure *DependencyError) Error() string {
	if failure == nil {
		return ""
	}
	if failure.Message != "" {
		return failure.Message
	}
	return "solve dependency failed"
}
func (failure *DependencyError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

func dependencyFailure(ctx context.Context, fallback FailureKind, stage, message string, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return contextFailure(stage, ctx.Err())
	}
	var dependency *DependencyError
	if errors.As(cause, &dependency) {
		if dependency.Kind != "" {
			fallback = dependency.Kind
		}
		if dependency.Message != "" {
			message = dependency.Message
		}
	}
	return fail(fallback, stage, message, cause)
}

func fail(kind FailureKind, stage, message string, cause error) error {
	return &Failure{Kind: kind, Stage: stage, Message: message, Cause: cause}
}

func stageFailure(ctx context.Context, kind FailureKind, stage, message string, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return contextFailure(stage, ctx.Err())
	}
	return fail(kind, stage, message, cause)
}

func contextFailure(stage string, cause error) error {
	return fail(FailureNetwork, stage, "请求已取消或超过总时限", cause)
}

func checkStageContext(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return contextFailure(stage, err)
	}
	return nil
}

func failureReason(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return string(failure.Kind)
	}
	return string(FailureInternal)
}
