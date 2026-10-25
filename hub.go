package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"download2/internal/apperr"
	"download2/internal/config"
	"download2/internal/detect"
	"download2/internal/journal"
	"download2/internal/mover"
	"download2/internal/watch"
)

// Hub 是把四个包接起来的那根线：watch 供事件、detect 判完成、mover 动字节、journal 记每一步。
//
// goroutine 口径只有一条，是从 watch 那边继承的：
//
//   - detect.Machine 只在 watch.Runner 的那一条循环 goroutine 上被碰。
//   - 搬运的 IO 在另一条线程上跑（它可能要复制几 GB），跑完用 Runner.Post 回到循环上改机器状态。
//   - Runner 没在跑的时候（启动前、暂停中、收摊后）那条循环 goroutine 不存在，
//     这时直接改机器状态是安全的 —— settle 分两支就是为了这件事。
//
// machine 用 atomic.Pointer 换：改静置阈值要重建它，而重建只发生在监听已停下的时候。
// 用原子而不是「再加一把锁」，是为了让「循环 goroutine 读、UI 线程换」这件事有一个说得清的先后关系。
type Hub struct {
	log   *slog.Logger
	dir   string
	store config.Store
	jr    *journal.Journal
	mv    *mover.Mover

	mu       sync.Mutex
	settings config.Settings
	// disk 是 settings.json 里那一份设置。它和 settings 之间的差，就是启动参数带来的
	// 那几项「只对本次生效」的覆盖（转移目录、语言、配色）—— 一次性的参数要是跟着锚点
	// 一起进设置文件，用户下一次双击就会把下载搬进那个临时目录，而他做的只是拖了一下药丸。
	// 没带参数时两者恒等，这条区分对日常是透明的。
	disk    config.Settings
	runner  *watch.Runner
	running bool
	started bool // 搬运线程起来过没有：Shutdown 靠它决定要不要等 h.closed
	quit    bool // Shutdown 之后不再开新监听

	// reason 是「为什么现在没在监控」的那句话，由 h.mu 护着。
	// 它必须存在 hub 里而不是只塞进一次 status：药丸每 500ms 被刷一次，
	// 一次监听死亡的原文要是只活在那一条 status 里，下一拍就被刷成「监控中」了。
	reason string

	// watchSince 是最近一次「监听起来」的那一刻：启动、从暂停恢复、改设置重建都算。
	// 对账往回追候选时拿它当上限（见 reconcileCutoff），由 h.mu 护着。
	watchSince time.Time

	machine atomic.Pointer[detect.Machine]

	queue  chan string
	closed chan struct{}
	wg     sync.WaitGroup
	shut   sync.Once

	// smu 只护 status 这一份小快照：循环 goroutine 写、UI 线程读。
	smu        sync.Mutex
	lastStatus status

	// emit 由 main.go 在建窗之后缝一次，之后只读：hub 到这儿就不认识窗口了。
	emit func(name string, payload any)
}

// notConfigured 是「还没配转移目录」时药丸要说的那一句。它走 status.reason 这条唯一的
// 通道，界面不再自己编第二套说法。
const notConfigured = "还没设转移目录，药丸先不动任何文件。"

// status 是药丸上那行字的唯一数据源。
type status struct {
	Watching  bool   `json:"watching"`
	Paused    bool   `json:"paused"`
	Pending   int    `json:"pending"`
	Live      int    `json:"live"`
	TargetDir string `json:"targetDir"`
	SourceDir string `json:"sourceDir"`
	Reason    string `json:"reason,omitempty"`
}

// newHub 的 set 必须是盘上那一份设置：disk 就从它抄下来。启动参数带来的覆盖由 Use* 那几条
// 在建好之后单独打上去，「盘上该写什么」和「本轮用什么」才各有各的出处。
func newHub(dir string, log *slog.Logger, set config.Settings) (*Hub, error) {
	store, err := config.NewStore(dir)
	if err != nil {
		return nil, err
	}
	jr, err := journal.New(dir)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		log:      log,
		dir:      dir,
		store:    store,
		jr:       jr,
		settings: set,
		disk:     set,
		queue:    make(chan string, 64),
		closed:   make(chan struct{}),
	}
	h.mv, err = mover.New(mover.Options{
		Settings: func() mover.Config {
			s := h.cur()
			return mover.Config{SourceDir: s.SourceDir, TargetDir: s.TargetDir, DateSubdirs: s.DateSubdirs}
		},
		Book:   jr,
		Logger: log,
	})
	if err != nil {
		return nil, err
	}
	h.machine.Store(newMachine(set))
	return h, nil
}

func newMachine(s config.Settings) *detect.Machine {
	return detect.New(
		detect.Config{
			Quiet:     time.Duration(s.QuietMs) * time.Millisecond,
			MaxSettle: time.Duration(s.MaxSettleMs) * time.Millisecond,
		},
		detect.SamplerFunc(detect.StatSample),
		detect.LockerFunc(detect.Locked),
		detect.RealClock{},
	)
}

func (h *Hub) cur() config.Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

// forDisk 是那几条「跟目录/语言/配色无关却顺手写一份设置」的路（记锚点、暂停、重新定位监控
// 目录）写盘前的那一步：那三项换回盘上那一份。本轮的覆盖要是跟着这些写入一起进设置文件，
// 表现就是「拖了一下药丸，下载从此搬进那个临时目录、界面从此是英文」——
// 而那一下拖拽里没有任何一句关于转移目录或语言的话。
func (h *Hub) forDisk(s config.Settings) config.Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.TargetDir, s.Theme, s.Lang = h.disk.TargetDir, h.disk.Theme, h.disk.Lang
	return s
}

// commitDisk 记下「盘上现在就是这一份」。只在 store.Save 真的成功之后调。
func (h *Hub) commitDisk(d config.Settings) {
	h.mu.Lock()
	h.disk = d
	h.mu.Unlock()
}

// setRun 改的是「本轮那一份」内存设置，盘上的差由 forDisk 认。
// 语言与配色不进 affectsRuntime 那张表，所以这里也不碰监听：换的是文案，不是判定。
func (h *Hub) setRun(fn func(*config.Settings)) {
	h.mu.Lock()
	fn(&h.settings)
	h.mu.Unlock()
}

// diskTargetFor 决定面板那一次保存该往盘上写哪个转移目录。
//
// 面板没有保存按钮，任何一个控件失焦都把「当前显示的落点」原样送回来。本轮的落点来自启动
// 参数、送回来的还是它 → 用户压根没碰转移目录那一行，盘上继续存它原本那一份；改个提示秒数
// 就把一次性参数写成永久设置，是这套用法最不像话的失败方式。
// 送回来的是别的目录 → 那是用户当场重选的永久落点，参数让位（内存里的 next 已经是它）。
// 盘上原本那份是空的（首次运行只配了参数）就没有要保的东西，参数那份直接转正。
func (h *Hub) diskTargetFor(shown string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.disk.TargetDir != "" && !config.SamePath(h.disk.TargetDir, h.settings.TargetDir) &&
		config.SamePath(shown, h.settings.TargetDir) {
		return h.disk.TargetDir
	}
	return shown
}

// UseLang 把本轮的界面语言换成启动参数 --lang 给的那一个：只改内存，不动 settings.json。
//
// 认不出的值是**报错**而不是回落默认（口径和界面上那颗钮走的 SetLang 相反）：按钮永远只送得出
// 那三个词，而命令行上写错一个词的人看不见界面反馈，静默改回默认等于把他的意图吞掉。
func (h *Hub) UseLang(code string) error {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case config.LangAuto, config.LangZh, config.LangEn:
	default:
		return apperr.New(apperr.EInvalidArg, "界面语言只认 auto、zh、en").With("lang", code)
	}
	h.setRun(func(s *config.Settings) { s.Lang = code })
	return nil
}

// UseTheme 把本轮的配色换成启动参数 --theme 给的那一个。判据与 UseLang 同一条：
// 只认 dark、light，认不出来就报错而不是回落到深色。
func (h *Hub) UseTheme(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case config.ThemeDark, config.ThemeLight:
	default:
		return apperr.New(apperr.EInvalidArg, "配色只认 dark、light").With("theme", name)
	}
	h.setRun(func(s *config.Settings) { s.Theme = name })
	return nil
}

// UseDestFolder 把本轮的转移目录换成启动参数 --dest-folder 给的那一份：当场生效，
// 但盘上那一份原样不动（见 disk）。
//
// 校验用的是完整那一套 Validate，不是只看「这个路径能不能写」：一个会形成自搬环的临时
// 落点和别的自搬环一样危险。目录不存在就建 —— 命令行上没有「选目录」那颗按钮，
// 而「参数指了个还没造的目录」是这套用法最自然的样子。
//
// 调用时机两处都合法：启动序列里（只改内存，Start 会按这份建基线）与已经跑起来之后
// （重建监听才谈得上生效），函数自己认得出是哪一种。
func (h *Hub) UseDestFolder(dir string) error {
	next := h.cur()
	next.TargetDir = config.CleanAbs(dir)
	if err := next.Validate(); err != nil {
		return err
	}
	if err := config.EnsureTarget(next.TargetDir); err != nil {
		return err
	}
	h.mu.Lock()
	booted := h.started
	h.mu.Unlock()
	if booted {
		// 已经跑起来了：换落点必须重建监听才谈得上生效。
		return h.rebuild(next)
	}
	// 还在启动序列里（main.go 在建 Hub 之后、Start 之前打这一次）：只改内存就行，
	// Start 会按这份设置建基线 —— 抢先 rebuild 会把「先重放再建基线」那个顺序打乱。
	h.setRun(func(s *config.Settings) { s.TargetDir = next.TargetDir })
	return nil
}

// Start 是启动序列，顺序不许换：先重放崩溃账本，再动目录里的字节，最后才建基线。
//
// 基线必须最后建 —— 它会把每个已存在的路径都记成「程序起来之前就在这儿的老文件」，
// 而重放要恢复恰恰是这些路径里的某几个。
func (h *Hub) Start() error {
	s := h.cur()
	var watchErr error
	replays, err := h.replay()
	if err != nil {
		// 账本读不动不是致命伤：那只是对上次崩溃失忆，本轮监控照开。
		h.log.Error("重放没做成，照常启动", "err", err)
	}
	if err := h.rebuild(s); err != nil {
		// 监听开不起来（监控目录不存在、被重定向走了…）不致命：
		// 药丸必须照样出现并在上面说清为什么没在监控，而不是无声退出。
		h.log.Error("监听没起来，界面会显示原因", "err", err)
		watchErr = err
	}
	for _, r := range replays {
		h.machine.Load().Reclaim(r.Src, r.sample)
	}
	h.wg.Add(1)
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	go h.worker()
	if d, err := h.jr.Compact(time.Now()); err != nil {
		h.log.Warn("压缩账本没成", "err", err)
	} else if d > 0 {
		h.log.Info("账本已压缩", "dropped", d)
	}
	h.pushStatus("")
	return watchErr
}

// rebuild 用给定设置装好判定器与监听。改设置与开监听走的是同一个入口，
// 所以「新阈值什么时候生效」只有一条答案：下一次重建。
func (h *Hub) rebuild(s config.Settings) error {
	h.mu.Lock()
	h.settings = s
	h.stopRunnerLocked()
	h.mu.Unlock()

	mach := newMachine(s)
	snap, err := detect.Snapshot(s.SourceDir)
	if err != nil {
		h.log.Warn("建基线时看不了监控目录", "dir", s.SourceDir, "err", err)
	} else {
		mach.Baseline(snap)
		h.log.Info("基线已建", "items", len(snap))
	}
	h.machine.Store(mach)

	h.mu.Lock()
	err = h.startRunnerLocked()
	h.mu.Unlock()
	// 重建完必须刷一次药丸：h.reason 刚被 startRunnerLocked 改过，
	// 而 status 是快照不是查询 —— 不推这一次，界面上还挂着上一轮那句「没在监控」。
	h.pushStatus("")
	return err
}

func (h *Hub) startRunnerLocked() error {
	s := h.settings
	if h.quit || s.Paused {
		// 暂停是「拔掉事件源」，不是循环里加一个 if：这样「暂停期间账本零新增」
		// 由构造保证，而不是由某一次判定的运气保证。
		return nil
	}
	if !s.Ready() {
		// 没配转移目录时连监听都不开：开着它，每一个下载都会走完
		// 「判成完成 → 搬运被拒 → 记一笔失败」，账本里堆的全是注定的失败，
		// 而文件一个都不会动 —— 那正是首次运行时最该省下的噪音。
		h.log.Warn("还没配转移目录，暂不开监听")
		// 这里只记原因、不推状态：调用方（rebuild / SetPaused）末尾都会 pushStatus，
		// 两处都推就会有两个先后不一的快照，而那正是「闪一下又变回去」的来源。
		h.reason = notConfigured
		return nil
	}
	r, err := watch.New(watch.Config{Dir: s.SourceDir}, h)
	if err != nil {
		e := apperr.Of(err)
		h.log.Error("监听开不起来", "dir", s.SourceDir, "code", string(e.Code), "err", e.Err)
		h.reason = e.Msg
		return e
	}
	h.runner = r
	h.running = true
	// 对账往回追就从这一刻算：这之前落进目录的东西，我们从没负责过看见它的 Create。
	h.watchSince = time.Now()
	h.reason = ""
	h.log.Info("监听已开", "dir", s.SourceDir, "bufferBytes", r.BufferSize())
	go func() {
		err := r.Run(context.Background())
		switch {
		case errors.Is(err, watch.ErrStopped):
			h.log.Info("监听已收摊")
		case err != nil:
			h.log.Error("监听中途挂了", "err", err)
		}
		h.mu.Lock()
		h.running = false
		if err != nil && !errors.Is(err, watch.ErrStopped) {
			// 挂掉的原因要留在 hub 里：之后每一次状态刷新都得继续说这一句，
			// 否则一次「监听半路死了」会在下一次节拍被刷成「监控中」，那是假话。
			h.reason = apperr.Of(err).Msg
		}
		h.mu.Unlock()
		h.pushStatus("")
	}()
	return nil
}

func (h *Hub) stopRunnerLocked() {
	if h.runner == nil {
		return
	}
	if err := h.runner.Stop(); err != nil {
		h.log.Warn("监听没干净地停下", "err", err)
	}
	h.runner = nil
	h.running = false
}

// SetPaused 停/开监听。暂停不是「在循环里加一个 if」，而是把事件源整个拔掉 ——
// 这样「暂停期间账本零新增」是由构造保证的，而不是由某一次判定的运气保证的。
func (h *Hub) SetPaused(p bool) error {
	s := h.cur()
	s.Paused = p
	if err := h.store.Save(h.forDisk(s)); err != nil {
		return err
	}
	h.mu.Lock()
	h.settings = s
	if p {
		h.stopRunnerLocked()
	} else if h.runner == nil {
		err := h.startRunnerLocked()
		h.mu.Unlock()
		h.pushStatus("")
		return err
	}
	h.mu.Unlock()
	h.pushStatus("")
	return nil
}

// —— 设置变更 ——

// saveDTO 是配置页允许改的那几项。刻意不含 sourceDir / paused / anchor：界面送来的
// 是一份差量，其余字段一律从内存里当前设置继承 —— 把「没送」当成「用户要零值」，
// 保存按钮就成了清空转移目录的最短路径。
type saveDTO struct {
	TargetDir    string `json:"targetDir"`
	DateSubdirs  bool   `json:"dateSubdirs"`
	ToastEnabled bool   `json:"toastEnabled"`
	ToastSeconds int    `json:"toastSeconds"`
	QuietMs      int    `json:"quietMs"`
	MaxSettleMs  int    `json:"maxSettleMs"`
	// TargetDirPresets 是那颗 ▾ 上「删掉一条之后」的整串清单。它是唯一由界面说了算的
	// 清单字段（移除这个手势只有那里有），而当前落点那一条永远由后端补上，见 Save。
	TargetDirPresets []string `json:"targetDirPresets"`
}

func (d saveDTO) mergeInto(base config.Settings) config.Settings {
	base.TargetDir = d.TargetDir
	base.DateSubdirs = d.DateSubdirs
	base.ToastEnabled = d.ToastEnabled
	base.ToastSeconds = d.ToastSeconds
	base.QuietMs = d.QuietMs
	base.MaxSettleMs = d.MaxSettleMs
	// 空列表按「没送」处理，口径同上那几项：改一个提示秒数不该把用户的常用目录清单洗掉。
	// 清单里永远有当前落点那一条（Save 补），所以「真的空了」这个状态界面上送不出来。
	if len(d.TargetDirPresets) > 0 {
		base.TargetDirPresets = d.TargetDirPresets
	}
	return base
}

// Save 是配置页「保存」那一条完整的路：合并 → 校验 → 建目录 → 落盘 → 生效。
//
// 顺序是有讲究的：校验不过就不该有任何东西被写到盘上，
// 而目录建不出来时也不落盘 —— 否则设置里记着一个当前放不了文件的目录，
// 下一次下载会在搬运那一步才失败，那离用户点保存已经太远了。
func (h *Hub) Save(d saveDTO) error {
	next := d.mergeInto(h.cur())
	// 落盘的那一份和内存的那一份可以不同：本轮的目录/语言/配色可能来自启动参数，
	// 而面板送回来的那一项只是回显，不是用户改的（见 diskTargetFor）。
	persist := h.forDisk(next)
	persist.TargetDir = h.diskTargetFor(next.TargetDir)
	// 常用列表记的是**落盘那一份**目录：--dest-folder 本轮用的那个不该被记成永久常用，
	// 它压根没打算留下（口径同上面那两行对一次性参数的处理）。
	// 内存那份拿同一个列表：界面重画用的是后端回的 settings，两边各一套就会「删掉的又出现」。
	persist.RememberTarget()
	next.TargetDirPresets = persist.TargetDirPresets
	if err := next.Validate(); err != nil {
		return err
	}
	if err := config.EnsureTarget(persist.TargetDir); err != nil {
		return err
	}
	if err := h.store.Save(persist); err != nil {
		return err
	}
	// 到这一步才认下「盘上现在就是这一份」：一次失败的保存不该让内存以为某个目录已经存住了。
	h.commitDisk(persist)
	if err := h.Apply(next); err != nil {
		// 设置已经生效了，只是监听没起来。这时候骗用户「保存失败」会让他反复点保存，
		// 而真正要他去处理的是那个监控目录 —— 药丸上那行「没在监控」才是对的说法。
		h.log.Error("设置已存但监听没起来", "err", err)
	}
	return nil
}

// Relocate 重新定位监控目录：OneDrive 接管下载目录之后，只有注册表那个键会跟着变。
//
// 它当场落盘并生效。这个字段在界面上是只读的，唯一的改法就是这个按钮，
// 「还要再点一次保存才生效」等于把一件事拆成两步、而且只在第二步才校验。
func (h *Hub) Relocate() (string, error) {
	dir, err := config.DownloadsDir()
	if err != nil {
		return "", err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", apperr.Wrap(apperr.EFileOpFailed, "下载目录现在看不了", err).With("source", dir)
	}
	if !st.IsDir() {
		return "", apperr.New(apperr.EWatchUnavailable, "那个路径不是目录").With("source", dir)
	}
	next := h.cur()
	next.SourceDir = dir
	// 只填了监控目录、还没配转移目录时不跑整套校验：那时「先选一个转移目录」会拦住
	// 一个本来正当的重新定位，而用户此刻做的事就是配目录本身。
	if next.Ready() {
		if err := next.Validate(); err != nil {
			return "", err
		}
	}
	if err := h.store.Save(h.forDisk(next)); err != nil {
		return "", err
	}
	if err := h.Apply(next); err != nil {
		h.log.Error("换了监控目录但监听没起来", "dir", dir, "err", err)
	}
	return h.cur().SourceDir, nil
}

// SetAnchor 记下窗口到工作区右/下边的物理像素距离。窗口每次被挪动都会被调一次。
func (h *Hub) SetAnchor(a config.Anchor) error {
	s := h.cur()
	s.Anchor = a
	if err := h.store.Save(h.forDisk(s)); err != nil {
		return err
	}
	h.mu.Lock()
	h.settings = s
	h.mu.Unlock()
	return nil
}

// SetTheme 记下配色选择。它和 DateSubdirs 一样**不触发重建**：
// 外观改动打断一次在传的搬运没有道理（见 affectsRuntime 那段）。
//
// 认不出的名字一律按深色办而不是报错 —— 主题是一个偏好，不是一次判决，
// 配置写坏不该让任何按钮点不动（口径同 config.normalize）。
func (h *Hub) SetTheme(name string) error {
	if name != config.ThemeLight {
		name = config.ThemeDark
	}
	s := h.cur()
	if s.Theme == name {
		return nil // 没换就不写盘：这条按钮会被反复点到
	}
	s.Theme = name
	// 用户按下的这一项要存住，其余那几项可能来自启动参数的仍留在本轮：
	// 命令行上写了 `--dest-folder` 之后点一下配色，不该顺手把那个临时目录写成永久设置。
	d := h.forDisk(s)
	d.Theme = name
	if err := h.store.Save(d); err != nil {
		return err
	}
	h.commitDisk(d)
	return h.Apply(s)
}

// SetLang 记下界面语言选择。它和 SetTheme 是同一类：只换文案，不进 affectsRuntime 那张表，
// 所以换语言绝不会被误当成一次「值得把判定器和监听重做一遍」的改动。
//
// 落盘而不是只改内存：语言是一个跨启动的偏好，下次开屏还得是那句英文。
func (h *Hub) SetLang(code string) error {
	switch code {
	case config.LangAuto, config.LangZh, config.LangEn:
	default:
		code = config.LangAuto
	}
	s := h.cur()
	if s.Lang == code {
		return nil // 没换就不写盘：下拉框会被反复点到
	}
	s.Lang = code
	// 同 SetTheme：只有用户按下的这一项进盘，启动参数那几项仍留在本轮。
	d := h.forDisk(s)
	d.Lang = code
	if err := h.store.Save(d); err != nil {
		return err
	}
	h.commitDisk(d)
	return h.Apply(s)
}

// TodayTarget 是「查看」那颗按钮要看的地方。
type TodayTarget struct {
	Dir    string `json:"dir"`
	Dated  string `json:"dated"` // 用到的日期目录名，没开分目录时为空
	Exists bool   `json:"exists"`
	// Opened 是「查看」实际该打开的那个目录：今天的落点在就直接开它，不在就退到转移目录。
	// 空字符串表示两处都不在（转移目录被人删了），那时什么都不该开。
	Opened string `json:"opened"`
}

// TodayDir 算出今天的落点，只看不动磁盘。
//
// 两条刻意的取舍：
//   - **不建目录**。「查看」是一个读动作，用户可能只是想看一眼今天有没有东西；
//     为了回答「还没有」而先造出一个空文件夹，等于让每次点击都留下副作用。
//   - 日期取**今天**而不是搬运那天。mover 用的是源文件 mtime（补搬上周的东西要回到
//     上周那个目录），所以这个按钮可能打开一个空目录而别的目录里躺着今天补搬的文件 ——
//     界面那行提示要把这件事说清楚，别让用户以为丢了。
func (h *Hub) TodayDir(now time.Time) (TodayTarget, error) {
	s := h.cur()
	if s.TargetDir == "" {
		return TodayTarget{}, apperr.New(apperr.ENotConfigured, "还没设转移目录，没有「今天」可看。")
	}
	out := TodayTarget{Dir: s.TargetDir}
	if s.DateSubdirs {
		out.Dated = now.Format(mover.DateLayout)
		out.Dir = filepath.Join(s.TargetDir, out.Dated)
	}
	st, err := os.Stat(out.Dir)
	switch {
	case err == nil:
		out.Exists = st.IsDir()
	case errors.Is(err, os.ErrNotExist):
		out.Exists = false
	default:
		return out, apperr.Wrap(apperr.EFileOpFailed, "那个目录现在看不了", err).With("dir", out.Dir)
	}
	// 「今天还没有」不是错误，而用户点这颗按钮要的是「看一眼东西到底在哪」，
	// 所以落点不存在时退到转移目录本身 —— 不开第二颗按钮，也不弹一句报错。
	open := out.Dir
	if !out.Exists {
		open = s.TargetDir
	}
	if st, err := os.Stat(open); err == nil && st.IsDir() {
		out.Opened = open
	}
	return out, nil
}

// MovesLog 报搬运流水（moves.jsonl）的位置，以及这会儿它到底在不在。
//
// 「查看日志」那颗按钮靠 exists 决定开记事本还是说一句「还没有搬运记录」：
// 流水是每行现开现建的，一次都还没搬过时那个文件压根不存在 —— 那是个答案，
// 不是一次故障，所以既不报 E_FILE_OP_FAILED，也不提前造一个空文件留写盘副作用。
func (h *Hub) MovesLog() (string, bool) {
	p := h.jr.MovesPath()
	st, err := os.Stat(p)
	return p, err == nil && !st.IsDir()
}

// ClearMovesLog 清掉人类可读的流水。崩溃账本 pending.jsonl 由 journal.ClearMoves
// 保证一个字节不动，所以这里唯一的失败理由就是那个文件本身清不掉。
func (h *Hub) ClearMovesLog() error {
	if err := h.jr.ClearMoves(); err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "搬运流水没清掉", err)
	}
	return nil
}

// Apply 让已经落盘的新设置生效。落盘由调用方做完，这里只负责「让它跑起来」。
//
// 只有影响判定或监听的字段变了才重建：改个提示秒数不该让监控断一秒，
// 而重建要重扫目录、把每个文件的观察状态从头来过，那个代价是看得见的。
func (h *Hub) Apply(next config.Settings) error {
	if !affectsRuntime(h.cur(), next) {
		h.mu.Lock()
		h.settings = next
		h.mu.Unlock()
		h.pushStatus("")
		return nil
	}
	return h.rebuild(next)
}

// affectsRuntime 报告「这版设置的改变值不值得把判定器和监听重做一遍」。
//
// 四个刻意不在表里的字段：
//
//   - Paused 走 SetPaused 那条「拔掉事件源」的路。重建会顺带重新建基线，
//     而基线的作用正是「这些文件在程序起来之前就在这儿，别动」：正在传的那个
//     文件会被打回老文件那一票否决里，等它改名完成时就成了一个永远搬不走的东西。
//   - DateSubdirs 只决定落点在哪一天那个子目录里，搬运时现读现用（mover.Options.Settings
//     是个闭包）；判定与监听跟它一点关系都没有，为它重建等于让一次勾选打断一次在传。
//   - Theme 是纯外观，前端自己换 token，后端没有任何一处读它。
//   - Lang 同理，而且比 Theme 更没资格重建：后端连一个词的文案都不出，
//     界面换语言时正在传的那个文件却被打回「老文件」，是一笔纯粹赔出去的代价。
func affectsRuntime(old, next config.Settings) bool {
	return old.SourceDir != next.SourceDir ||
		old.TargetDir != next.TargetDir ||
		old.QuietMs != next.QuietMs ||
		old.MaxSettleMs != next.MaxSettleMs
}

// Shutdown 收尾：停监听、等在途的搬完、压一次账本。调多少次都只做一遍。
func (h *Hub) Shutdown() {
	h.shut.Do(h.shutdown)
}

func (h *Hub) shutdown() {
	h.mu.Lock()
	h.quit = true
	h.stopRunnerLocked()
	started := h.started
	h.mu.Unlock()

	if started {
		close(h.queue) // shutdown 至多跑一遍，所以这里关一次；worker 见关即收
		select {
		case <-h.closed:
		case <-time.After(30 * time.Second):
			h.log.Warn("等在途搬运超时了，直接收摊")
		}
		h.wg.Wait()
	}
	if d, err := h.jr.Compact(time.Now()); err != nil {
		h.log.Warn("退出时压缩账本没成", "err", err)
	} else if d > 0 {
		h.log.Info("退出时压缩了账本", "dropped", d)
	}
}

// —— watch.Handler：这三个方法只在 Runner 的那一条 goroutine 上被调 ——

func (h *Hub) OnEvent(ev detect.Event) { h.machine.Load().OnEvent(ev) }

func (h *Hub) OnTick() {
	m := h.machine.Load()
	for _, o := range m.Tick() {
		switch o.Kind {
		case detect.OutClaim:
			m.MarkClaimed(o.Path)
			select {
			case h.queue <- o.Path:
			default:
				// 队列排满说明 IO 线程被什么东西卡住了。退回观察，让它下一轮再判一次；
				// 静悄悄丢掉就是「文件永远没搬」，而那是最难查的一种缺件。
				m.MarkFailed(o.Path, "搬运排到了队尾")
				h.log.Warn("搬运队列排满了", "path", o.Path)
			}
		case detect.OutStuck:
			m.MarkAbandoned(o.Path)
			h.logMove(journal.Move{
				TS: time.Now(), Src: o.Path, Size: o.Sample.Size,
				Outcome: journal.OutcomeStuck, Reason: o.Reason,
			})
			h.fire("download2:stuck", map[string]any{"path": o.Path, "reason": o.Reason})
		case detect.OutAborted:
			h.logMove(journal.Move{
				TS: time.Now(), Src: o.Path, Outcome: journal.OutcomeAborted, Reason: o.Reason,
			})
			h.fire("download2:aborted", map[string]any{"path": o.Path, "reason": o.Reason})
		}
	}
	h.pushStatus("")
}

// reconcileBackfill 是对账往回追的时长上限。
const reconcileBackfill = 10 * time.Minute

// reconcileCutoff 定下这一次对账往回看到哪儿：最多回看 reconcileBackfill，但**绝不早于
// 监听起来的那一刻**。那之前就在目录里的东西，基线已经把它记成「程序起来之前就有的老文件」，
// 我们从没负责过看见它的 Create —— 对账再把它补成一次下载，等于把 Downloads 里任何一样
// 碰巧被改过的东西都当成待搬的（一份「启动前 9 秒到的文件在启动 61 秒后被搬走」的报告就是这么来的）。
// 夹住之后溢出恢复照样成立：漏掉事件的那个文件必然是在监听期间到的，mtime 晚于 watchSince。
func reconcileCutoff(now, watchSince time.Time) time.Time {
	c := now.Add(-reconcileBackfill)
	if watchSince.After(c) {
		return watchSince
	}
	return c
}

func (h *Hub) OnLost(err error) {
	s := h.cur()
	snap, serr := detect.Snapshot(s.SourceDir)
	if serr != nil {
		h.log.Warn("对账时看不了监控目录", "err", serr)
		return
	}
	h.machine.Load().Scan(reconcileCutoff(time.Now(), h.watchSinceNow()), snap)
	if !errors.Is(err, watch.ErrReconcile) {
		h.log.Warn("事件流不可信，已用目录对账", "err", err)
	}
	h.pushStatus("")
}

// —— 搬运线程 ——

func (h *Hub) worker() {
	defer h.wg.Done()
	defer close(h.closed)
	for p := range h.queue {
		h.settle(func() { h.machine.Load().MarkMoving(p) })
		res, err := h.mv.Move(p)
		if err != nil {
			e := apperr.Of(err)
			h.log.Warn("搬运没做成", "src", p, "code", string(e.Code), "msg", e.Msg, "err", e.Err)
			h.settle(func() { h.machine.Load().MarkFailed(p, e.Msg) })
			h.fire("download2:stuck", map[string]any{"path": p, "reason": e.Msg})
			h.pushStatus("")
			continue
		}
		h.settle(func() { h.machine.Load().MarkDone(p) })
		h.fire("download2:moved", movedPayload{
			Src: res.Src, Dst: res.Dst, Size: res.Size, Method: res.Method,
			Dated: res.Dated, Tries: res.Tries, Conflicts: res.Conflicts,
			MS: res.MS, PendingDelete: res.PendingDelete,
		})
		if res.PendingDelete {
			h.log.Info("副本已就位，源欠一次补删", "src", res.Src, "dst", res.Dst)
		}
		h.pushStatus("")
	}
}

// settle 把一次机器状态改动送回那条循环 goroutine。
func (h *Hub) settle(fn func()) {
	h.mu.Lock()
	r, running := h.runner, h.running
	h.mu.Unlock()
	if running && r != nil {
		r.Post(fn)
		return
	}
	fn()
}

// —— 状态与流水 ——

func (h *Hub) baseStatus(s status) status {
	// 一次加锁读齐设置与原因：分两次锁会读到「改了但还没生效」的中间态，
	// 而那种态在界面上的表现是一行说不上哪里不对的文案。
	h.mu.Lock()
	cur, reason, running := h.settings, h.reason, h.running
	h.mu.Unlock()
	m := h.machine.Load()
	if s.SourceDir == "" {
		s.SourceDir = cur.SourceDir
	}
	if s.TargetDir == "" {
		s.TargetDir = cur.TargetDir
	}
	s.Paused = cur.Paused
	s.Watching = !cur.Paused && running
	if m != nil {
		s.Pending = m.Pending()
		s.Live = m.LiveDownloads()
	}
	if s.Reason == "" {
		s.Reason = reason
	}
	return s
}

func (h *Hub) isRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// watchSinceNow 读最近一次监听起来的那一刻。还没监听过给的是零值 —— 那不是「从公元 1 年
// 开始追」，reconcileCutoff 会因为它而退回正常的回看窗口。
func (h *Hub) watchSinceNow() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.watchSince
}

// Status 给 UI 线程读：它不加 machine 的锁，读的是循环 goroutine 写过的那份小快照。
// 直接去问 Machine.Pending() 会让两条线程同时碰同一张 map。
func (h *Hub) Status() status {
	h.smu.Lock()
	defer h.smu.Unlock()
	return h.lastStatus
}

// pushStatus 刷一次药丸。reason 传空串表示「这一次没什么新话要说」，
// 此时沿用 hub 里一直记着的那句 —— 否则每 500ms 一次的刷新会把
// 「监听开不起来」冲成空白，界面下一拍就显示成「监控中」。
func (h *Hub) pushStatus(reason string) {
	h.setStatus(h.baseStatus(status{Reason: reason}))
}

func (h *Hub) setStatus(s status) {
	h.smu.Lock()
	h.lastStatus = s
	h.smu.Unlock()
	h.fire("download2:status", s)
}

func (h *Hub) fire(name string, payload any) {
	if h.emit == nil {
		return
	}
	h.emit(name, payload)
}

func (h *Hub) logMove(mv journal.Move) {
	if err := h.jr.LogMove(mv); err != nil {
		h.log.Error("流水记不上", "err", err)
	}
}

// —— 崩溃账本的重放执行侧 ——

type replayRow struct {
	journal.Record
	sample detect.Sample
}

// replay 先重放再建基线。返回的记录是「源还在、该回到观察」那一类，
// 由调用方在建完基线之后逐条 Reclaim —— 顺序反了就会被基线一票否决。
func (h *Hub) replay() ([]replayRow, error) {
	actions, skipped, err := h.jr.Replay(viewer{})
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		// 静默吞掉账本行等于对崩溃历史撒谎，所以这一句必须落到日志里。
		h.log.Warn("账本有读不懂的行，已跳过", "skipped", skipped)
	}
	var rewatch []replayRow
	for _, a := range actions {
		r := a.Record
		switch a.Kind {
		case journal.ActionNone:
		case journal.ActionRewatch:
			s, serr := detect.StatSample(r.Src)
			if serr != nil {
				h.log.Warn("要重看的文件已经看不得了", "src", r.Src, "err", serr)
				continue
			}
			rewatch = append(rewatch, replayRow{r, s})
		case journal.ActionCleanTmp:
			if err := os.Remove(r.Tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
				h.log.Warn("清中间名没清掉", "tmp", r.Tmp, "err", err)
				continue
			}
			h.log.Info("清掉了上次没处理完的中间名", "tmp", r.Tmp)
		case journal.ActionFinishDelete:
			if err := h.removeSource(r.Src); err != nil {
				h.log.Warn("补删源没删成", "src", r.Src, "err", err)
				continue
			}
			h.done(r, "崩溃后补删了源")
		case journal.ActionAdopt:
			h.done(r, "崩溃后认下了已就位的副本")
		case journal.ActionAbandon:
			r.State = journal.StateAbandoned
			r.Reason = a.Reason
			if err := h.jr.Append(r); err != nil {
				h.log.Warn("放弃那笔记不上账", "src", r.Src, "err", err)
			}
			// 这里刻意不再补一行流水：abandoned 的流水由 Compact 在它出账时写一次，
			// 两处都写就是同一件事记两遍。
			h.log.Info("这笔说不清，只记日志不碰字节", "src", r.Src, "reason", a.Reason)
		case journal.ActionForget:
			// 「两边都不在了」：用户在别处删了它。标成 done 让它出账，
			// 而不是留在账本里每轮重放一次 —— 但它不进流水，因为并没有什么东西被搬走。
			r.State = journal.StateDone
			r.Reason = a.Reason
			if err := h.jr.Append(r); err != nil {
				h.log.Warn("作废那笔记不上账", "src", r.Src, "err", err)
			}
		default:
			return nil, apperr.New(apperr.EInternal, fmt.Sprintf("重放给出了没见过的动作：%s", a.Kind))
		}
	}
	return rewatch, nil
}

func (h *Hub) done(r journal.Record, reason string) {
	r.State = journal.StateDone
	r.Reason = reason
	if err := h.jr.Append(r); err != nil {
		h.log.Error("结不了账", "src", r.Src, "err", err)
		return
	}
	h.logMove(journal.Move{
		TS: r.TS, Src: r.Src, Dst: r.Dst, Size: r.DstSize,
		Method: r.Method, Outcome: journal.OutcomeMoved, Reason: reason,
	})
	h.log.Info("崩溃恢复补完了一单", "src", r.Src, "dst", r.Dst, "reason", reason)
}

// removeSource 是重放侧唯一删用户文件的地方，所以那句判据留在这儿再说一遍：
// 只有目标已就位且字节数与账本完全一致才走到这里（journal.Plan 保证）。
func (h *Hub) removeSource(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// viewer 是 journal.Observer 的真实现：只看，不改。
type viewer struct{}

func (viewer) Observe(r journal.Record) (journal.View, error) {
	var v journal.View
	s, err := detect.StatSample(r.Src)
	switch {
	case err == nil:
		v.SrcExists, v.SrcSize, v.SrcMtime, v.SrcIndex = true, s.Size, s.Mtime, s.Index
	case errors.Is(err, os.ErrNotExist):
		// -src 不在，SrcExists 保持 false
	default:
		return v, err
	}
	if r.Tmp != "" {
		if st, statErr := os.Stat(r.Tmp); statErr == nil {
			v.TmpExists = !st.IsDir()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return v, statErr
		}
	}
	if r.Dst != "" {
		if st, statErr := os.Stat(r.Dst); statErr == nil {
			v.DstExists, v.DstSize = !st.IsDir(), st.Size()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return v, statErr
		}
	}
	return v, nil
}

// movedPayload 是 download2:moved 的负载。字段名必须是小写 json tag：
// mover.Result 直接发出去的话 JS 拿到的是 Src/Dst 这种大写名，前端读不到。
type movedPayload struct {
	Src           string `json:"src"`
	Dst           string `json:"dst"`
	Size          int64  `json:"size"`
	Method        string `json:"method"`
	Dated         string `json:"dated"`
	Tries         int    `json:"tries"`
	Conflicts     int    `json:"conflicts"`
	MS            int64  `json:"ms"`
	PendingDelete bool   `json:"pendingDelete"`
}
