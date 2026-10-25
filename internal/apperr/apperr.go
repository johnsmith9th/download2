// Package apperr 是全后端共用的错误词汇表：稳定码 + 一句中文 + 只进日志的原始错误。
//
// 为什么要单独一个包而不是塞在 ui.go 里：config、mover、detect 都会造出要交给界面的
// 错误，码必须是它们和界面之间的共同语言，谁都不该为了拿一个常量去 import 别人。
package apperr

import (
	"errors"
	"fmt"
)

type Code string

const (
	EInvalidArg       Code = "E_INVALID_ARG"
	EPathForbidden    Code = "E_PATH_FORBIDDEN"
	EFileOpFailed     Code = "E_FILE_OP_FAILED"
	EConfigCorrupt    Code = "E_CONFIG_CORRUPT"
	ENotConfigured    Code = "E_NOT_CONFIGURED"
	EWatchUnavailable Code = "E_WATCH_UNAVAILABLE"
	EInternal         Code = "E_INTERNAL"
)

// Error 的 Msg 是可以直接显示给用户的中文短句；Err 只进日志。
// 这条界线很重要：OS 原文是英文带路径反斜杠的，扔进界面会把面板撑坏。
type Error struct {
	Code   Code
	Msg    string
	Params map[string]string
	Err    error
}

func New(code Code, msg string) *Error {
	return &Error{Code: code, Msg: msg}
}

func Wrap(code Code, msg string, err error) *Error {
	return &Error{Code: code, Msg: msg, Err: err}
}

// With 挂参数用：链式写，省掉先构造再赋字段的三步。
func (e *Error) With(key, val string) *Error {
	if e.Params == nil {
		e.Params = make(map[string]string, 2)
	}
	e.Params[key] = val
	return e
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Of 把任意 error 归一化成 *Error。不是 *Error 的一律按 E_INTERNAL 计——
// 那说明某个地方漏了包装，宁可给界面一个通用文案，也不把 Go 的原始报错贴上去。
func Of(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(EInternal, "内部错误", err)
}
