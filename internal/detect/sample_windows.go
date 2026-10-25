//go:build windows

package detect

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// StatSample 是 Sampler 的真实实现。
//
// size / mtime / 目录与隐藏属性一律走 os.Stat：它内部查的是文件属性，不开文件句柄，
// 因此不受下载器共享模式的影响 —— 正在被独占写的文件也必须能被采样到，
// 否则「采样失败」会被状态机读成「文件没了」，把在传的东西静默丢出追踪。
//
// fileIndex 只能开着句柄问（GetFileInformationByHandle），所以要单独试一次。
// 本机实测：FILE_READ_ATTRIBUTES 这种开法不受共享模式约束，连 share=0 的独占写者都挡不住它，
// 所以正在下载的文件也拿得到完整身份。真拿不到（文件系统没有 serial number、
// 或那一次开句柄失败）就留 0，含义见 Sample.equal。
func StatSample(path string) (Sample, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Sample{}, err
	}
	s := Sample{
		Size:  fi.Size(),
		Mtime: fi.ModTime(),
		IsDir: fi.IsDir(),
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		s.Hidden = d.FileAttributes&windows.FILE_ATTRIBUTE_HIDDEN != 0
	}
	s.Index = fileIndex(path)
	return s, nil
}

// fileIndex 尽力取文件的 serial number。失败返回 0（= 不知道），不返回错误：
// 这一个量拿不到不该让整次采样作废。
func fileIndex(path string) uint64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, // 目录也要能问
		0,
	)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)

	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &d); err != nil {
		return 0
	}
	return uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow)
}
