//go:build windows

package mover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// 这个文件是 mover 直接摸 Win32 的唯一一处，另一面是 fs_other.go。
//
// 为什么不能用 os.Rename：Windows 上它走 MoveFileEx + MOVEFILE_REPLACE_EXISTING，
// 撞上已存在的目标名是**会成功并且吃掉目标文件**的。我们全部搬运都从它这里过，
// 「绝不覆盖」就钉在这一个函数上。
//
// longPath 也是这一处的职责：Go 的 os 包自己会在长路径前补 \\?\，我们直接调的
// Win32 函数不会，超过 260 的路径就会莫名失败，而下载目录里的深层文件名很容易超。

// Win32 错误码。名字比数字可读，且这些码在判定侧和搬运侧含义相同。
const (
	errSharingViolation = syscall.Errno(windows.ERROR_SHARING_VIOLATION) // 32：还有人开着写
	errLockViolation    = syscall.Errno(windows.ERROR_LOCK_VIOLATION)    // 33：字节段被锁
	errNotSameDevice    = syscall.Errno(windows.ERROR_NOT_SAME_DEVICE)   // 17：改名跨不了卷
	errFileExists       = syscall.Errno(windows.ERROR_FILE_EXISTS)       // 80：目标名已被占
	errAlreadyExists    = syscall.Errno(windows.ERROR_ALREADY_EXISTS)    // 183：同一件事的另一种报法
)

// longPath 给绝对路径补 \\?\ 前缀（\\?\UNC\... 处理网络路径）。
// 相对路径原样返回：内部 IO 一律传绝对路径，真收到相对路径应该由调用方去报错，
// 而不是在这里猜一个基准目录 —— 猜错就是往别的地方写文件。
func longPath(p string) string {
	if p == "" || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	if !filepath.IsAbs(p) {
		return p
	}
	if v := filepath.VolumeName(p); strings.HasPrefix(v, `\\`) {
		return `\\?\UNC` + v[1:] + p[len(v):]
	}
	return `\\?\` + p
}

func caseInsensitive() bool { return true }

func utf16(s string) (*uint16, error) { return windows.UTF16PtrFromString(s) }

// volumeID 返回 p 所在卷的序列号，用来判「源和目标是同一个卷吗」。
//
// 序列号挂在卷上而不是文件上，所以开的是卷根目录的句柄，不碰文件本身。
// 取不到就报错（调用方改走校验字节的那条复制路）——「不知道」永远不该被读成「同一个卷」，
// 那个读法的代价是跨卷改名失败后我们才发现自己猜错了。
func volumeID(p string) (uint64, error) {
	root, err := volumeRoot(p)
	if err != nil {
		return 0, err
	}
	h, err := openForQuery(root, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return 0, fmt.Errorf("打开卷根 %s 失败: %w", root, err)
	}
	defer windows.CloseHandle(h)

	var (
		name    [windows.MAX_PATH + 1]uint16
		fsName  [windows.MAX_PATH + 1]uint16
		serial  uint32
		compLen uint32
		fsFlags uint32
	)
	if err := windows.GetVolumeInformationByHandle(h, &name[0], uint32(len(name)),
		&serial, &compLen, &fsFlags, &fsName[0], uint32(len(fsName))); err != nil {
		return 0, fmt.Errorf("问 %s 的卷号失败: %w", root, err)
	}
	return uint64(serial), nil
}

// volumeRoot 取出 p 所在卷的根：盘符卷是 "F:\"，UNC 是分享根 "\\srv\share\"。
func volumeRoot(p string) (string, error) {
	v := filepath.VolumeName(p)
	if v == "" {
		return "", fmt.Errorf("%s 不是绝对路径，判不出卷", p)
	}
	return v + `\`, nil
}

// openForQuery 用「只问属性、谁都不挡」的开法拿句柄：
// share 三位全开 + FILE_READ_ATTRIBUTES，因此正在被独占写的文件也问得到。
func openForQuery(p string, flags uint32) (windows.Handle, error) {
	u, err := utf16(longPath(p))
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(u,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// renameNoClobber 是一次「目标名已经有人就不做」的改名。
//
// flags 为 0 就是 MOVEFILE_REPLACE_EXISTING 不开：撞名时 MoveFileEx 直接失败并报 80，
// 于是「覆盖掉别人的文件」在这条路上没有可能，而不是「我们尽量先检查一下」。
// 跨卷时它报 17，调用方据此换复制路 —— 这个码同时也是卷号判错的兜底。
func renameNoClobber(src, dst string) error {
	s, err := utf16(longPath(src))
	if err != nil {
		return err
	}
	d, err := utf16(longPath(dst))
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(s, d, 0); err != nil {
		return err
	}
	return nil
}

// isBusy 是「现在动不了，但等一下可能就动得了」。
//
// 只有这两个码算忙：别的失败（权限、路径没了、磁盘出问题）重试不会变好，
// 把它们也算成忙只会让一个坏文件把重试预算全吃光。
func isBusy(err error) bool { return isErrno(err, errSharingViolation, errLockViolation) }

func isAlreadyThere(err error) bool { return isErrno(err, errFileExists, errAlreadyExists) }

func isCrossDevice(err error) bool { return isErrno(err, errNotSameDevice) }

func isErrno(err error, codes ...syscall.Errno) bool {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range codes {
		if e == c {
			return true
		}
	}
	return false
}

// accessTime 取真实访问时间；拿不到就用 mtime 顶上（宁可时间不精确，也不许改坏 mtime）。
func accessTime(fi os.FileInfo) time.Time {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		t := time.Unix(0, d.LastAccessTime.Nanoseconds())
		if !t.IsZero() {
			return t
		}
	}
	return fi.ModTime()
}
