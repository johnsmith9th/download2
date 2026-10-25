//go:build windows

package mover

import "syscall"

// errBusy 是一个真的「现在动不了」错误，用来在单测里造占用。
//
// 它必须是本机的原生码：isBusy 只认这两个码，测试造一个包装过的假错误
// 就等于把「占用判据」这条测试对象偷换成了「错误传递」。
var errBusy error = syscall.Errno(32) // ERROR_SHARING_VIOLATION

// errNotSameDeviceForTest 是 ERROR_NOT_SAME_DEVICE：改名跨不了卷的那个码。
var errNotSameDeviceForTest error = syscall.Errno(17)
