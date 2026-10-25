//go:build windows

package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// 那颗互斥量读的就是 ERROR_ALREADY_EXISTS。这里钉两件事，错一件都有声音：
// 1) 别人持有时报 false —— 报成 true 就是后起的实例把正在跑的实例的日志清空；
// 2) 探完必须把自己那颗放掉 —— 攥着句柄往下走，application.New 里 wails 那次 CreateMutex
// 会在自己造的这颗上读出「已有人 Running」，本进程被当成第二份，程序永远起不来。
func TestOwnsSingleInstanceLockYieldsThenLetsGo(t *testing.T) {
	name := windows.StringToUTF16Ptr(lockNameFor(uniqueID))

	held, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// 桌面上真有一颗药丸在跑：那正是这条要认的情形，顺手验完再收工。
		if ownsSingleInstanceLock() {
			t.Error("真实例正在跑，探测却报「本轮唯一」——那份日志会被清空")
		}
		t.Skip("这颗互斥量已经被真实例持有，没法在这里再占一次")
	}
	if err != nil {
		t.Fatal(err)
	}

	if ownsSingleInstanceLock() {
		windows.CloseHandle(held)
		t.Fatal("别人持有同名互斥量时该报 false")
	}
	if err := windows.CloseHandle(held); err != nil {
		t.Fatal(err)
	}
	if !ownsSingleInstanceLock() {
		t.Error("句柄已经关掉了还报 false —— application.New 会据此把本进程当成第二份退出")
	}
}
