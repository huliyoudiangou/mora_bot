// Package bizerr 业务错误统一约定：
// 流程内所有"预期失败" / "参数错误" / "无权限"都用 bizerr.Errorf / Wrapf 返回，
// worker 统一在 handler 外兜底一次，避免每条 OT 调一个单独的 fail()。
package bizerr

import (
	"errors"
	"fmt"
)

// Code 用作日志分类/用户文案。
type Code int

const (
	CodeNone       Code = iota
	CodeInput           // 用户输入错误（二次确认/格式错误）
	CodeState           // 状态机过期/无效
	CodePermission      // 权限不足
	CodeRejected        // 外部服务器明确拒绝（如用户名被占用）
	CodeInternal        // 未知内部错误
)

// Error 带语义的错误；处理时不会把内部细节交给用户。
type Error struct {
	Code Code
	Op   string // 出错操作（日志用，如 "RedeemInvite"）
	Msg  string // 用户可读的中文短句
	Err  error  // 内部链
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Failw 包装底层错误并返回 Error。
func Failw(op string, err error, msg string) *Error {
	if internal, ok := AsInternal(err); ok { // keep unwrap on internal
		_ = internal
	}
	return &Error{Code: CodeInternal, Op: op, Msg: msg, Err: err}
}

// FailInput 用户输入问题（不会向上抛不确定错误）。
func FailInput(op, msg string) *Error {
	return &Error{Code: CodeInput, Op: op, Msg: msg}
}

// FailState 状态机/会话不合理。
func FailState(op, msg string) *Error {
	return &Error{Code: CodeState, Op: op, Msg: msg}
}

// FailRejected 外部服务明确拒绝。
func FailRejected(op, msg string) *Error {
	return &Error{Code: CodeRejected, Op: op, Msg: msg}
}

// FailPermission 权限不足。
func FailPermission(op, msg string) *Error {
	return &Error{Code: CodePermission, Op: op, Msg: msg}
}

func AsInternal(err error) (*Error, bool) {
	var be *Error
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
