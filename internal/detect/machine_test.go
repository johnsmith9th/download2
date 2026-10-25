package detect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 所有用例都跑在内存假文件系统 + 假时钟上，路径只是字符串。
// 真实 OS 行为（StatSample / Locked）在 sample_windows_test.go 里单独验，不混在这儿。

const (
	quietMs  = 900
	settleMs = 600_000
	// 一次 tick 走 500ms，所以 10 分钟的预算是 1200 次 tick —— 用生产的两个数字推出来，
	// 不在测试里另立一套阈值。
	ticksPerSet = settleMs / 500
)

// tickEvery 是生产的采样节拍，测试照它走，不自己发明节奏。
const tickEvery = 500 * time.Millisecond

var errGone = errors.New("没了")

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

type fakeFS struct {
	readings map[string]Sample
	locked   map[string]bool
	gone     map[string]bool
	reads    map[string]int
	nextIdx  uint64
}

func newFS() *fakeFS {
	return &fakeFS{
		readings: map[string]Sample{},
		locked:   map[string]bool{},
		gone:     map[string]bool{},
		reads:    map[string]int{},
		nextIdx:  100,
	}
}

func (f *fakeFS) Sample(p string) (Sample, error) {
	f.reads[p]++
	if f.gone[p] {
		return Sample{}, errGone
	}
	s, ok := f.readings[p]
	if !ok {
		return Sample{}, errGone
	}
	return s, nil
}

func (f *fakeFS) Locked(p string) bool { return f.locked[p] }

// put 放一个普通文件进去；同一个路径反复 put 会换一个 index，
// 那就是「同名重建」的形态。
func (f *fakeFS) put(p string, size int64, mtime time.Time) Sample {
	f.nextIdx++
	s := Sample{Size: size, Mtime: mtime, Index: f.nextIdx}
	f.readings[p] = s
	return s
}

func (f *fakeFS) rm(p string) {
	delete(f.readings, p)
	delete(f.locked, p)
}

// growAt 让文件变长但身份不变：真实的下载就是同一个文件对象上单调追加。
func (f *fakeFS) growAt(p string, size int64, at time.Time) {
	s, ok := f.readings[p]
	if !ok {
		panic("growAt 用在一个不存在的文件上")
	}
	s.Size = size
	s.Mtime = at
	f.readings[p] = s
}

// move 是一次重命名：换名字，三个读数原样不动。
//
// 这不是为了凑测试而编的形态 —— realpath_windows_test.go 里那条真文件用例量的就是它
// （NTFS 上改名不换 fileIndex，也不改 LastWriteTime）。
func (f *fakeFS) move(old, newName string) Sample {
	s, ok := f.readings[old]
	if !ok {
		panic("move 用在一个不存在的文件上")
	}
	delete(f.readings, old)
	f.readings[newName] = s
	return s
}

func (h *harness) arrive(p string) { h.m.OnEvent(Event{Kind: EvCreate, Path: p}) }
func (h *harness) write(p string)  { h.m.OnEvent(Event{Kind: EvWrite, Path: p}) }
func (h *harness) gone(p string)   { h.m.OnEvent(Event{Kind: EvRemove, Path: p}) }

// renamedFrom 是改名对的左半边：fsnotify 把它和右半边的 Create 作为相邻两条事件给出。
func (h *harness) renamedFrom(p string) { h.m.OnEvent(Event{Kind: EvRenameFrom, Path: p}) }

// h 是三个替身 + 状态机。测试只经由事件与 tick 驱动它，绝不改内部字段。
type harness struct {
	t   *testing.T
	m   *Machine
	fs  *fakeFS
	clk *fakeClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	fs := newFS()
	m := New(Config{
		Quiet:     time.Duration(quietMs) * time.Millisecond,
		MaxSettle: time.Duration(settleMs) * time.Millisecond,
	}, fs, fs, clk)
	return &harness{t: t, m: m, fs: fs, clk: clk}
}

// file 在假目录里放一个 size 字节、mtime 就是「现在」的文件。
func (h *harness) file(p string, size int64) Sample {
	return h.fs.put(p, size, h.clk.Now())
}

// grow 模拟还在下：字节数变大，mtime 跟着推到现在，文件身份不变。
func (h *harness) grow(p string, size int64) {
	h.fs.growAt(p, size, h.clk.Now())
}

// setDir/setHidden 是「不是普通文件」的两种形态。
func (h *harness) setDir(p string, size int64) {
	s := h.fs.put(p, size, h.clk.Now())
	s.IsDir = true
	h.fs.readings[p] = s
}

func (h *harness) setHidden(p string, size int64) {
	s := h.fs.put(p, size, h.clk.Now())
	s.Hidden = true
	h.fs.readings[p] = s
}

func (h *harness) hold(p string) { h.fs.locked[p] = true }
func (h *harness) free(p string) { delete(h.fs.locked, p) }

func (h *harness) step(d time.Duration) { h.clk.add(d) }

// tickN 走 n 次 500ms 的时钟并收集全部 Outcome。
func (h *harness) tickN(n int) []Outcome {
	var out []Outcome
	for i := 0; i < n; i++ {
		h.clk.add(tickEvery)
		out = append(out, h.m.Tick()...)
	}
	return out
}

// settle 走到「静置窗口满 + 两次采样一致」必然成立的那一步：
// quietMs 需要至少 2 次 tick（900ms > 500ms），再多给一次冗余。
func (h *harness) settle() []Outcome { return h.tickN(3) }

// claims 挑出 Outcome 里的搬运请求。
func claims(out []Outcome) []Outcome {
	var c []Outcome
	for _, o := range out {
		if o.Kind == OutClaim {
			c = append(c, o)
		}
	}
	return c
}

func (h *harness) state(p string) State {
	h.t.Helper()
	s, ok := h.m.StateOf(p)
	if !ok {
		return StateDone // 没在追踪 = 已经了结
	}
	return s
}

func (h *harness) wantState(p string, want State) {
	h.t.Helper()
	if got := h.state(p); got != want {
		h.t.Fatalf("%s 状态 = %s, 想要 %s", p, got, want)
	}
}

func (h *harness) wantNoClaim(out []Outcome, why string) {
	h.t.Helper()
	if c := claims(out); len(c) != 0 {
		h.t.Fatalf("%s：居然要搬 %+v", why, c)
	}
}

func (h *harness) wantOneClaim(out []Outcome, p string) Outcome {
	h.t.Helper()
	c := claims(out)
	if len(c) != 1 || c[0].Path != p {
		h.t.Fatalf("想要恰好一个 %s 的搬运请求, 得到 %+v", p, out)
	}
	return c[0]
}

// —— 设计书 T4 的六条硬案子 ——

// ① 临时名改名即完成：改名对是 Chrome/Edge/Firefox 的完成信号。
// 改名之前那个最终名一个都不许搬；改名对到达、静置满、两次采样一致之后恰好搬一次。
func TestRenamePairIsTheCompletionSignal(t *testing.T) {
	h := newHarness(t)
	const temp, final = `D:\Downloads\a.zip.crdownload`, `D:\Downloads\a.zip`

	h.file(temp, 512)
	h.arrive(temp)
	h.step(200 * time.Millisecond)
	h.grow(temp, 4096)
	h.write(temp)

	// 还没改名：最终名连存在都不存在，绝不能有搬运请求。
	h.wantNoClaim(h.settle(), "改名之前")

	// 改名对：老名消失 + 新名出现，相邻两条到达。
	h.file(final, 4096)
	h.fs.rm(temp)
	h.renamedFrom(temp)
	h.arrive(final)

	// 改名当时静置窗口还没过。
	h.wantNoClaim(h.tickN(1), "改名后 500ms")

	out := h.settle()
	// 整批只有一条：改名成功时绝不能顺手记一条「下载被取消」。
	if len(out) != 1 {
		t.Fatalf("改名成功那一轮只该有一条搬运请求, 得到 %+v", out)
	}
	c := h.wantOneClaim(out, final)
	if c.Sample.Size != 4096 {
		t.Errorf("快照字节数 = %d, 想要 4096", c.Sample.Size)
	}
	if c.Sample.Index == 0 {
		t.Error("快照没带 fileIndex，崩溃重放就没法验身份")
	}
	h.wantState(final, StateReady)
}

// ② 单调变大的「4GB 型」文件绝不被 claim：大文件会连发几十秒的 Write，
// 任何一次动静都得把静置窗口推回去。
func TestMonotonicGrowthIsNeverClaimed(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\huge.iso`
	h.file(p, 1<<20)
	h.arrive(p)

	// 40 次「还在长」，每次 500ms —— 共 20 秒，早过静置阈值了。
	for i := 0; i < 40; i++ {
		h.step(tickEvery)
		h.grow(p, int64(1<<20)+int64(i+1)*(1<<17))
		h.write(p)
		h.wantNoClaim(h.m.Tick(), fmt.Sprintf("第 %d 次还在长", i))
		if s := h.state(p); s == StateReady || s == StateDone {
			t.Fatalf("第 %d 次居然就绪了", i)
		}
	}

	// 停止增长之后，同一条判据必须能正常放行 —— 否则就是永远搬不动。
	last := int64(1<<20) + 40*(1<<17)
	h.grow(p, last)
	c := h.wantOneClaim(h.settle(), p)
	if c.Sample.Size != last {
		t.Errorf("搬走的是 %d 字节, 想要 %d", c.Sample.Size, last)
	}
}

// ③ 预分配 size 稳定但仍被锁的绝不被 claim。
// Chrome 一开始就把最终大小占好，所以「字节数不再变」什么也证明不了。
func TestStableButLockedIsNeverClaimed(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\movie.mp4`
	h.file(p, 8<<20)
	h.hold(p)
	h.arrive(p)

	for i := 0; i < 6; i++ {
		h.wantNoClaim(h.tickN(1), fmt.Sprintf("被占用第 %d 轮", i))
	}
	// 卡在哪一条判据上要说得出口：界面与日志共用这句。
	if r := h.m.BlockReason(p); r != "文件还被别的程序占用" {
		t.Errorf("卡住原因 = %q, 想要占用那条", r)
	}
	// 采样稳定性必须已经达成，否则这条用例其实是在测静置窗口。
	h.free(p)
	h.wantOneClaim(h.settle(), p)
}

// ④ 占用重试预算耗尽转 Stuck：文件原地不动，只说一次。
func TestBudgetExhaustionGoesStuck(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\locked.zip`
	h.file(p, 4096)
	h.hold(p)
	h.arrive(p)

	out := h.tickN(int(ticksPerSet) + 5)
	var stuck []Outcome
	for _, o := range out {
		if o.Kind == OutStuck {
			stuck = append(stuck, o)
		}
	}
	h.wantNoClaim(out, "预算烧满也不该搬一个被占用的文件")
	if len(stuck) != 1 {
		t.Fatalf("Stuck 应当恰好一条, 得到 %+v", stuck)
	}
	if stuck[0].Path != p || stuck[0].Reason == "" {
		t.Errorf("Stuck 内容不对: %+v", stuck[0])
	}
	h.wantState(p, StateStuck)

	// 之后继续 tick 不能再刷屏。
	h.wantNoClaim(h.tickN(10), "Stuck 之后")

	// 解除占用、并且又收到一次动静：这是新的一次机会，预算从头算。
	h.free(p)
	h.write(p)
	if s, ok := h.m.StateOf(p); !ok || s == StateStuck {
		t.Errorf("重新有动静之后还卡在 %s", s)
	}
	h.wantOneClaim(h.settle(), p)
}

// ⑤ denylist：Office 属主锁文件、系统文件、目录、隐藏文件都不许搬。
// 它们全都「立刻就静置」，只看静置必然误搬。
func TestDenylistAndNonPlainFilesAreNeverClaimed(t *testing.T) {
	cases := []struct {
		name string
		p    string
		dir  bool
		hid  bool
	}{
		{"office 属主锁文件", `D:\Downloads\~$报表.docx`, false, false},
		{"desktop.ini", `D:\Downloads\desktop.ini`, false, false},
		{"thumbs.db", `D:\Downloads\Thumbs.db`, false, false},
		{"回收站", `D:\Downloads\$Recycle.Bin`, false, false},
		{"目录", `D:\Downloads\刚解压的一堆图`, true, false},
		{"隐藏文件", `D:\Downloads\.hidden`, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			switch {
			case c.dir:
				h.setDir(c.p, 4096)
			case c.hid:
				h.setHidden(c.p, 4096)
			default:
				h.file(c.p, 4096)
			}
			h.arrive(c.p)
			h.wantNoClaim(h.tickN(int(ticksPerSet)+5), c.name)
			if s, ok := h.m.StateOf(c.p); ok && s != StateStuck {
				t.Errorf("%s 还挂在状态机里: %s", c.name, s)
			}
		})
	}
}

// ⑥ 基线绝不清扫老文件：启动时已经躺在 Downloads 里的东西一个都不搬。
// 这一条错了就是「第一次运行把整个下载目录搬空」。
func TestBaselineIsNeverSwept(t *testing.T) {
	h := newHarness(t)
	old := h.clk.Now().Add(-72 * time.Hour)
	paths := []string{
		`D:\Downloads\去年就装好的.exe`,
		`D:\Downloads\发票.pdf`,
		`D:\Downloads\照片.zip`,
	}
	var snap []ScanEntry
	for _, p := range paths {
		s := h.fs.put(p, 1<<20, old)
		snap = append(snap, ScanEntry{Path: p, Sample: s})
	}

	// 启动走 Baseline 而不是 Scan：建基线不跟时钟较劲，见 Baseline 的注释。
	h.m.Baseline(snap)
	h.wantNoClaim(h.tickN(int(ticksPerSet)*2), "基线老文件")
	for _, p := range paths {
		h.wantState(p, StateBaseline)
	}

	// 老文件被别的程序碰了一下（事件来了、身份没变）：还是不许搬。
	h.arrive(`D:\Downloads\发票.pdf`)
	h.wantNoClaim(h.tickN(4), "身份没变的重新通知")
	h.wantState(`D:\Downloads\发票.pdf`, StateBaseline)

	// 同名重建（index 变了）才是真正的下载。
	rebuilt := h.file(`D:\Downloads\发票.pdf`, 3<<20)
	if rebuilt.Index == 0 {
		t.Fatal("假文件系统没给新身份编号")
	}
	h.arrive(`D:\Downloads\发票.pdf`)
	c := h.wantOneClaim(h.settle(), `D:\Downloads\发票.pdf`)
	if c.Sample.Size != 3<<20 {
		t.Errorf("搬走的字节数 = %d", c.Sample.Size)
	}
}

// 启动时撞上的那个洞：文件 mtime 可能比我们读到的时钟还晚几毫秒（本机实测过一次，
// 写入返回之后才落下的时间戳）。任何「拿 mtime 和启动时刻比大小来判新不新」的写法
// 都会在这种时刻把这些老文件搬走，而且每次启动都可能命中不同的那一两个。
// Baseline 那条路上根本没有时钟参与判断，所以没有这个洞 —— 这一条钉住它别回来。
func TestBaselineDoesNotArgueWithTheClock(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\上次就下好的.zip`
	drifted := h.fs.put(p, 1<<16, h.clk.Now().Add(6*time.Millisecond))
	h.m.Baseline([]ScanEntry{{Path: p, Sample: drifted}})

	// 甚至再喂一条 Create 进来（重复通知、别的程序碰了它）：身份没变就还是不许搬。
	h.arrive(p)
	h.wantNoClaim(h.tickN(int(ticksPerSet)+5), "mtime 漂到未来的老文件")
	h.wantState(p, StateBaseline)
}

// TestReclaimPullsACrashedClaimBackIntoObservation 是崩溃恢复的入口。
//
// 重放表对「源还在、目标未就位」给的是 rewatch，而要真回到观察就得先把基线那票否决证据清掉。
// 这一条测的就是这个先后顺序：基线盖过之后再 Reclaim，它才应该被重新判成一次完成。
func TestReclaimPullsACrashedClaimBackIntoObservation(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\搬一半.zip`
	s := h.fs.put(p, 1<<16, h.clk.Now().Add(-time.Minute))

	h.m.Baseline([]ScanEntry{{Path: p, Sample: s}})
	h.wantState(p, StateBaseline)
	h.wantNoClaim(h.tickN(10), "被基线盖住的崩一半文件")

	h.m.Reclaim(p, s)
	// 静置早就够了（mtime 在一分钟前），差的只是连续两次采样一致 —— 那就是两个节拍。
	h.wantOneClaim(h.tickN(4), p)
}

// —— 判据之外的边角：临时名存活、取消、0 字节、回调、重扫 ——

func TestLiveTempBlocksFinalName(t *testing.T) {
	h := newHarness(t)
	const temp, final = `D:\Downloads\b.zip.crdownload`, `D:\Downloads\b.zip`

	// 有的下载器先把最终名占上、再写临时名。这时候最终名一个都不许搬。
	h.file(final, 4096)
	h.arrive(final)
	h.file(temp, 4096)
	h.arrive(temp)

	h.wantNoClaim(h.tickN(6), "临时名还挂着")
	if r := h.m.BlockReason(final); r != "它的临时名还挂着" {
		t.Errorf("卡住原因 = %q", r)
	}

	// 临时名改名过去：阻塞解除，而且因为最终名早已被追踪，不许报成取消。
	h.renamedFrom(temp)
	h.write(final)
	out := h.settle()
	h.wantOneClaim(out, final)
	for _, o := range out {
		if o.Kind == OutAborted {
			t.Errorf("改名成功却记了一条取消: %+v", o)
		}
	}
}

func TestCancelledDownloadIsLoggedExactlyOnce(t *testing.T) {
	h := newHarness(t)
	const temp, final = `D:\Downloads\c.mp4.part`, `D:\Downloads\c.mp4`

	h.file(temp, 1<<20)
	h.arrive(temp)
	h.wantNoClaim(h.tickN(2), "还在下")

	h.gone(temp)
	out := h.tickN(1)
	if len(out) != 1 || out[0].Kind != OutAborted {
		t.Fatalf("想要恰好一条取消, 得到 %+v", out)
	}
	if out[0].Path != final {
		t.Errorf("取消记的是 %q, 想要最终名 %q", out[0].Path, final)
	}
	if !strings.Contains(out[0].Reason, temp) {
		t.Errorf("取消的原因里没带上临时名 %q: %q", temp, out[0].Reason)
	}
	if len(h.tickN(3)) != 0 {
		t.Error("取消被记了不止一次")
	}
	if h.m.Pending() != 0 {
		t.Errorf("取消之后还挂着 %d 条追踪", h.m.Pending())
	}
}

// 下载器改名之后叫什么，我们无从知道（Chrome 去掉后缀，别的工具顺手把名也改了）。
// 所以「临时名消失 + 同一窗口里冒出一个不认识的新名字」必须当成改名成功，
// 不能报「下载未完成」——那条假话会指着刚搬走的文件说它失败了。
func TestRenameToAnArbitraryNameIsNotAnAbort(t *testing.T) {
	h := newHarness(t)
	const temp, other = `D:\Downloads\安装包.exe.crdownload`, `D:\Downloads\已重命名.exe`

	h.file(temp, 1<<20)
	h.arrive(temp)
	h.wantNoClaim(h.tickN(2), "还在下")

	h.fs.put(other, 1<<20, h.clk.Now())
	h.renamedFrom(temp)
	h.arrive(other) // 改名对的右半边：一个我们完全没预料到的名字

	out := h.tickN(int(ticksPerSet) + 5)
	h.wantOneClaim(out, other)
	for _, o := range out {
		if o.Kind == OutAborted {
			t.Errorf("改名成功却记了取消: %+v", o)
		}
	}

	// 消音只吃一个窗口：下一次单独的取消照样要说。
	h.fs.rm(temp)
	h.gone(temp)
	h.step(tickEvery)
	h.gone(other) // 搬完了我们自己删的，不算新名字
	var got []Outcome
	for _, o := range h.tickN(2) {
		if o.Kind == OutAborted {
			got = append(got, o)
		}
	}
	if len(got) != 1 || got[0].Path != `D:\Downloads\安装包.exe` {
		t.Fatalf("想要一条单独的取消流水, 得到 %+v", got)
	}
}

// 0 字节的最终名很可疑：盯住它、说明它为什么没动，但绝不搬。
func TestZeroByteFileIsWatchedButNeverMoved(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\空壳.zip`
	h.file(p, 0)
	h.arrive(p)

	for i := 0; i < 4; i++ {
		h.wantNoClaim(h.tickN(1), "0 字节")
	}
	if r := h.m.BlockReason(p); r != "文件还是 0 字节" {
		t.Errorf("卡住原因 = %q", r)
	}
	if n := countKind(h.tickN(int(ticksPerSet)+5), OutStuck); n != 1 {
		t.Errorf("Stuck 条数 = %d, 想要 1", n)
	}

	// 之后被写上内容，就该正常放行。
	h.fs.growAt(p, 2048, h.clk.Now())
	h.write(p)
	h.wantOneClaim(h.settle(), p)
}

// 搬运器失败回来重试：预算起点不动，所以最终一定会 Stuck，不会永远在试。
func TestMoveFailureReturnsToJudging(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\d.zip`
	h.file(p, 4096)
	h.arrive(p)

	h.wantOneClaim(h.settle(), p)
	h.m.MarkClaimed(p)
	h.wantState(p, StateClaimed)
	h.m.MarkMoving(p)
	h.wantState(p, StateMoving)

	h.hold(p)
	h.m.MarkFailed(p, "改名被占用")
	if got := h.m.TriesOf(p); got != 1 {
		t.Errorf("tries = %d, 想要 1", got)
	}
	h.wantState(p, StateSettling)
	// 静置基准重新立过，所以不能立刻再搬一次。
	h.wantNoClaim(h.tickN(1), "刚失败")

	out := h.tickN(int(ticksPerSet) + 5)
	h.wantNoClaim(out, "一直被占用的文件不该重试成功")
	if n := countKind(out, OutStuck); n != 1 {
		t.Errorf("Stuck 条数 = %d, 想要 1", n)
	}

	// 成功那条路：搬完就不该再被追踪。
	h.free(p)
	h.arrive(p)
	h.wantOneClaim(h.settle(), p)
	h.m.MarkDone(p)
	if _, ok := h.m.StateOf(p); ok {
		t.Error("MarkDone 之后还在追踪")
	}
	if h.m.Pending() != 0 {
		t.Errorf("Pending = %d, 想要 0", h.m.Pending())
	}
}

func TestVanishedFileStopsBeingTracked(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\e.zip`
	h.file(p, 4096)
	h.arrive(p)
	h.step(tickEvery)
	h.fs.rm(p)

	if len(h.m.Tick()) != 0 {
		t.Fatal("文件没了还能产出动作")
	}
	if _, ok := h.m.StateOf(p); ok {
		t.Error("采样失败之后还挂着追踪")
	}
}

// 事件流残缺之后的重扫：漏掉 Create 的新文件要补成候选，老文件还是一个都不许动。
func TestRescanAfterOverflowReentersFromSnapshot(t *testing.T) {
	h := newHarness(t)
	start := h.clk.Now()
	fresh := `D:\Downloads\刚下好的.pdf`
	old := `D:\Downloads\上个月的东西.iso`

	h.fs.put(old, 1<<20, start.Add(-time.Hour))
	h.fs.put(fresh, 5<<20, start.Add(-time.Minute))
	// 启动先正常建基线，之后才谈得上溢出重扫。
	h.m.Baseline([]ScanEntry{
		{Path: old, Sample: h.fs.readings[old]},
		{Path: fresh, Sample: h.fs.readings[fresh]},
	})
	h.wantState(old, StateBaseline)
	h.wantState(fresh, StateBaseline)

	h.m.Scan(start.Add(-5*time.Minute), []ScanEntry{
		{Path: old, Sample: h.fs.readings[old]},
		{Path: fresh, Sample: h.fs.readings[fresh]},
	})
	out := h.settle()
	if c := claims(out); len(c) != 1 || c[0].Path != fresh {
		t.Fatalf("想要恰好一条 %s 的搬运请求, 得到 %+v", fresh, out)
	}

	// 快照里发现某个基线路径被重写过（没收到 Create 也要认下来）。
	h.fs.put(old, 9<<20, h.clk.Now())
	h.m.Scan(start.Add(-5*time.Minute), []ScanEntry{{Path: old, Sample: h.fs.readings[old]}})
	h.wantOneClaim(h.settle(), old)
}

func TestPendingCountsOnlyAdvancingWork(t *testing.T) {
	h := newHarness(t)
	const temp, final, stuck = `D:\Downloads\f.bin.part`, `D:\Downloads\f.bin`, `D:\Downloads\g.bin`

	h.file(temp, 1<<10)
	h.arrive(temp)
	if h.m.LiveDownloads() != 1 {
		t.Fatal("存活的临时名没被数到")
	}

	h.arrive(final)
	h.file(final, 1<<10)
	if h.m.Pending() != 1 {
		t.Errorf("Pending = %d, 想要 1", h.m.Pending())
	}

	// 改名对之后交给搬运器：两条计数都该看清在途。
	h.renamedFrom(temp)
	h.wantOneClaim(h.settle(), final)
	h.m.MarkClaimed(final)
	h.m.MarkMoving(final)
	if h.m.Pending() != 1 || h.m.LiveDownloads() != 0 {
		t.Errorf("在途计数 = %d/%d, 想要 1/0", h.m.Pending(), h.m.LiveDownloads())
	}
	h.m.MarkDone(final)

	// Stuck 的文件不再推进，不该把界面永远点亮成忙。
	h.file(stuck, 0)
	h.arrive(stuck)
	h.tickN(int(ticksPerSet) + 5)
	h.wantState(stuck, StateStuck)
	if h.m.Pending() != 0 {
		t.Errorf("Pending = %d, 想要 0（Stuck 的不算）", h.m.Pending())
	}
}

// 就绪之前又来一次 Write：静置窗口必须重新算，不然「边下边改名」的文件会被抢搬。
func TestLateWritePushesTheQuietWindowBack(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\h.zip`
	h.file(p, 100)
	h.arrive(p)

	h.tickN(1) // 500ms：还不够静置
	h.fs.growAt(p, 200, h.clk.Now())
	h.write(p)

	h.wantNoClaim(h.tickN(1), "又来了一次 Write")
	if r := h.m.BlockReason(p); r != "还在写入" {
		t.Errorf("卡住原因 = %q, 想要静置那条", r)
	}
	h.wantOneClaim(h.settle(), p)
}

// —— 用户第一轮试用报的那一类误伤（T15.1）——
//
// 一次普通的「重命名」和一次下载完成在事件流上是同一个形状：Rename(老) + Create(新)。
// 而基线那票否决只比同一路径的身份 —— 名字一换就绕过去了。
// 这两条用例把「靠什么分开它们」钉住：改名不换 fileIndex，新下载一定换。

// 用户在资源管理器里给一份老文件换个名字，搬走它就是错。
// 改名对到达之后要给足静置、稳定、预算 —— 全程一次都不许claim。
func TestRenamedOldFileIsNotADownload(t *testing.T) {
	h := newHarness(t)
	const old, renamed = `D:\Downloads\发票.pdf`, `D:\Downloads\发票(1).pdf`
	oldTime := h.clk.Now().Add(-48 * time.Hour)
	h.fs.put(old, 1<<18, oldTime)
	h.m.Baseline([]ScanEntry{{Path: old, Sample: h.fs.readings[old]}})

	h.fs.move(old, renamed)
	h.renamedFrom(old)
	h.arrive(renamed)

	h.wantNoClaim(h.tickN(int(ticksPerSet)*2), "换了名字的老文件")
	h.wantState(renamed, StateBaseline)
	if h.m.BlockReason(renamed) != "" {
		t.Errorf("被否决的路径不该有卡住原因: %q", h.m.BlockReason(renamed))
	}
	if h.m.Pending() != 0 {
		t.Errorf("Pending = %d, 想要 0（改名否决不该留下活儿）", h.m.Pending())
	}

	// 再编辑一下它（同一个对象，字节数变了）：还是不许搬，见 sameObject。
	h.fs.growAt(renamed, 3<<18, h.clk.Now())
	h.write(renamed)
	h.wantNoClaim(h.tickN(6), "改名之后又被编辑")
	h.wantState(renamed, StateBaseline)
}

// 否决不能把真下载挡在门外：改名走了一条之后紧跟着一份新下载，
// fileIndex 是新的，照旧要被判成完成。这条是上一条的存在理由 ——
// 只按「窗口里有过改名就整批否决」那种偷懒写法，就会在这儿红。
func TestFreshDownloadAfterARenameIsStillClaimed(t *testing.T) {
	h := newHarness(t)
	const old, renamed, fresh = `D:\Downloads\a.txt`, `D:\Downloads\b.txt`, `D:\Downloads\新下的.zip`
	h.fs.put(old, 4096, h.clk.Now().Add(-time.Hour))
	h.m.Baseline([]ScanEntry{{Path: old, Sample: h.fs.readings[old]}})

	h.fs.move(old, renamed)
	h.renamedFrom(old)
	h.arrive(renamed)
	h.wantState(renamed, StateBaseline)

	h.file(fresh, 8192)
	h.arrive(fresh)
	h.wantOneClaim(h.settle(), fresh)
}

// 双击打开下载目录里那份老文档、改一笔存盘：一个 Write 事件、字节数和时间戳都变了，
// 可它还是同一个文件对象（fileIndex 没动），所以不是新下载。
func TestOldFileEditedInPlaceIsNotADownload(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\合同.docx`
	h.fs.put(p, 1<<16, h.clk.Now().Add(-24*time.Hour))
	h.m.Baseline([]ScanEntry{{Path: p, Sample: h.fs.readings[p]}})

	h.grow(p, 3<<16) // 存盘：同一个对象上变长
	h.write(p)
	h.wantNoClaim(h.tickN(int(ticksPerSet)+5), "被编辑过的老文档")
	h.wantState(p, StateBaseline)
}

// 同名重建（真的重新下载了一次）必须照旧能搬 —— 上一那条的判据是 fileIndex，
// 而重建换 index，所以它没被误伤。见 T4 硬案子⑥ 里同名的那一半。
func TestRebuiltSameNameIsNotBlockedByTheObjectVeto(t *testing.T) {
	h := newHarness(t)
	const p = `D:\Downloads\发票.pdf`
	h.fs.put(p, 1<<16, h.clk.Now().Add(-24*time.Hour))
	h.m.Baseline([]ScanEntry{{Path: p, Sample: h.fs.readings[p]}})

	h.file(p, 1<<17) // put 会换一个新 index：同名重建的形态
	h.arrive(p)
	h.wantOneClaim(h.settle(), p)
}

// 改名否决的证据会过期，也会挤满。这两条预算是给「永远搬不走」设的下限：
// 没有它们，一次改名会让这个文件对象在此之后每一次出现都被否决掉。
func TestRenameVetoExpiresAndIsCapped(t *testing.T) {
	t.Run("窗口之外证据作废", func(t *testing.T) {
		h := newHarness(t)
		const old, renamed = `D:\Downloads\c.txt`, `D:\Downloads\d.txt`
		h.fs.put(old, 2048, h.clk.Now().Add(-time.Hour))
		h.m.Baseline([]ScanEntry{{Path: old, Sample: h.fs.readings[old]}})
		h.fs.move(old, renamed)
		h.renamedFrom(old)

		// 改名对的两条事件本该同批到达（上一条用例就是那样）；被一整个文件夹的拷贝
		// 卡住时才会隔开发。隔过 renameVetoWindow 再到的右半边，我们不再认它。
		// 这一条断言的是「作废之后按常规重新判」，也就是会被搬走 ——
		// 那是这条窗口的代价，写进 README 的未验清单，不在这里粉饰。
		h.tickN(int(renameVetoWindow/tickEvery) + 1)
		h.arrive(renamed)
		h.wantState(renamed, StateWatching)
		h.wantOneClaim(h.settle(), renamed)
	})

	t.Run("批量改名只留最近那些", func(t *testing.T) {
		h := newHarness(t)
		n := renameVetoCap + 1
		for i := 0; i < n; i++ {
			p := fmt.Sprintf(`D:\Downloads\老%03d.txt`, i)
			h.fs.put(p, int64(1000+i), h.clk.Now().Add(-time.Hour))
			h.m.Baseline([]ScanEntry{{Path: p, Sample: h.fs.readings[p]}})
		}
		// 全选重命名一次，条数比上限多一条：最早那条记录被挤掉，最近的还认得。
		for i := 0; i < n; i++ {
			old := fmt.Sprintf(`D:\Downloads\老%03d.txt`, i)
			h.fs.move(old, fmt.Sprintf(`D:\Downloads\新%03d.txt`, i))
			h.renamedFrom(old)
		}
		first := `D:\Downloads\新000.txt`
		h.arrive(first)
		h.wantState(first, StateWatching)

		last := fmt.Sprintf(`D:\Downloads\新%03d.txt`, n-1)
		h.arrive(last)
		h.wantState(last, StateBaseline)
	})
}

func countKind(out []Outcome, k OutcomeKind) int {
	n := 0
	for _, o := range out {
		if o.Kind == k {
			n++
		}
	}
	return n
}
