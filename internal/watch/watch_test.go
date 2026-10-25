package watch

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"download2/internal/detect"
)

// fakeSource 是 source 的替身：三个通道全部由测试自己攥着，
// 于是「溢出」「通道关闭」「事件排在节拍前面」这些形状都能按我要的顺序演出来。
type fakeSource struct {
	evs  chan fsnotify.Event
	errs chan error
	post chan func()

	closes atomic.Int32
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		evs:  make(chan fsnotify.Event, eventChanDepth),
		errs: make(chan error, 8),
		post: make(chan func(), postChanDepth),
	}
}

func (s *fakeSource) Events() <-chan fsnotify.Event { return s.evs }
func (s *fakeSource) Errors() <-chan error          { return s.errs }
func (s *fakeSource) Post() chan func()             { return s.post }
func (s *fakeSource) Close() error {
	s.closes.Add(1)
	close(s.evs)
	close(s.errs)
	return nil
}

// recorder 既是断言用的收集器，也是「不该发生并发」的探针：
// 循环只有一条 goroutine，所以任何一次回调都不可能在另一次还没返回时被调进来。
// 一旦 busy 的重入被记下来，就说明 Post 与事件其实跑在不同的线程上。
type recorder struct {
	mu         sync.Mutex
	busy       bool
	violations int
	events     []detect.Event
	ticks      int
	lost       []error
	// firstTickLen 是第一次节拍时的 len(events)：pump 收干了就该等于全部 burst。
	firstTickLen int
}

// recState 是一份加锁读出来的快照，测试线程只看它，绝不直接碰循环线程写的字段。
type recState struct {
	events, ticks, lost, violations, firstTickLen int
}

func (r *recorder) begin() func() {
	r.mu.Lock()
	if r.busy {
		r.violations++
	}
	r.busy = true
	return func() { r.busy = false; r.mu.Unlock() }
}

func (r *recorder) OnEvent(ev detect.Event) {
	defer r.begin()()
	r.events = append(r.events, ev)
}

func (r *recorder) OnTick() {
	defer r.begin()()
	r.ticks++
	if r.ticks == 1 {
		r.firstTickLen = len(r.events)
	}
}

func (r *recorder) OnLost(err error) {
	defer r.begin()()
	r.lost = append(r.lost, err)
}

func (r *recorder) state() recState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recState{
		events:       len(r.events),
		ticks:        r.ticks,
		lost:         len(r.lost),
		violations:   r.violations,
		firstTickLen: r.firstTickLen,
	}
}

// got 返回收到的事件种类，顺序就是投递顺序。
func (r *recorder) got() []detect.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]detect.EventKind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *recorder) counts() (events, ticks, lost, violations int) {
	st := r.state()
	return st.events, st.ticks, st.lost, st.violations
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

func TestClassifiedEventsReachTheHandler(t *testing.T) {
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: time.Hour}) // 节拍压到不会响，只看事件

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	src.evs <- fsnotify.Event{Name: "/D/Downloads/a.zip.crdownload", Op: fsnotify.Create}
	src.evs <- fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Rename}
	src.evs <- fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Chmod}

	waitUntil(t, "三条事件里的两条被投递", func() bool {
		n, _, _, _ := rec.counts()
		return n == 2
	})
	_, _, _, v := rec.counts()
	if v != 0 {
		t.Errorf("回调重入 %d 次：循环不是单 goroutine 的", v)
	}
	want := []detect.EventKind{detect.EvCreate, detect.EvRenameFrom}
	if kinds := rec.got(); len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Errorf("收到 %+v, 想要 %+v（Chmod 不该占一条）", kinds, want)
	}

	if err := r.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrStopped) {
		t.Errorf("Run 返回 %v, 想要 ErrStopped", err)
	}
}

// pump 存在的意义：一整个文件夹拷进来时，如果判定夹在一半事件中间跑，
// 那些「临时名已经消失、最终名还没到」的文件会被说成下载被取消。
func TestEventsAlreadyQueuedAreDrainedBeforeTheFirstTick(t *testing.T) {
	src := newFakeSource()
	const burst = 200
	for i := 0; i < burst; i++ {
		src.evs <- fsnotify.Event{Name: "/D/Downloads/f" + string(rune('A'+i%26)) + ".zip", Op: fsnotify.Create}
	}

	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: 5 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	waitUntil(t, "走到第一次节拍", func() bool {
		return rec.state().ticks >= 1
	})
	st := rec.state()
	if st.firstTickLen != burst {
		t.Errorf("第一次节拍时只收到 %d 条, 想要 %d 条全部先收干", st.firstTickLen, burst)
	}
	if st.violations != 0 {
		t.Errorf("回调重入 %d 次", st.violations)
	}
	_ = r.Stop()
	<-done
}

// fsnotify 的错误通道 = 「这段事件不可信」，调用方唯一的正确处置是重扫。
func TestBackendErrorsBecomeOnLost(t *testing.T) {
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: time.Hour})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	src.errs <- fsnotify.ErrEventOverflow

	waitUntil(t, "溢出被上报", func() bool {
		_, _, n, _ := rec.counts()
		return n == 1
	})
	_ = r.Stop()
	<-done
}

// 对账间隔就是「事件流静默丢了一批」的兜底，见 DefaultSweep 的注释。
func TestSweepAsksForReconcile(t *testing.T) {
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: time.Hour, Sweep: 5 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitUntil(t, "对账触发", func() bool {
		_, _, n, _ := rec.counts()
		return n > 0
	})
	_ = r.Stop()
	<-done
}

func TestSweepStaysOffUnlessAsked(t *testing.T) {
	// newRunner 不擅自打开对账：只有 New（真目录）才补默认值，
	// 否则每个假流测试都会多出一串 OnLost。
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: 5 * time.Millisecond})
	if r.cfg.Sweep != 0 {
		t.Fatalf("假流测试里 Sweep = %v, 想要 0", r.cfg.Sweep)
	}

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitUntil(t, "走过几轮节拍", func() bool {
		_, ticks, _, _ := rec.counts()
		return ticks >= 3
	})
	_ = r.Stop()
	<-done
	if _, _, lost, _ := rec.counts(); lost != 0 {
		t.Errorf("没开对账却上报了 %d 次", lost)
	}
}

func TestTickFallsBackToDefault(t *testing.T) {
	r := newRunner(newFakeSource(), &recorder{}, Config{})
	if r.cfg.Tick != DefaultTick {
		t.Errorf("Tick = %v, 想要 %v", r.cfg.Tick, DefaultTick)
	}
}

// 事件流被后端关掉（目录没了、句柄失效）时 Run 必须带着错误退出，
// 而不是留一个「进程活着但一个文件都不搬」的空壳。
func TestClosedSourceEndsRun(t *testing.T) {
	src := newFakeSource()
	r := newRunner(src, &recorder{}, Config{Tick: 5 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	close(src.evs)

	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Errorf("Run 返回 %v, 想要 ErrStopped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("事件通道关了，Run 还在等")
	}
}

// Post 是本包对外的唯一入口：搬运器在自己的 goroutine 上完事之后，
// 必须把「搬完了」交回循环线程，才能碰那台不是并发安全的机器。
func TestPostedCallbacksRunOnTheLoopGoroutine(t *testing.T) {
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: time.Hour})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	var ran atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Post(func() { ran.Add(1) })
		}()
		// 事件同时也在到，验的是两条来源不会交织
		src.evs <- fsnotify.Event{Name: "/D/Downloads/x.zip", Op: fsnotify.Write}
	}
	wg.Wait()

	waitUntil(t, "16 个回调都跑完", func() bool { return ran.Load() == 16 })
	if _, _, _, v := rec.counts(); v != 0 {
		t.Errorf("回调重入 %d 次：Post 和事件不在同一条 goroutine 上", v)
	}
	_ = r.Stop()
	<-done
}

// Run 已经退出之后 Post 既不能把人挂住，也不能再把回调塞给一个没有读者的循环。
func TestPostAfterExitDropsInsteadOfHanging(t *testing.T) {
	src := newFakeSource()
	rec := &recorder{}
	r := newRunner(src, rec, Config{Tick: 5 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitUntil(t, "走过一轮节拍", func() bool {
		_, ticks, _, _ := rec.counts()
		return ticks >= 1
	})
	_ = r.Stop()
	<-done

	var ran atomic.Int32
	// 比队列额度还多，确保至少有一批撞上「队列已满」的那一支。
	for i := 0; i < postChanDepth+8; i++ {
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			r.Post(func() { ran.Add(1) })
		}()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("Run 退出之后 Post 卡住了")
		}
	}
	if n := ran.Load(); n != 0 {
		t.Errorf("循环退了还跑掉 %d 个回调", n)
	}
}

// Stop 重复调用只关一次底层监听：界面退出和 ctx 取消两条路都可能各调一次，
// 而 fsnotify 的 Close 关第二遍会 panic。
func TestStopIsIdempotent(t *testing.T) {
	src := newFakeSource()
	r := newRunner(src, &recorder{}, Config{Tick: 5 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()

	if err := r.Stop(); err != nil {
		t.Errorf("第一次 Stop: %v", err)
	}
	if err := r.Stop(); err != nil {
		t.Errorf("第二次 Stop: %v", err)
	}
	if n := src.closes.Load(); n != 1 {
		t.Errorf("底层监听从了 %d 次话, 想要 1", n)
	}
}

// stuckSource 只把 Close 换成「永远不回来」，演的是真 fsnotify 在 Windows 上收不到
// 回执的那条死路（见 Stop 的注释）。测试收尾时放行，不留悬着的 goroutine。
type stuckSource struct {
	*fakeSource
	release chan struct{}
}

func (s *stuckSource) Close() error { <-s.release; return nil }

// Close 不回来不能把 Stop 一起带走：Stop 是界面线程调的，卡住就是整个药丸冻住。
// 这里断言三件事 —— 在预算内回来、报 ErrCloseStuck、循环确实退了。
func TestStopSurvivesACloseThatNeverReturns(t *testing.T) {
	src := &stuckSource{fakeSource: newFakeSource(), release: make(chan struct{})}
	t.Cleanup(func() { close(src.release) })

	r := newRunner(src, &recorder{}, Config{Tick: 5 * time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	start := time.Now()
	err := r.Stop()
	spent := time.Since(start)

	if !errors.Is(err, ErrCloseStuck) {
		t.Errorf("Stop 返回 %v, 想要 ErrCloseStuck", err)
	}
	if spent > closeWait+time.Second {
		t.Errorf("Stop 花了 %s, 想要不超过 closeWait(%s) 加一点调度余量", spent, closeWait)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Errorf("Run 返回 %v, 想要 ErrStopped", err)
		}
	case <-time.After(time.Second):
		t.Error("Close 卡住之后循环没退：Stop 叫不动它，调用方还得替它等")
	}
	if err := r.Stop(); !errors.Is(err, ErrCloseStuck) {
		t.Errorf("第二次 Stop 返回 %v, 想要还是那一句 ErrCloseStuck", err)
	}
}

// —— 与真状态机的接缝：假事件流 + 真 Machine，看改名对能不能按设计书完成判定 ——

// 这个测试里测试线程一次都不碰机器：detect.Machine 不是并发安全的，
// 而循环 goroutine 一直在 Tick 它。所以所有观察都经 handler 记一笔再读，
// 假文件系统也在开跑之前就填好，跑起来之后只读不写。
func TestMachineClaimsAChromeRenameThroughTheRunner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Downloads")
	temp, final := dir+string(filepath.Separator)+"movie.mp4.crdownload",
		filepath.Join(dir, "movie.mp4")

	fs := map[string]detect.Sample{
		temp:  {Size: 1 << 20, Mtime: time.Now(), Index: 7},
		final: {Size: 1 << 20, Mtime: time.Now(), Index: 7},
	}
	// 时钟传 nil：这一路要的就是真时钟加真 ticker，节拍与静置窗口怎么配合只能这样验。
	m := detect.New(detect.Config{Quiet: 30 * time.Millisecond, MaxSettle: time.Minute},
		detect.SamplerFunc(func(p string) (detect.Sample, error) {
			s, ok := fs[p]
			if !ok {
				return detect.Sample{}, errors.New("没了")
			}
			return s, nil
		}),
		detect.LockerFunc(func(string) bool { return false }), nil)

	h := &machineHandler{m: m, claims: map[string]int{}}
	src := newFakeSource()
	r := newRunner(src, h, Config{Tick: 10 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	src.evs <- fsnotify.Event{Name: temp, Op: fsnotify.Create}
	waitUntil(t, "临时名被追踪", func() bool { return h.liveOf() == 1 })

	// 改名对：两个 action 挨着到达，中间不许插一轮判定。
	src.evs <- fsnotify.Event{Name: temp, Op: fsnotify.Rename}
	src.evs <- fsnotify.Event{Name: final, Op: fsnotify.Create}

	waitUntil(t, "最终名被判成可搬", func() bool { return h.claimsOf(final) > 0 })
	if n := h.claimsOf(final); n != 1 {
		t.Errorf("%s 被判了 %d 次搬运请求, 想要恰好一次", final, n)
	}
	if n := h.aborts(); n != 0 {
		t.Errorf("改名成功却记了 %d 条取消", n)
	}
	if n := h.liveOf(); n != 0 {
		t.Errorf("改名之后还挂着 %d 个存活临时名", n)
	}
	_ = r.Stop()
	<-done
}

// machineHandler 是最小的一层胶水：三个回调都只做「转交给机器」。
// T7 的正式实现会在此之上加账本与搬运器，但 Handler 这份契约就是现在这个样子。
type machineHandler struct {
	mu      sync.Mutex
	m       *detect.Machine
	claims  map[string]int
	aborted int
	live    int
}

func (h *machineHandler) OnEvent(ev detect.Event) { h.m.OnEvent(ev) }

func (h *machineHandler) OnTick() {
	for _, o := range h.m.Tick() {
		switch o.Kind {
		case detect.OutClaim:
			h.mu.Lock()
			h.claims[o.Path]++
			h.mu.Unlock()
			h.m.MarkDone(o.Path) // 假搬运：当场了结，免得它一直挂在途
		case detect.OutAborted:
			h.mu.Lock()
			h.aborted++
			h.mu.Unlock()
		case detect.OutStuck:
		}
	}
	h.mu.Lock()
	h.live = h.m.LiveDownloads() // 在循环 goroutine 上读，测试线程只读这份副本
	h.mu.Unlock()
}

func (h *machineHandler) OnLost(error) {}

func (h *machineHandler) claimsOf(p string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.claims[p]
}

func (h *machineHandler) aborts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aborted
}

func (h *machineHandler) liveOf() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.live
}
