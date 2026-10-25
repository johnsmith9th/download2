// Package watch 是唯一把 OS 文件事件翻译成判定输入的地方。
//
// 它存在的理由只有一句话：detect.Machine 不是并发安全的，而这里保证事件、
// 判定节拍、定期对账、以及调用方从别的 goroutine 投进来的回调，
// 全部在同一个 goroutine 上跑。搬运器的完成回调、设置变更之类都必须走 Post，
// 绝不在自己的 goroutine 上直接摸机器。
package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"download2/internal/apperr"
	"download2/internal/detect"
)

const (
	// DefaultTick 是判定节拍，和 detect 的「连续两次采样一致」是配套的一对数：
	// 间隔再短也只是多采几次同样的读数，再长就把静置窗口拉长了。
	DefaultTick = 500 * time.Millisecond

	// DefaultSweep 是「不管事件流有没有动静都对一次账」的间隔，见 OnLost。
	//
	// 它不是多余的保险：Windows 上 ReadDirectoryChangesW 的缓冲区被写满时，
	// fsnotify 会拿着截断的缓冲区继续解析，剩下那批记录是**静默消失**的
	// （backend_windows.go 只在 ERROR_MORE_DATA 时把 n 改成整个缓冲区长度，
	// 并不会报 ErrEventOverflow）。漏掉一个 Create 的代价是「这个文件永远不被搬」，
	// 而一次顶层 ReadDir + 每项一次 stat 的价钱，在一个目录里几乎可以忽略。
	// 一分钟一次，最坏情况就是搬得晚一点，而不是不搬。
	DefaultSweep = time.Minute

	// eventChanDepth 是事件通道的缓冲：一次系统调用能带回上百条事件，
	// 而判定每 500ms 才走一轮，中途到达的事件得先有地方放。
	eventChanDepth = 1024

	// postChanDepth 是排队回调的额度。搬运器一轮最多提交一个结果，64 个位置早够了；
	// 真排满了说明循环线程被某个回调卡住 —— 那种时候 Post 阻塞住，
	// 比把回调堆到一个永远处理不完的列表里更容易看见问题。
	postChanDepth = 64

	// closeWait 是 Stop 的全部预算：等底层监听关掉、等循环收摊，两件事共用这一份。
	// Stop 由界面线程调用，不能因为监听线程正好卡在一个慢回调里就把界面一起挂住。
	closeWait = 2 * time.Second
)

// bufferSizes 是 ReadDirectoryChangesW 的缓冲区阶梯，从大到小试第一个能开的。
//
// fsnotify 默认 64K，它自己的文档写着「64K 是所有文件系统都能接受的最大值」，
// 也就是更大的数在某个重定向器上是会失败的，所以这里退着试而不是硬给一个数。
// 64K 在「往 Downloads 拷一整个文件夹」这种 burst 下会溢出，而溢出是静默的
// （见 DefaultSweep），所以先把缓冲区开大。
//
// 本机实测（2026-10-07）：8MB 那档在本地 NTFS 上开起来了；WithBufferSize 只拦
// 4096 以下的下限，上限一概不校验，所以这个阶梯是我们自己的政策而不是探测。
var bufferSizes = []int{8 << 20, 2 << 20, 64 << 10}

// ErrStopped 是监听正常停止时 Run 的返回值，不是故障。
var ErrStopped = errors.New("watch: 监听已停止")

// ErrReconcile 是通过对 OnLost 说的「没有别的理由，该对一次账了」。
// 调用方对它和对真溢出的处置完全一样：重扫目录、交给 Machine.Scan。
var ErrReconcile = errors.New("watch: 定期对账")

// ErrCloseStuck 说「底层监听在 closeWait 之内没关掉」。
//
// 见 Stop：这不是理论风险，Windows 上 fsnotify 的 Close 有这一条死路。
var ErrCloseStuck = errors.New("watch: 监听句柄没在期限内关掉")

// Config 是监听一个目录需要定的三个数。零值都用默认。
type Config struct {
	Dir   string
	Tick  time.Duration
	Sweep time.Duration // 0 = DefaultSweep；负数 = 关闭对账（测试里为了跑得快会关掉）

	// sizes 是缓冲区阶梯的覆盖入口，只有本包的测试用得到：
	// 想逼出「事件静默消失」就得把 ReadDirectoryChangesW 的缓冲区开小，
	// 默认的 8MB 在一台本机上是灌多少都不容易溢出的。生产留空 = 用 bufferSizes。
	sizes []int
}

// Handler 是循环 goroutine 回调给调用方的三件事。
//
// 三个方法都只在 Run 的那个 goroutine 上被调用，所以实现里可以直接摸
// detect.Machine 而不加锁 —— 这条约束就是本包存在的原因。
type Handler interface {
	// OnEvent 收一条归一化后的事件。
	OnEvent(ev detect.Event)
	// OnTick 到判定的节拍了：实现里调 Machine.Tick() 并处理产出的动作。
	OnTick()
	// OnLost 说「这一段事件不可信，去重扫目录」：缓冲区溢出、目录被删、
	// 或者只是到了 DefaultSweep。实现里调 Snapshot + Machine.Scan。
	// 这里只报告不暂停：之后监听仍然继续。
	OnLost(err error)
}

// source 是被监听目录底下那层事件流。
//
// 抽成接口是为了 Runner 能在假流上测：溢出、通道关闭、事件与回调的先后这些形状，
// 用真文件系统跑出来的那一条路，都不如直接喂进去的一条路说得清。
type source interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Post() chan func()
	Close() error
}

// Runner 把一个目录的事件流、判定节拍和调用方回调汇进同一个 goroutine。
type Runner struct {
	h   Handler
	src source
	cfg Config
	dir string

	// size 是实际用上的缓冲区字节数，日志与 dlprobe 要看。
	size int

	closeOnce sync.Once
	stopping  chan struct{} // Stop 一进门就关它：循环看到就走，不等 Close 回来
	closed    chan struct{} // 循环真的收摊了

	// closeErr 是那一次 Close 的结果，之后每一次 Stop 都说同一个数。
	// 只由 closeOnce 里那次写，Once 保证别的 goroutine 读到它之前写已经发生。
	closeErr error
}

// New 开始监听 cfg.Dir。返回时监听句柄已开好，但还没有开始投递 ——
// Run 才是那条 goroutine。
func New(cfg Config, h Handler) (*Runner, error) {
	if cfg.Tick == 0 {
		cfg.Tick = DefaultTick
	}
	if cfg.Sweep == 0 {
		cfg.Sweep = DefaultSweep
	}
	sizes := cfg.sizes
	if len(sizes) == 0 {
		sizes = bufferSizes
	}
	src, buf, err := openDir(cfg.Dir, sizes)
	if err != nil {
		return nil, err
	}
	r := newRunner(src, h, cfg)
	r.dir, r.size = absOf(cfg.Dir), buf
	return r, nil
}

func newRunner(src source, h Handler, cfg Config) *Runner {
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultTick
	}
	return &Runner{h: h, src: src, cfg: cfg,
		stopping: make(chan struct{}), closed: make(chan struct{})}
}

// Post 把 fn 排到循环 goroutine 上执行，可以从任意 goroutine 调用。
//
// 队列满时会等。Run 已经退出时 fn 被丢弃：那时机器已经没有读者了。
func (r *Runner) Post(fn func()) {
	select {
	case <-r.closed:
	case r.src.Post() <- fn:
	}
}

// Run 在调用它的 goroutine 上跑事件循环，直到 ctx 被取消、Stop 被调、
// 或底层事件流关闭。返回 ErrStopped 之外的错误时，调用方要重扫目录再重开监听。
func (r *Runner) Run(ctx context.Context) error {
	defer close(r.closed)

	tick := time.NewTicker(r.cfg.Tick)
	defer tick.Stop()

	// 对账间隔关掉时就用一个永远不响的通道，省掉到处写 if。
	var sweepC <-chan time.Time
	if r.cfg.Sweep > 0 {
		s := time.NewTicker(r.cfg.Sweep)
		defer s.Stop()
		sweepC = s.C
	}

	for {
		// 先把已经到达的东西收干再判：一整个文件夹拷进来时，中途夹一轮 Tick
		// 会把还没到齐的改名对说成「下载被取消」。
		if r.pump() {
			return ErrStopped
		}

		select {
		case <-ctx.Done():
			return ErrStopped
		case <-r.stopping:
			// Stop 已经叫过了。这里不等 src.Close 把事件通道关掉：
			// 那一次 Close 可能永远回不来，见 Stop。
			return ErrStopped
		case fn := <-r.src.Post():
			fn()
		case ev, ok := <-r.src.Events():
			if !ok {
				return ErrStopped // 通道关了 = 后端已经没了，等也不会有事件进来
			}
			r.deliver(ev)
		case err, ok := <-r.src.Errors():
			if ok && err != nil {
				r.h.OnLost(err)
			}
		case <-sweepC:
			r.h.OnLost(ErrReconcile)
		case <-tick.C:
			r.h.OnTick()
		}
	}
}

// pump 处理掉此刻已经排在路上的回调与事件；返回 true 表示事件流已经关闭。
func (r *Runner) pump() bool {
	for {
		select {
		case fn := <-r.src.Post():
			fn()
			continue
		default:
		}
		select {
		case ev, ok := <-r.src.Events():
			if !ok {
				return true
			}
			r.deliver(ev)
			continue
		case err, ok := <-r.src.Errors():
			if ok && err != nil {
				r.h.OnLost(err)
			}
			continue
		default:
			return false
		}
	}
}

func (r *Runner) deliver(ev fsnotify.Event) {
	for _, e := range Classify(ev) {
		r.h.OnEvent(e)
	}
}

// Stop 关掉监听并等循环收摊，两件事共用 closeWait 这一份预算。重复调用只关一次。
//
// 给 Close 也上界不是防御性的保守，是它真会不回来。Windows 上 fsnotify 的 Close
// 先把一个令牌递给它的读线程、再等回执（backend_windows.go:108 送、:112 等），而
// 收这个令牌的不止循环顶部那一处：sendError（:90）把它收走就直接 return，回执从此
// 没人发。CI 上撞到的现场正是这两帧 —— Close 卡在 :112 十九分钟，读线程卡在 :514
// 的 GetQueuedCompletionStatus 一动不动。触发它的是那条 4096 字节缓冲区 + 1500 个
// 文件的用例，本机连跑三次没复现，是一条要抢 CPU 才踩得上的时序。
//
// 等不到就带着 ErrCloseStuck 回来：那个读 goroutine 和它的句柄就此悬着（它已经不
// 理会任何人，没有别的办法），但界面线程、暂停、重建监听不陪着它一起卡死。
func (r *Runner) Stop() error {
	deadline := time.Now().Add(closeWait)
	r.closeOnce.Do(func() {
		close(r.stopping)
		r.closeErr = r.closeSrc(time.Until(deadline))
	})
	waitClosed(r.closed, time.Until(deadline))
	return r.closeErr
}

// closeSrc 在另一条 goroutine 上关事件流，最多等 limit；limit 用光就是 ErrCloseStuck。
func (r *Runner) closeSrc(limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- r.src.Close() }()

	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return ErrCloseStuck
	}
}

// Dir 是监听用的绝对路径。
func (r *Runner) Dir() string { return r.dir }

// BufferSize 是实际开出来的 ReadDirectoryChangesW 缓冲区字节数。
// 「为什么一直在重扫」第一条要看的就是这个数落在阶梯的哪一级。
func (r *Runner) BufferSize() int { return r.size }

func waitClosed(done <-chan struct{}, limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func absOf(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// fsSource 是 fsnotify.Watcher 的 source 适配：那个类型把通道做成字段而不是方法，
// 而排队回调的通道由适配层持有，好让 source 把「队列也归循环线程管」说清楚。
type fsSource struct {
	w     *fsnotify.Watcher
	queue chan func()
}

func (s *fsSource) Events() <-chan fsnotify.Event { return s.w.Events }
func (s *fsSource) Errors() <-chan error          { return s.w.Errors }
func (s *fsSource) Post() chan func()             { return s.queue }
func (s *fsSource) Close() error                  { return s.w.Close() }

// openDir 监听 dir 顶层，返回事件流与实际用上的缓冲区大小。
// sizes 是候选阶梯；测试里传一个小的来逼出溢出。
func openDir(dir string, sizes []int) (*fsSource, int, error) {
	abs := absOf(dir)
	// fsnotify 不会等一个还不存在的目录，而这里也分不清「还没建」和「打不开」，
	// 所以目录必须由调用方保证存在（config 校验在保存时就 MkdirAll）。
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, 0, apperr.New(apperr.EWatchUnavailable, "监控目录不存在或不是一个文件夹").
			With("dir", abs)
	}

	w, err := fsnotify.NewBufferedWatcher(eventChanDepth)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.EWatchUnavailable, "无法创建目录监听", err)
	}
	var lastErr error
	for _, size := range sizes {
		// fsnotify 没有公开的按 Op 过滤入口（withOps 是包私有的），
		// 所以用不上的操作只能在 Classify 那一侧丢，这里能控的只有缓冲区。
		if lastErr = w.AddWith(abs, fsnotify.WithBufferSize(size)); lastErr == nil {
			return &fsSource{w: w, queue: make(chan func(), postChanDepth)}, size, nil
		}
	}
	_ = w.Close()
	return nil, 0, apperr.Wrap(apperr.EWatchUnavailable, "无法监听监控目录", lastErr).With("dir", abs)
}
