//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockNameFor 是 wails v3 在 Windows 上那颗单实例互斥量的名字拼法。
// 它是 vendor 内部的拼法，不是我们的约定 —— 出处 single_instance_windows.go 的 acquire：
// "wails-app-" + UniqueID + 后缀。这里的 "-sim" 抄的是 mutex 那一条（同一路还拼出
// "-sic"/"-siw" 那两个窗口类名）。拼错了的后果不响：探的是一颗谁都不持有的名字，
// 于是每一轮都以为自己是第一份，日志照旧被后起的实例清空。
func lockNameFor(id string) string { return "wails-app-" + id + "-sim" }

// ownsSingleInstanceLock 问一句「这一刻有没有别的实例已经持有那颗互斥量」。
//
// 只问，不占：拿到的句柄立刻关掉。这一步非做不可 —— 互斥量对象活到最后一个句柄关闭，
// 攥着它往下走，application.New 里 wails 自己那次 CreateMutex 就会在我自己造的这颗上读出
// ERROR_ALREADY_EXISTS，于是本进程被判定成「第二份」，通知完前一份（也就是自己）当场退出：
// 表现是程序永远起不来。
//
// 提前问的只有这一件事：本轮能不能清空那份共享的运行日志。谁是第一份的最终判定、以及
// 「通知前一份然后退出」都仍然在 application.New 里，不在这里。
func ownsSingleInstanceLock() bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(lockNameFor(uniqueID)))
	// 两条路都可能拿到句柄 —— CreateMutex 在 ERROR_ALREADY_EXISTS 时照样返回一个可用的。
	// 攥着它往下走就等于自己挡住自己，见上面那段「只问不占」。
	if h != 0 {
		_ = windows.CloseHandle(h)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false // 有一份在跑：那份日志只能往里追加
	}
	// err 为 nil（本轮建出来的就是它），或者失败得说不清（名字不合法、安全描述符不让建…）：
	// 后者说明不了有人在跑，就按第一轮的老样子办。赌注只是一份运行日志被清空，
	// 而「连句柄都拿不到」那一路本来也写不出多少日志。
	return true
}
