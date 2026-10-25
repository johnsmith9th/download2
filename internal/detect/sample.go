package detect

import (
	"os"
	"path/filepath"
	"time"
)

// Sample 是「某一刻关于这个路径的全部已知事实」。
//
// 三个字段合起来当身份用：size 会变，mtime 会被下载器预分配时改动，
// fileIndex 才是「这个路径上还是不是同一个文件对象」的硬证据 ——
// 同名重建会换一个 index，而单调追加不改。
type Sample struct {
	Size   int64
	Mtime  time.Time
	Index  uint64 // 0 = 这一轮没探到，不是「身份为 0」
	IsDir  bool
	Hidden bool
}

// Sampler 是状态机唯一的读文件系统入口。真实实现见 StatSample。
type Sampler interface {
	Sample(path string) (Sample, error)
}

// Locker 回答「现在有人开着它写吗」。真实实现见 Locked（Windows 侧是 share=0 的 CreateFile 试探）。
type Locker interface {
	Locked(path string) bool
}

// Clock 让测试能把 10 分钟的预算压成几次赋值。
type Clock interface {
	Now() time.Time
}

type SamplerFunc func(string) (Sample, error)

func (f SamplerFunc) Sample(p string) (Sample, error) { return f(p) }

// Snapshot 枚举目录顶层一次，产出 Machine.Scan 要的快照。
//
// 非递归是刻意的：别人往 Downloads 拷一整个文件夹时，我们只该看到一个目录事件，
// 而不是把里面的东西逐文件切碎 —— 目录会被「不是普通文件」那条判据挡掉。
// 单个条目采样失败（正好被删掉）只跳过它，不让整次快照失败：
// 一次重扫失败就等于丢掉整个 Downloads 的追踪。
func Snapshot(dir string) ([]ScanEntry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]ScanEntry, 0, len(des))
	for _, de := range des {
		p := filepath.Join(dir, de.Name())
		s, err := StatSample(p)
		if err != nil {
			continue
		}
		out = append(out, ScanEntry{Path: p, Sample: s})
	}
	return out, nil
}

type LockerFunc func(string) bool

func (f LockerFunc) Locked(p string) bool { return f(p) }

// RealClock 是生产用的时钟。
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// equal 比的是身份三元组，但 Index 为 0 表示「这一轮没探到」——
// 文件系统没有 serial number（exFAT/UFD 这类盘）或者那次开句柄失败时就是这样。
// 未知不能当否定证据：一次偶发的开句柄失败就会让基线老文件看起来「身份变了」，
// 被读成新下载而搬走 —— 那正是设计书风险 7 要防的误伤。
func (a Sample) equal(b Sample) bool {
	if a.Size != b.Size || !a.Mtime.Equal(b.Mtime) {
		return false
	}
	return a.Index == b.Index || a.Index == 0 || b.Index == 0
}

// sameIdentity 是「这两份读数说的是同一个东西」。基线否决与改名否决共用它，
// 但各自还叠了自己的判据，见 sameObject 与 Machine.noteRenameFrom。
func sameIdentity(a, b Sample) bool { return a.equal(b) }

// sameObject 回答「这个路径上还是不是当初那一个文件对象」，是基线否决的判据。
//
// 这里不能再用 equal：用户在下载目录里双击打开一份老文档、改一笔存盘，
// 字节数和时间戳都变了，可 fileIndex 没变 —— 那是同一个老文件被编辑，不是新下载。
// equal 会说「身份变了」，于是它被当成一次下载搬走；这才是本轮用户报的那一类误伤。
//
// 任何一侧读不到编号时退回 equal 那个弱判据（exFAT/UFD 没有序列号、开句柄被安全软件
// 拦都会给 0）。所以成立关系是「equal 成立 ⇒ sameObject 成立」：这条判据只会比原来
// 多挡下「同一个对象被改写」那一类，绝不会把原来挡得住的东西放开 ——
// 误伤的方向单向地朝「不搬」收敛，而那正是用户要的方向。
func sameObject(a, b Sample) bool {
	if a.Index == 0 || b.Index == 0 {
		return a.equal(b)
	}
	return a.Index == b.Index
}
