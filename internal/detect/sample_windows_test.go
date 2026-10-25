//go:build windows

package detect

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 这一组验证的是那两个对真实文件系统说话的功能函数（StatSample / Locked）。
// 状态机本身跑在替身上，判据能不能信全靠这里。
// 下面的每一条断言都是在本机 NTFS 上实测出来的，不是从文档推的。

func TestStatSampleRealFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.bin")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := StatSample(p)
	if err != nil {
		t.Fatalf("StatSample: %v", err)
	}
	if s.Size != 4096 {
		t.Errorf("Size = %d, 想要 4096", s.Size)
	}
	if s.IsDir {
		t.Error("普通文件被读成目录")
	}
	if s.Hidden {
		t.Error("普通文件被读成隐藏")
	}
	if d := time.Since(s.Mtime); d > time.Minute || d < -time.Minute {
		t.Errorf("Mtime = %v, 离现在太远（FILETIME 换算错了）", s.Mtime)
	}
	// index 是「同名重建」的唯一证据。本机 NTFS 实测必然非 0；
	// 哪天变成 0，同身份判定就退化成 size+mtime，老文件可能被误认成新下载 —— 所以钉住。
	if s.Index == 0 {
		t.Error("fileIndex 取不到：同身份判定退化")
	}
}

func TestStatSampleDirectoryAndHidden(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "刚解压的一堆图")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 目录也要能采样：采样失败会被状态机读成「文件没了」，那是最难查的失败形态。
	s, err := StatSample(sub)
	if err != nil {
		t.Fatalf("采目录: %v", err)
	}
	if !s.IsDir {
		t.Error("目录没被认出来 —— 往 Downloads 解压一整个文件夹就会被当文件搬走")
	}

	hidden := filepath.Join(dir, ".hidden")
	if err := os.WriteFile(hidden, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hiddenU16, err := windows.UTF16PtrFromString(hidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(hiddenU16, windows.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	s, err = StatSample(hidden)
	if err != nil {
		t.Fatalf("采隐藏文件: %v", err)
	}
	if !s.Hidden {
		t.Error("隐藏属性没读到：判据里的「非隐藏」形同虚设")
	}
}

// 下载器正拿着文件写的时候，两头都不能错：
// 采样报错会被读成「文件没了」而漏搬；探不出占用会把半成品搬走。
//
// 实测结论：FILE_READ_ATTRIBUTES 的开法不受共享模式约束（连 share=0 的独占写者都挡不住它），
// 所以正在下载的文件也能拿到完整身份；而 Locked 那条 share=0 的探测恰好相反，一定失败。
func TestStatSampleAndLockedAgainstHoldingWriters(t *testing.T) {
	cases := []struct {
		name  string
		share uint32
	}{
		{"只放开读共享（改名前占用最终名的典型形态）", windows.FILE_SHARE_READ},
		{"完全独占，谁都不放开", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "正在下.bin")
			if err := os.WriteFile(p, make([]byte, 1024), 0o600); err != nil {
				t.Fatal(err)
			}
			u16, err := windows.UTF16PtrFromString(p)
			if err != nil {
				t.Fatal(err)
			}
			h, err := windows.CreateFile(u16, windows.GENERIC_WRITE, c.share,
				nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatalf("模拟持有者失败: %v", err)
			}

			s, err := StatSample(p)
			if err != nil {
				t.Fatalf("被占用时采样失败: %v —— 状态机会把这个在传的文件当成已消失", err)
			}
			if s.Size != 1024 {
				t.Errorf("Size = %d, 想要 1024", s.Size)
			}
			if s.Index == 0 {
				t.Error("被占用时拿不到 fileIndex：稳定判据会白等一轮")
			}
			if !Locked(p) {
				t.Error("被独占写着却判成「没被占用」：判据会搬走半成品")
			}

			// 写者一走，两条判据都得立刻放开，否则就是永远搬不动。
			// 句柄主动关掉、不再靠 defer：句柄号会被系统回收重用，重复关闭可能关掉别人的。
			if err := windows.CloseHandle(h); err != nil {
				t.Fatal(err)
			}
			if Locked(p) {
				t.Error("已经没人开着了，还判成被占用")
			}
			s2, err := StatSample(p)
			if err != nil || s2.Size != 1024 {
				t.Fatalf("释放之后采样 = %+v, %v", s2, err)
			}
			// 同一个文件对象在释放前后必须给出同一个 index，否则「连续两次读数一致」永远达不成。
			if s2.Index != s.Index {
				t.Errorf("index 在释放前后变了: %d → %d", s.Index, s2.Index)
			}
		})
	}
}

func TestLockedOnFreeAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "free.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Locked(p) {
		t.Error("没人开着的文件被判成占用，工具就再也不会搬任何东西了")
	}
	if !Locked(filepath.Join(dir, "根本没这个文件.bin")) {
		t.Error("不存在的路径该按「占用」处理：判据宁可晚不可早")
	}
	if !Locked("") {
		t.Error("空路径必须按「占用」处理：路径为空是调用方的错，但失败只能往「不搬」那边走")
	}
}
