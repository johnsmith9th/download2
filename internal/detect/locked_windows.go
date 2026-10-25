//go:build windows

package detect

import (
	"golang.org/x/sys/windows"
)

// Locked 用一次 share=0 的打开来问「还有人正写着它吗」。
//
// 这是 Windows 上唯一可信的判据：Chrome 会预分配出最终大小，所以「size 已等于预期」
// 什么也证明不了，mtime 也可能在最后几 KB 之前就停下 —— 只有句柄还开着这件事说明没下完。
//
// 打开成功就立刻 Close：那一次关闭本身也是「写者已经走了」的证据。
// Errno 32（SHARING_VIOLATION）与 33（LOCK_VIOLATION）在判定侧和改名侧含义相同，
// 所以 detect 与 mover 共用这一个函数，一条口径只写一遍。
//
// 其余失败（权限不够、路径正好没了）一律当「占用」返回：判据宁可晚一步也不能早一步，
// 而真正消失的文件会在下一次采样时由 Sampler 报错清掉，不会永远挂着。
func Locked(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // share mode 为 0：任何人开着它都会让我们失败
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return true
	}
	_ = windows.CloseHandle(h)
	return false
}
