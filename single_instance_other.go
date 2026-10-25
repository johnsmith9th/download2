//go:build !windows

package main

// ownsSingleInstanceLock 的非 Windows 退路：一律报「这一刻没人在跑」，等于每次启动清空日志，
// 也就是本次改动之前的行为。本工具只发 Windows，这份存在的意义是让包在非 Windows 上编译得过；
// 那边 wails 用的是锁文件而不是互斥量，名字与拼法都另有一套，照着抄只会抄出一个假指针。
func ownsSingleInstanceLock() bool { return true }
