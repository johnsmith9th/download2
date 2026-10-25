//go:build !windows

package mover

import "syscall"

// errBusy 是一个真的「现在动不了」错误，用来在单测里造占用。
//
// 这一面几乎造不出占用（POSIX 上写着的文件照样能改名），所以借用 EBUSY 这个码，
// 让同一条重试路径在非 Windows 的开发机上也能被跑到。
var errBusy error = syscall.EBUSY

// errNotSameDeviceForTest 对应 Windows 的 ERROR_NOT_SAME_DEVICE。
var errNotSameDeviceForTest error = syscall.EXDEV
