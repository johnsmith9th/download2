package watch

import (
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"download2/internal/detect"
)

// Classify 把一条 fsnotify 事件摊成零或一条判定输入。
//
// Windows 上 ReadDirectoryChangesW 的五种 action 经 fsnotify v1.10.1 的后端
// （backend_windows.go 的 toFSnotifyFlags + newEvent）映射成这样：
//
//	ADDED            → Create(新名)
//	MODIFIED         → Write(名)
//	REMOVED          → Remove(名)
//	RENAMED_OLD_NAME → Rename(老名)
//	RENAMED_NEW_NAME → Create(新名)
//
// 最后两条合起来就是「下载完成」的信号：Chrome/Edge 的 x.zip.crdownload、
// Firefox 的 x.zip.part 都以「改名成最终名」收尾，而 detect 要的正是
// EvRenameFrom(临时名) 紧跟 EvCreate(最终名) 这一对。
//
// 这一对只能靠顺序对上：fsnotify v1.10.1 把改名来源字段（renamedFrom）设成包私有，
// 而且没有留任何读取入口，所以调用方拿不到「这个 Create 是从哪个名字来的」。
// 这也是 OnEvent 不当场下结论的又一个理由 —— 左半边先到、右半边还没到，
// 中间那一瞬间「临时名没了」看着就像下载被取消，判定必须等下一轮 Tick 才结算。
//
// Chmod（Windows 上是属性变化，杀软扫一遍就会发）不参与判据，直接丢。
func Classify(ev fsnotify.Event) []detect.Event {
	if ev.Name == "" {
		return nil
	}
	name := filepath.Clean(ev.Name)

	switch {
	case ev.Op.Has(fsnotify.Create):
		return []detect.Event{{Kind: detect.EvCreate, Path: name}}
	case ev.Op.Has(fsnotify.Rename):
		return []detect.Event{{Kind: detect.EvRenameFrom, Path: name}}
	case ev.Op.Has(fsnotify.Remove):
		return []detect.Event{{Kind: detect.EvRemove, Path: name}}
	case ev.Op.Has(fsnotify.Write):
		// Write 只表示「有动静」；静置窗口由 detect 按 mtime 与 size 复核，
		// 因为下载器预分配大小之后可能一个 Write 都不再发。
		return []detect.Event{{Kind: detect.EvWrite, Path: name}}
	}
	return nil
}
