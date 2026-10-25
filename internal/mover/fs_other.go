//go:build !windows

package mover

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// 这一面只服务一件事：让 mover 在非 Windows 上也能编译、跑单测。
// 产品只发 Windows（下载目录 + MoveFileEx 的语义就是需求本身），
// 所以这里追求的是「行为可推断」，不是「和 Win32 一模一样」。
//
// 但有一条 POSIX 上也照样成立，而且正是全项目最要紧的那条：
// renameNoClobber 绝不覆盖已经存在的目标名。Unix 的 rename(2) 是会覆盖的，
// 所以这里不走 os.Rename，走 link + remove —— link 撞名天然报 EEXIST。

func longPath(p string) string { return p }

func caseInsensitive() bool { return false }

// volumeID 用设备号当卷号：它标识的正是「这个路径落在哪块存储上」，
// 和 Windows 那面用卷序列号是同一个问题的两种答法。
func volumeID(p string) (uint64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s 拿不到设备号", p)
	}
	return uint64(st.Dev), nil
}

// renameNoClobber 绝不覆盖已经存在的目标名。
func renameNoClobber(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		// 只撤我们自己刚建出来的那个名字，源文件还在原地。
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// isBusy 在 POSIX 上几乎没有出场机会：那边写着的文件照样能改名。
// 留着是为了让「占用」这个概念在两面都有答案，而不是让调用方去猜空实现。
func isBusy(err error) bool { return errors.Is(err, syscall.EBUSY) }

func isAlreadyThere(err error) bool {
	return errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST)
}

func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// accessTime：这一面不去读访问时间（各平台的 stat 结构字段名不一样，
// 为一个只在开发期用到的分支引入平台分支不值当），用 mtime 顶上。
func accessTime(fi os.FileInfo) time.Time { return fi.ModTime() }
