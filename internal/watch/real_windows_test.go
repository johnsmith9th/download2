//go:build windows

package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"download2/internal/detect"
)

// 这一组跑的是真监听、真文件、真时钟。
//
// watch_test.go 用假流把「形状」演清楚了（溢出、通道关闭、事件与节拍的先后），
// 但有四件事只有真文件系统能回答，而整个项目就压在它们上面：
//  1. Chrome 那一次改名，ReadDirectoryChangesW 到底给回什么事件对、什么顺序；
//  2. 这台机器实际开出来的缓冲区是哪一档，阶梯降级是不是真的会退；
//  3. 一次 5000 个 Create 的 burst 在默认阶梯下丢不丢；
//  4. 事件真的静默丢失之后，对账重扫那条路能不能把文件全找回来。
//
// 答错的代价不是「测试红」，是用户下载好的文件永远躺在 Downloads 里不动。

var payload = make([]byte, 256)

// waitReal 是真文件版的等待：内核通知有延迟、mtime 有精度，
// 替身测试那种 2 秒期限在这里只会造出假失败。
func waitReal(t *testing.T, lim time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lim)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等不到：%s（期限 %s 已过）", what, lim)
}

// runReal 用生产入口 New 起一条真监听，把实际缓冲区大小说出来，并负责收摊。
// 走 New 而不是 openDir 是刻意的：阶梯、默认值、目录校验那条路都得被真跑一遍。
func runReal(t *testing.T, cfg Config, h Handler) *Runner {
	t.Helper()
	r, err := New(cfg, h)
	if err != nil {
		t.Fatalf("New(%s): %v", cfg.Dir, err)
	}
	t.Logf("监听 %s ｜ 缓冲区 %d 字节 ｜ Tick %s ｜ Sweep %s", r.Dir(), r.BufferSize(), cfg.Tick, cfg.Sweep)
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	t.Cleanup(func() {
		if err := r.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
		// Stop 关事件流，循环那条路就是 ErrStopped；别的返回值都说明收摊不干净。
		if err := <-done; err != nil && !errors.Is(err, ErrStopped) {
			t.Errorf("Run 返回 %v, 想要 ErrStopped", err)
		}
	})
	return r
}

// realHub 是 T7 将来那个正式 Handler 的雏形：事件转交机器、节拍驱动判定、
// OnLost 重扫目录。判定产出的 OutClaim 在这里被「假装搬走」（删源文件），
// 否则下一轮对账会把它重新判成一次新下载，测试就在自转。
type realHub struct {
	mu         sync.Mutex
	busy       bool
	violations int

	m     *detect.Machine
	dir   string
	start time.Time

	events []detect.Event
	claims map[string]int
	nAbort int
	nStuck int
	nScan  int
	lost   []error
	failed []string // 假装搬运时删源失败的路径，以及重扫失败的原因
}

func newRealHub(t *testing.T, dir string, quiet time.Duration) *realHub {
	t.Helper()
	h := &realHub{
		m:      detect.New(detect.Config{Quiet: quiet, MaxSettle: 2 * time.Minute}, detect.SamplerFunc(detect.StatSample), detect.LockerFunc(detect.Locked), nil),
		dir:    dir,
		start:  time.Now(),
		claims: map[string]int{},
	}
	// 启动基线走真 Snapshot：目录刚建好，快照空，但这条路必须被真跑一次。
	snap, err := detect.Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", dir, err)
	}
	h.m.Baseline(snap)
	return h
}

// guard 是单 goroutine 不变式的探针：真后端自带一条 IO 线程，
// 要是事件、节拍、投进来的回调能被并发调进来，这里就会记下一笔。
//
// 用法是 defer h.guard()() —— defer 会立刻执行 guard()，所以整个回调期间这把锁一直被握着，
// 回调函数体里就**不得**再 Lock 一次（Go 的互斥锁不可重入，第二次就是自锁死）。
// 只有测试线程那一侧的读方法才自己 Lock。
func (h *realHub) guard() func() {
	h.mu.Lock()
	if h.busy {
		h.violations++
	}
	h.busy = true
	return func() { h.busy = false; h.mu.Unlock() }
}

func (h *realHub) OnEvent(ev detect.Event) {
	defer h.guard()()
	h.events = append(h.events, ev)
	h.m.OnEvent(ev)
}

func (h *realHub) OnTick() {
	defer h.guard()()
	for _, o := range h.m.Tick() {
		switch o.Kind {
		case detect.OutClaim:
			h.m.MarkClaimed(o.Path)
			h.m.MarkMoving(o.Path)
			err := os.Remove(o.Path)
			h.m.MarkDone(o.Path)
			h.claims[o.Path]++
			if err != nil {
				h.failed = append(h.failed, o.Path+": "+err.Error())
			}
		case detect.OutAborted:
			h.nAbort++
		case detect.OutStuck:
			h.nStuck++
		}
	}
}

// OnLost 就是设计书里那句话的落地：这一段事件不可信 → 重扫顶层 → 交给 Machine.Scan。
// 溢出、目录被删、以及 DefaultSweep 的定期对账，处置完全一样。
func (h *realHub) OnLost(err error) {
	defer h.guard()()
	h.lost = append(h.lost, err)

	snap, serr := detect.Snapshot(h.dir)
	if serr != nil {
		h.failed = append(h.failed, "Snapshot: "+serr.Error())
		return
	}
	h.nScan++
	h.m.Scan(h.start.Add(-10*time.Minute), snap)
}

func (h *realHub) claimsOf(p string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.claims[p]
}

func (h *realHub) claimsTotal() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.claims {
		n += c
	}
	return n
}

func (h *realHub) aborts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nAbort
}

func (h *realHub) stuck() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nStuck
}

func (h *realHub) scans() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nScan
}

func (h *realHub) eventCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

func (h *realHub) lostCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.lost)
}

func (h *realHub) diag(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("%s: 事件 %d 条, 判过 %d 个路径, 取消 %d, 卡住 %d, 重扫 %d, OnLost %d, 失败 %v, 重入 %d",
		name, len(h.events), len(h.claims), h.nAbort, h.nStuck, h.nScan, len(h.lost), h.failed, h.violations)
}

func (h *realHub) checkQuiet(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.violations != 0 {
		t.Errorf("回调重入 %d 次：循环不是单 goroutine 的", h.violations)
	}
	if len(h.failed) != 0 {
		t.Errorf("Hub 里有失败记录: %v", h.failed)
	}
}

// indexOf 返回第一条 kind+path 的下标，没有则 -1。
func (h *realHub) indexOf(kind detect.EventKind, path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, e := range h.events {
		if e.Kind == kind && e.Path == path {
			return i
		}
	}
	return -1
}

// TestRealRenamePairArrivesAsRenameThenCreate 钉住整个判定赖以成立的那一个事实：
// 一次改名在 Windows 上到达的是 Rename(老名) 紧跟 Create(新名) 这一对，
// 而不是别的东西（比如单个 Create，或者顺序反过来）。
//
// 这条一旦不成立，「改名对 = 下载完成」的判据就得重设计：
// fsnotify v1.10.1 不把改名来源暴露出来（见 classify_test 的那条反射钉），
// 配对只能靠到达顺序。
func TestRealRenamePairArrivesAsRenameThenCreate(t *testing.T) {
	dir := t.TempDir()
	const quiet = 200 * time.Millisecond
	h := newRealHub(t, dir, quiet)
	runReal(t, Config{Dir: dir, Tick: 50 * time.Millisecond, Sweep: -1}, h)

	temp := filepath.Join(dir, "movie.mp4.crdownload")
	final := filepath.Join(dir, "movie.mp4")
	if err := os.WriteFile(temp, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	waitReal(t, 10*time.Second, "临时名的 Create 到达", func() bool {
		return h.indexOf(detect.EvCreate, temp) >= 0
	})

	if err := os.Rename(temp, final); err != nil {
		t.Fatalf("改名: %v", err)
	}

	waitReal(t, 10*time.Second, "改名对的两条都到达", func() bool {
		return h.indexOf(detect.EvRenameFrom, temp) >= 0 && h.indexOf(detect.EvCreate, final) >= 0
	})
	from, to := h.indexOf(detect.EvRenameFrom, temp), h.indexOf(detect.EvCreate, final)
	t.Logf("事件下标：Rename(%s)=%d, Create(%s)=%d", filepath.Base(temp), from, filepath.Base(final), to)
	if from > to {
		t.Errorf("Rename(老名) 排在 Create(新名) 后面（%d > %d）：靠到达顺序配对改名对的前提垮了", from, to)
	}

	waitReal(t, 20*time.Second, "最终名被判成可搬", func() bool { return h.claimsOf(final) > 0 })
	if n := h.claimsOf(final); n != 1 {
		t.Errorf("%s 被判了 %d 次搬运请求, 想要恰好一次", final, n)
	}
	if n := h.claimsTotal(); n != 1 {
		t.Errorf("一共判了 %d 个路径, 想要只有 %s 一个: %s", n, final, h.diag("判定过多"))
	}
	// 改名左半边不是取消。这是代价最高的一种谎话：
	// 用户会看见「下载未完成」正贴在他刚下好的文件上。
	if n := h.aborts(); n != 0 {
		t.Errorf("记了 %d 次下载取消, 想要 0 次: %s", n, h.diag("改名被当成取消"))
	}
	h.checkQuiet(t)
}

// TestRealBufferSizeIsTheBiggestTheMachineTakes 回答「这台机器实际用上哪一档」，
// 并且钉住阶梯降级：第一档开不动要退到下一档，而不是整个监听开不起来。
//
// 为什么非要大缓冲区：Windows 上 ReadDirectoryChangesW 灌满之后是**静默**丢的
// （fsnotify 只在 n==0 时报 ErrEventOverflow），而 fsnotify 自己的注释说 64K 是
// 「所有文件系统都能接受的最大值」，所以不能硬写一个更大的数，只能退着试。
//
// 实测（本机 2026-10-07）：阶梯第一档 8MB 在本地 NTFS 上真开得起来；
// 而 WithBufferSize 只校验 4096 的下限，上限一概不拦（1 GiB 都接受），
// 所以阶梯不是在「探测系统肯给多大」，是一条我们自己的政策：
// 缓冲区是每个监听实打实占住的内核内存，8MB 够吞下一整个文件夹的拷贝，再大就是浪费；
// 64K 那一档留给重定向器/网络盘那种会把大数拒掉的挂载。
func TestRealBufferSizeIsTheBiggestTheMachineTakes(t *testing.T) {
	prod := t.TempDir()
	r := runReal(t, Config{Dir: prod, Tick: time.Hour, Sweep: -1}, &recorder{})
	got := r.BufferSize()
	if !slices.Contains(bufferSizes, got) {
		t.Errorf("缓冲区 %d 不在阶梯 %v 里", got, bufferSizes)
	}
	if got < 64<<10 {
		t.Errorf("缓冲区只有 %d 字节, 比 fsnotify 默认的 64K 还小：拷一整个文件夹就会静默丢事件", got)
	}
	t.Logf("本机实际用上 %d 字节（阶梯从大到小 %v）", got, bufferSizes)

	fallback := t.TempDir()
	src, size, err := openDir(fallback, []int{1024, 4096})
	if err != nil {
		t.Fatalf("阶梯降级失败: %v", err)
	}
	defer src.Close()
	if size != 4096 {
		t.Errorf("1024 字节被拒之后用的是 %d, 想要 4096", size)
	} else {
		t.Logf("1024 字节被拒 → 退到 4096，阶梯降级按预期工作")
	}

	// 整条阶梯都不合用时要把错误报出去，而不是悄悄退回一个「能用的默认值」：
	// 一个看起来开好了、其实一直在静默丢事件的监听，是最难查的那种。
	if _, _, err := openDir(fallback, []int{1024, 512}); err == nil {
		t.Errorf("整条阶梯都低于 4096 还开成功了：阶梯尽头没报错，等于对调用方撒谎")
	} else {
		t.Logf("阶梯用尽后的报错：%v", err)
	}
}

// countHub 只数事件，不碰任何机器：这一条要量的是「事件流本身丢没丢」。
type countHub struct {
	mu      sync.Mutex
	creates map[string]bool
	total   int
	lost    int
}

func (h *countHub) OnEvent(ev detect.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	if ev.Kind == detect.EvCreate {
		if h.creates == nil {
			h.creates = map[string]bool{}
		}
		h.creates[ev.Path] = true
	}
}
func (h *countHub) OnTick()              {}
func (h *countHub) OnLost(error)         { h.mu.Lock(); h.lost++; h.mu.Unlock() }
func (h *countHub) nCreates() int        { h.mu.Lock(); defer h.mu.Unlock(); return len(h.creates) }
func (h *countHub) snapshot() (int, int) { h.mu.Lock(); defer h.mu.Unlock(); return h.total, h.lost }

// TestRealBurstKeepsEveryCreate 是设计书里那条「灌 5000 个 Create」的量法：
// 用生产阶梯，一个都不许丢。
//
// 这条测试红了就等于「一次文件夹拷贝会被搬掉一部分」，所以它必须报出实际字节数：
// 要修的是阶梯，不是断言。
func TestRealBurstKeepsEveryCreate(t *testing.T) {
	const burst = 5000
	dir := t.TempDir()
	h := &countHub{}
	r := runReal(t, Config{Dir: dir, Tick: time.Hour, Sweep: -1}, h)

	makeFiles(t, dir, burst)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && h.nCreates() < burst {
		time.Sleep(20 * time.Millisecond)
	}
	total, lost := h.snapshot()
	t.Logf("收到事件共 %d 条, 其中 Create 覆盖 %d/%d 个名字, OnLost %d 次, 缓冲区 %d 字节",
		total, h.nCreates(), burst, lost, r.BufferSize())
	if h.nCreates() != burst {
		t.Errorf("只看见 %d 个 Create, 想要 %d 个：丢了 %d 个, 而 OnLost 只报了 %d 次"+
			"（丢了又不报 = 事件静默消失，重扫路径没被触发）",
			h.nCreates(), burst, burst-h.nCreates(), lost)
	}
}

// makeFiles 在 dir 里造 n 个小文件，名字形如 f0000.bin，内容 1 字节。
func makeFiles(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%04d.bin", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("造第 %d 个文件: %v", i, err)
		}
	}
}

// TestFilesThatMissedTheirEventAreRecoveredByTheSweep 演的是「事件来不了」这一路：
// 基线建好、400 个文件写进去、**然后**才开监听。
//
// 原本以为这样注定一条事件都收不到，实测并非如此：新武装的 RDCW 会补送一批
// 最近发生的改动（本机这次 400 个文件补送了 13 条事件），因为通知缓冲区挂在
// 目录对象上而不是挂在句柄上。这对产品是好事，但断言要改成说得清的形态：
// 事件数远远小于文件数，而全部文件最终都被判了 —— 于是「缺口由对账补齐」这件事
// 是被证明的，不是被假设的。
//
// 为什么值得单独钉一条：RDCW 的缓冲区溢出是**静默**的（fsnotify 在 ERROR_MORE_DATA 时
// 只是把 n 改成整个缓冲区长度接着解析，剩下的记录就地消失，连 ErrEventOverflow 都不报）。
// 漏掉一个 Create 的代价是「这个文件永远不被搬」，而这条测试保证那不可能发生。
func TestFilesThatMissedTheirEventAreRecoveredByTheSweep(t *testing.T) {
	const want = 400
	dir := t.TempDir()
	const quiet = 150 * time.Millisecond
	h := newRealHub(t, dir, quiet) // 此刻目录是空的，基线里什么都没有

	makeFiles(t, dir, want) // 监听还没开，绝大部分改动不可能再被通知到

	runReal(t, Config{Dir: dir, Tick: 100 * time.Millisecond, Sweep: 300 * time.Millisecond}, h)

	waitReal(t, 60*time.Second, fmt.Sprintf("%d 个文件全部被判成可搬", want), func() bool {
		return h.claimsTotal() >= want
	})
	t.Logf("%s", h.diag("对账恢复"))
	if n := h.eventCount(); n >= want {
		t.Errorf("收到 %d 条事件, 想要远少于 %d 条：事件几乎齐了，这一条就没测到对账那条路", n, want)
	}
	if h.scans() == 0 {
		t.Errorf("一次重扫都没跑，但 %d 个文件却被判了 —— 判定是从哪儿来的？", want)
	}
	if got := h.claimsTotal(); got != want {
		t.Errorf("判了 %d 个路径, 想要恰好 %d 个（重复判定也会被这里抓到）", got, want)
	}
	if n := h.aborts(); n != 0 {
		t.Errorf("记了 %d 次下载取消：这批文件根本没有临时名，一次取消都不该有", n)
	}
	if n := h.stuck(); n != 0 {
		t.Errorf("有 %d 个被判成卡住：预算 2 分钟而期限 60 秒，这条不该成立", n)
	}
	h.checkQuiet(t)
}

// TestTinyBufferStalledLoopStillClaimsEveryFile 真去撞一次溢出：
// 4096 字节的内核缓冲区 + 循环线程停 2 秒（Post 的回调跑在循环线程上，
// 这一觉期间没人读事件、也没人重新武装读取）+ 期间灌进 1500 个文件。
//
// 断言的只有那条要紧的不变式（最终一个都不漏）；到底丢没丢只报数。
// 本机第一次跑（400 个文件、同样的停摆）是**一条没丢**的：821 条事件全数到达，
// 因为 fsnotify 的 Go 通道有 1024 格，后端宁可阻塞在通道上也不放回缓冲区。
// 所以这条要 1500 个才够把通道压穿 —— 它测的是「真丢了也能收敛」，
// 而上一条测的是「一条都没来过也能收敛」。
func TestTinyBufferStalledLoopStillClaimsEveryFile(t *testing.T) {
	const want = 1500
	dir := t.TempDir()
	const quiet = 150 * time.Millisecond
	h := newRealHub(t, dir, quiet)
	r := runReal(t, Config{
		Dir:   dir,
		Tick:  100 * time.Millisecond,
		Sweep: 300 * time.Millisecond,
		sizes: []int{4096},
	}, h)

	stalled := make(chan struct{})
	r.Post(func() {
		time.Sleep(2 * time.Second)
		close(stalled)
	})
	makeFiles(t, dir, want)

	select {
	case <-stalled:
	case <-time.After(20 * time.Second):
		t.Fatal("循环线程没跑那个停摆回调：Post 的排队契约垮了")
	}

	waitReal(t, 120*time.Second, fmt.Sprintf("%d 个文件全部被判成可搬", want), func() bool {
		return h.claimsTotal() >= want
	})
	t.Logf("%s", h.diag("小缓冲区停摆"))
	// 丢没丢只报数，不作为断言：这一条要保的是「最终一个都不漏」，
	// 而不是「内核真的丢了」。真一条没丢的时候它照样该绿。
	//
	// 本机实测（-race 这一跑）：1500 个文件只收到 1025 条事件 —— 少的就是静默溢出的那批，
	// 而 OnLost 只在 DefaultSweep 时报过，ErrEventOverflow 一次都没有。
	// 每个文件至少该有一条 Create，所以 want-事件数 是「丢了几个文件的通知」的下限。
	if n := h.eventCount(); n < want {
		t.Logf("小缓冲区 + 停摆：事件 %d 条，至少有 %d 个文件的通知静默丢失（OnLost 只报过 %d 次，全是定期对账），最后仍全部判定",
			n, want-n, h.lostCount())
	} else {
		t.Logf("小缓冲区 + 停摆：事件 %d 条，这次没溢出（判定多半走事件那条路），最后仍全部判定", n)
	}
	if got := h.claimsTotal(); got != want {
		t.Errorf("判了 %d 个路径, 想要恰好 %d 个（重复判定也会被这里抓到）", got, want)
	}
	if n := h.aborts(); n != 0 {
		t.Errorf("记了 %d 次下载取消：这批文件根本没有临时名，一次取消都不该有", n)
	}
	if n := h.stuck(); n != 0 {
		t.Errorf("有 %d 个被判成卡住：预算 2 分钟而期限 120 秒，这条不该成立", n)
	}
	h.checkQuiet(t)
}
