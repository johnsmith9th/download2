//go:build windows

package detect

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 这一组跑的是真文件 + 真采样 + 真时钟：状态机的判据到底能不能信，只有这里说得清。
// 替身跑绿了不代表 share=0 的占用探测真的会拦住半成品，也不代表改名对真的能被认出来。

func newRealMachine(t *testing.T, quiet time.Duration) *Machine {
	t.Helper()
	return New(Config{Quiet: quiet, MaxSettle: 10 * time.Minute},
		SamplerFunc(StatSample), LockerFunc(Locked), nil)
}

// waitFor 按生产的 500ms 节拍驱动真机器，直到出现动作或超过期限。
func waitFor(m *Machine, until time.Duration) []Outcome {
	deadline := time.Now().Add(until)
	var out []Outcome
	for time.Now().Before(deadline) {
		time.Sleep(tickEvery)
		out = append(out, m.Tick()...)
	}
	return out
}

// TestRealDownloadFlowIsClaimedExactlyOnce 走 Chrome/Edge/Firefox 的真实形态：
// 临时名写满内容 → 改名成最终名 → 静置 + 两次采样一致 + 没人占用 → 一次搬运请求。
func TestRealDownloadFlowIsClaimedExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	const quiet = 300 * time.Millisecond
	m := newRealMachine(t, quiet)

	old := filepath.Join(dir, "上次就下好的.zip")
	if err := os.WriteFile(old, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	// 启动基线：建基线这条路不比 mtime，已经在目录里的东西一个都不许搬。
	snap, err := Snapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Baseline(snap)

	// 一次完整下载。
	temp := filepath.Join(dir, "正在下.zip.crdownload")
	final := filepath.Join(dir, "下好了.zip")
	f, err := os.Create(temp)
	if err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvCreate, Path: temp})
	if _, err := f.Write(make([]byte, 1<<16)); err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvWrite, Path: temp})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// 静置窗口再长，临时名本身也绝不该被搬。
	if c := claims(waitFor(m, 4*quiet)); len(c) != 0 {
		t.Fatalf("临时名被当成完成: %+v", c)
	}

	if err := os.Rename(temp, final); err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvRenameFrom, Path: temp})
	m.OnEvent(Event{Kind: EvCreate, Path: final})

	out := waitFor(m, 6*quiet)
	c := claims(out)
	if len(c) != 1 {
		t.Fatalf("想要恰好一次搬运请求, 得到 %+v", out)
	}
	if c[0].Path != final {
		t.Errorf("请求搬的是 %q", c[0].Path)
	}
	if c[0].Sample.Size != 1<<16 || c[0].Sample.Index == 0 {
		t.Errorf("快照身份不全: %+v", c[0].Sample)
	}
	for _, o := range out {
		if o.Kind == OutAborted {
			t.Errorf("改名成功却记了取消: %+v", o)
		}
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("判定阶段不该动磁盘上的文件: %v", err)
	}

	// 老文件从头到尾没被动过，也没有被请求搬运。
	if _, err := os.Stat(old); err != nil {
		t.Errorf("基线老文件不见了: %v", err)
	}
	m.MarkClaimed(final)
	m.MarkMoving(final)
	m.MarkDone(final)
	if n := m.Pending(); n != 0 {
		t.Errorf("交还之后 Pending = %d, 想要 0", n)
	}
}

// 被独占持有的文件必须卡在占用那一条上 —— 这正是「判早了搬走半成品」的防线。
func TestRealHeldFileIsBlockedByLockProbe(t *testing.T) {
	dir := t.TempDir()
	const quiet = 300 * time.Millisecond
	m := newRealMachine(t, quiet)

	p := filepath.Join(dir, "还在写.bin")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	u16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(u16, windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("模拟独占写者失败: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = windows.CloseHandle(h)
		}
	}()
	m.OnEvent(Event{Kind: EvCreate, Path: p})

	out := waitFor(m, 8*quiet)
	if c := claims(out); len(c) != 0 {
		t.Fatalf("被独占写着的文件被判成可搬: %+v", c)
	}
	if r := m.BlockReason(p); r != "文件还被别的程序占用" {
		t.Errorf("卡住原因 = %q, 想要占用那一条（说明前三条都已成立）", r)
	}

	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	closed = true
	// 写者走了之后同一条判据必须放开，否则就是永远搬不动。
	if c := claims(waitFor(m, 6*quiet)); len(c) != 1 || c[0].Path != p {
		t.Fatalf("释放之后没能放行: %+v", c)
	}
}

// 往 Downloads 解压一整个文件夹：只该看到一个目录，一个文件都不许搬。
func TestRealDirectoryCopyIsNotSwept(t *testing.T) {
	dir := t.TempDir()
	const quiet = 300 * time.Millisecond
	m := newRealMachine(t, quiet)

	sub := filepath.Join(dir, "刚解压的一堆图")
	if err := os.MkdirAll(filepath.Join(sub, "更深一层"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(sub, "01.jpg"),
		filepath.Join(sub, "更深一层", "02.jpg"),
		filepath.Join(dir, "desktop.ini"),
		filepath.Join(dir, "~$开会用的.docx"),
	} {
		if err := os.WriteFile(p, make([]byte, 512), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := Snapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 两条进入路径都试：启动建基线，以及事件溢出之后的最宽重扫
	// （cutoff 推到一小时以前，等于「快照里的一切都算新到的」）。
	m.Baseline(snap)
	m.Scan(time.Now().Add(-time.Hour), snap)

	// 顶层事件（fsnotify 对拷进来的目录只给这一条）逐条喂进去。
	for _, e := range snap {
		m.OnEvent(Event{Kind: EvCreate, Path: e.Path})
	}
	if c := claims(waitFor(m, 8*quiet)); len(c) != 0 {
		t.Fatalf("目录/忽略清单里的东西被请求搬运: %+v", c)
	}
	if n := m.Pending(); n != 0 {
		t.Errorf("Pending = %d, 想要 0：这些路径不该留下任何候选", n)
	}
}

// 改名否决（T15.1）的全部依据就是这两条 NTFS 读数，所以这里量的不是我们的代码，
// 是文件系统本身：
//   - 重命名换名字不换对象 —— size / mtime / fileIndex 三个数一个都不动；
//   - 就地改写（O_TRUNC 那一类，双击打开改一笔存盘）还是同一个对象，只有字节数变了；
//   - 删掉重建（同名重新下载）换对象，fileIndex 一定是新的。
//
// 这三条要是有一条哪天不成立了，15.1 的判据就整体失效，这条用例会先红。
func TestRealRenameKeepsTheObjectAndRebuildReplacesIt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "原名.txt")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := StatSample(p)
	if err != nil {
		t.Fatal(err)
	}
	if before.Index == 0 {
		t.Skipf("这块盘读不到 fileIndex（非 NTFS，或开句柄被拦），改名否决在这里无从验证")
	}

	to := filepath.Join(dir, "换了名字.txt")
	if err := os.Rename(p, to); err != nil {
		t.Fatal(err)
	}
	after, err := StatSample(to)
	if err != nil {
		t.Fatal(err)
	}
	if after.Index != before.Index {
		t.Errorf("改名换了 fileIndex: %d → %d（那改名和重新下载就分不开了）", before.Index, after.Index)
	}
	if after.Size != before.Size {
		t.Errorf("改名换了字节数: %d → %d", before.Size, after.Size)
	}
	if !after.Mtime.Equal(before.Mtime) {
		t.Errorf("改名改了 mtime: %v → %v（sameIdentity 会因此认不出来，否决失效）", before.Mtime, after.Mtime)
	}

	// 就地改写：同一个对象，size 变了、index 没变 —— 这就是「老文档被编辑」的形态。
	if err := os.WriteFile(to, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := StatSample(to)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Size == after.Size {
		t.Fatal("就地改写没改字节数，这条用例的前提没了")
	}
	if edited.Index != after.Index {
		t.Errorf("就地改写换了 fileIndex: %d → %d（那基线否决会把编辑当成下载搬走）", after.Index, edited.Index)
	}

	// 删掉重建：换了对象。
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := StatSample(to)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Index == edited.Index {
		t.Errorf("同名重建没换 fileIndex: 两边都是 %d（那改名否决会把真下载一起挡掉）", rebuilt.Index)
	}
}

// 真文件版的两类误伤：改了名、编辑过的老文件都不许被搬走；
// 而真的重新下载一次同名文件必须照旧放行 —— 否决不能变成永久隐形。
func TestRealRenamedAndEditedOldFilesAreNotClaimed(t *testing.T) {
	dir := t.TempDir()
	const quiet = 300 * time.Millisecond
	m := newRealMachine(t, quiet)

	old := filepath.Join(dir, "发票.pdf")
	if err := os.WriteFile(old, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := Snapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Baseline(snap)

	renamed := filepath.Join(dir, "发票(1).pdf")
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvRenameFrom, Path: old})
	m.OnEvent(Event{Kind: EvCreate, Path: renamed})
	if c := claims(waitFor(m, 20*quiet)); len(c) != 0 {
		t.Fatalf("一次普通的重命名被当成下载: %+v", c)
	}
	if s, ok := m.StateOf(renamed); !ok || s != StateBaseline {
		t.Errorf("改名来的路径状态 = %s/%v, 想要 baseline", s, ok)
	}

	// 再就地编辑它（一个 Write 事件、字节数变了、对象没变）：还是不许搬。
	if err := os.WriteFile(renamed, make([]byte, 1<<15), 0o600); err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvWrite, Path: renamed})
	if c := claims(waitFor(m, 20*quiet)); len(c) != 0 {
		t.Fatalf("被编辑过的老文件被当成下载: %+v", c)
	}

	// 删掉重建 = 真的重新下载了一次。同一条判据必须放开，否则就是漏搬。
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamed, make([]byte, 1<<16), 0o600); err != nil {
		t.Fatal(err)
	}
	m.OnEvent(Event{Kind: EvCreate, Path: renamed})
	out := waitFor(m, 20*quiet)
	if c := claims(out); len(c) != 1 || c[0].Path != renamed {
		t.Fatalf("同名重建之后没能放行: %+v", out)
	}
}
