// Package detect 只回答一个问题：这个文件下载完了吗。
//
// 它不碰文件系统，也不决定怎么搬。所有输入由调用方归一化成 Event 与 Tick，
// 所有对外部的查询都走 Sampler / Locker / Clock 三个接口。
//
// 这条边界是设计书 T4 六条硬案子可测的前提：「它下载完了吗」的失败模式（判早了，
// 把半成品搬走）和「我怎么安全地搬它」的失败模式完全不同、需要的替身也完全不同，
// 所以判定必须能被三个假对象完全驱动。
package detect

import (
	"sort"
	"time"
)

// State 是设计书里那条链：基线 → 观察 → 静置 → 就绪 → 记账 → 搬运 → 终态。
type State int

const (
	StateBaseline State = iota
	StateWatching
	StateSettling
	StateReady
	StateClaimed
	StateMoving
	StateDone
	StateStuck
	StateAbandoned
)

func (s State) String() string {
	switch s {
	case StateBaseline:
		return "baseline"
	case StateWatching:
		return "watching"
	case StateSettling:
		return "settling"
	case StateReady:
		return "ready"
	case StateClaimed:
		return "claimed"
	case StateMoving:
		return "moving"
	case StateDone:
		return "done"
	case StateStuck:
		return "stuck"
	case StateAbandoned:
		return "abandoned"
	}
	return "unknown"
}

// inFlight 是「已经交给搬运器，机器别再对它做判断」的那一段。
// 搬运期间的 EvRemove 是我们自己改名造成的，按「文件消失」丢弃会把账本状态一起丢掉。
func (s State) inFlight() bool {
	return s == StateReady || s == StateClaimed || s == StateMoving
}

type EventKind int

const (
	EvCreate     EventKind = iota // 名字出现
	EvWrite                       // 内容变了
	EvRemove                      // 名字消失
	EvRenameFrom                  // 改名左侧：老名字消失
)

// Event 是 watch 归一化之后的输入。
//
// Machine 不是并发安全的：所有入口都得从同一个事件循环里调用，
// 否则 entries 会撕。搬运器回调也走那个循环。
type Event struct {
	Kind EventKind
	Path string
}

type OutcomeKind int

const (
	OutClaim   OutcomeKind = iota // 判据全成立，可以去搬了
	OutStuck                      // 预算耗尽，文件原地不动
	OutAborted                    // 下载没完成就没了，候选丢弃
)

// Outcome 是交给调用方的动作。OutClaim 带采样快照，正是要写进崩溃账本的那份身份。
type Outcome struct {
	Kind   OutcomeKind
	Path   string
	Sample Sample
	Reason string
}

type Config struct {
	Quiet     time.Duration
	MaxSettle time.Duration
}

const (
	defaultQuiet     = 900 * time.Millisecond
	defaultMaxSettle = 10 * time.Minute

	// minStableReadings = 2 就是「连续两次采样读数完全一致」：
	// 第一次立基准，第二次相等才开始计数。
	minStableReadings = 2

	// renameVetoWindow 是「改名走了的那个身份还能拿来否决新名字」的那段时间。
	// 改名对的两条事件本该在同一批里前后到达，60 秒是给事件循环被别的事卡住时留的余量。
	// 再拉长就成了「这个文件对象永远搬不走」：用户今天改名、下周真的重新下载同名文件，
	// 那条记录要是还在，就会被一次证据早已过期的否决挡掉。
	renameVetoWindow = 60 * time.Second

	// renameVetoCap 是一次批量重命名能占住的记录条数上限。
	// 一整个文件夹被改名时会一次冒出上百条身份，留着几千条只会让每个新名字多跑几千次比较。
	renameVetoCap = 128
)

// renamedRecord 是一条「这个名字改名走了，它当时的身份是这个」。
type renamedRecord struct {
	id Sample
	at time.Time
}

type entry struct {
	state     State
	candidate bool
	// base 是基线记下的身份。非空时，采到「还是当初那个文件对象」就一票否决：
	// 老文件被重新通知一遍、或者被人打开改一笔再存盘，都不等于新下载。
	base *Sample
	// lastChange 是「最后一次它有动静」：事件时刻、采样 mtime、size 变化三者的较大值。
	lastChange time.Time
	// since 是进入候选的时刻，预算从它算；失败退回不清零，所以坏文件不会永远占着名额。
	since   time.Time
	prev    *Sample
	sameRun int
	tries   int
	reason  string
}

// Machine 是下载完成判定的状态机。
type Machine struct {
	quiet     time.Duration
	maxSettle time.Duration
	sam       Sampler
	lock      Locker
	clk       Clock

	entries   map[string]*entry
	liveTemps map[string]struct{}
	// aborts 记下「某个临时名消失了，它的最终名可能被判成取消」，等下一轮 tick 再定。
	//
	// 改名对的两条事件是同一批里前后到达的（Rename(老) 紧跟 Create(新)），
	// 当场就报 aborted 会把每一次正常下载都说成失败。
	aborts map[string]string // 最终名 → 消失的临时名
	// newArrivals 数的是「这一轮窗口里有没有冒出一个我们不认识的名字」。
	//
	// 它用来给 aborts 消音：下载器把临时名改成什么名字我们无从知道（Chrome 是去掉
	// 后缀，别的工具可能顺手改了文件名），窗口里有新名字出现就说明这次改名大概率
	// 成功了。报「下载未完成」是假话，漏一行「未完成」只是少一条流水 —— 选后者。
	newArrivals int
	// renamedAway 记下「一个我们认识的名字改名走了，它当时的身份是什么」。
	//
	// 没有这一笔，资源管理器里一次普通的重命名和 Chrome 的下载完成在事件流上完全同形
	// （都是 Rename(老) + Create(新)），而基线那票否决只比同一路径的身份 —— 换了名字
	// 就绕过去了。用户第一轮试用报的就是这条：把 发票.pdf 改成 发票(1).pdf 会被搬走。
	renamedAway []renamedRecord
}

func New(cfg Config, sam Sampler, lock Locker, clk Clock) *Machine {
	if cfg.Quiet <= 0 {
		cfg.Quiet = defaultQuiet
	}
	if cfg.MaxSettle <= cfg.Quiet {
		cfg.MaxSettle = defaultMaxSettle
	}
	if clk == nil {
		clk = RealClock{}
	}
	return &Machine{
		quiet:     cfg.Quiet,
		maxSettle: cfg.MaxSettle,
		sam:       sam,
		lock:      lock,
		clk:       clk,
		entries:   make(map[string]*entry),
		liveTemps: make(map[string]struct{}),
		aborts:    make(map[string]string),
	}
}

// ScanEntry 是一次目录快照里的一项。
type ScanEntry struct {
	Path   string
	Sample Sample
}

// Baseline 是启动时建基线：把快照里的每一项都记成「程序起来之前它就在这儿了」。
//
// 这里刻意不比较 mtime。文件系统记下的写入时间可能比我们读时钟晚几毫秒
// （本机实测过一次：WriteFile 返回之后落下的 mtime 反而在 cutoff 之后），
// 拿它当「是不是新到的」的依据，等于每次启动都可能把一两个老文件误判成新下载搬走。
// 判断「新不新」由事件流负责；基线只负责否决，见 entry.base 与 tick 里的同身份判定。
func (m *Machine) Baseline(snap []ScanEntry) {
	for _, it := range snap {
		p := it.Path
		if IsTempPath(p) {
			// 上次退出时还在下载的那个：记成存活的临时名，等它改名或消失。
			m.liveTemps[p] = struct{}{}
			continue
		}
		if IsDenied(p) || it.Sample.IsDir || it.Sample.Hidden {
			continue
		}
		if e, ok := m.entries[p]; ok && e.state.inFlight() {
			continue // 已经在搬运器手上了，别插手
		}
		s := it.Sample
		m.entries[p] = &entry{state: StateBaseline, base: &s}
	}
}

// Reclaim 把一条「上次已经判定完成、却没搬完」的路径重新拉回观察，只给崩溃恢复用。
//
// 调用顺序有讲究：必须先 Baseline 再 Reclaim。基线给每个路径都记了身份，
// 而 tick 里的同身份判定是一票否决 —— 不清掉 base，下一次采样就把它原样退回 Baseline，
// 重放等于什么都没做（和 Scan 里那句注释同一个道理）。
//
// lastChange 用调用方给的 mtime 而不是现在：崩溃前它就已经静置了，
// 从此刻重新静置一遍只是把该搬的东西多压一会儿。
func (m *Machine) Reclaim(p string, s Sample) {
	e := m.entries[p]
	if e == nil {
		e = &entry{}
		m.entries[p] = e
	}
	e.base = nil
	m.promote(e, m.clk.Now(), s.Mtime)
}

// Scan 是事件溢出（ReadDirectoryChangesW 缓冲区丢事件）之后的重扫。
// 调用方传 cutoff = 「程序启动 - 5 分钟」：快照里 mtime 不早于它的普通文件被重新导出为候选，
// 因为那段时间里我们本该看到它的 Create 却没看到。
//
// 启动建基线请用 Baseline，别用这里 —— 那条路不跟时钟较劲。
// 已经追到的路径只有两种情况要补成候选：mtime 新过 cutoff，或者身份变了（名字被重写过）。
func (m *Machine) Scan(cutoff time.Time, snap []ScanEntry) {
	now := m.clk.Now()
	seen := make(map[string]struct{}, len(snap))

	for _, it := range snap {
		p := it.Path
		seen[p] = struct{}{}

		if IsTempPath(p) {
			m.liveTemps[p] = struct{}{}
			continue
		}
		if IsDenied(p) || it.Sample.IsDir || it.Sample.Hidden {
			continue
		}

		if e, ok := m.entries[p]; ok {
			if e.state == StateBaseline && (!it.Sample.Mtime.Before(cutoff) ||
				(e.base != nil && !sameIdentity(*e.base, it.Sample))) {
				// 走到这里就是调用方明确判定「这算一次下载」，
				// 基线身份那票否决证据随之作废 —— 不然 tick 会立刻把它退回基线，
				// 重扫等于什么都没做。
				e.base = nil
				m.promote(e, now, it.Sample.Mtime)
			}
			continue
		}

		e := &entry{}
		if it.Sample.Mtime.Before(cutoff) {
			s := it.Sample
			e.state = StateBaseline
			e.base = &s
		} else {
			m.promote(e, now, it.Sample.Mtime)
		}
		m.entries[p] = e
	}

	// 快照里没有的路径已经不在了；在途的除外，那是搬运器正拿着的。
	for p, e := range m.entries {
		if _, ok := seen[p]; !ok && !e.state.inFlight() {
			delete(m.entries, p)
		}
	}
	for p := range m.liveTemps {
		if _, ok := seen[p]; !ok {
			m.dropTemp(p)
		}
	}
}

// promote 把一个路径变成候选。lastChange 取它的 mtime（但不能晚于现在，
// 否则静置窗口会被未来的 mtime 永远挡住），于是从快照重新进入的文件也要先等满静置。
//
// base 故意不清：基线身份是「这不算新下载」的唯一否决证据，候选期间还得靠它比对。
// 真被否决时 tick 会把状态退回 StateBaseline。
func (m *Machine) promote(e *entry, now, mtime time.Time) {
	e.state = StateWatching
	e.candidate = true
	e.since = now
	e.prev = nil
	e.sameRun = 0
	e.reason = ""
	if mtime.After(now) {
		mtime = now
	}
	e.lastChange = mtime
}

// OnEvent 喂进一个归一化后的事件。它只更新追踪状态，不产出动作 ——
// 「能不能搬」必须由带节拍的 Tick 判定，事件本身没有资格下结论。
func (m *Machine) OnEvent(ev Event) {
	now := m.clk.Now()
	switch ev.Kind {
	case EvCreate, EvWrite:
		m.onArrive(ev.Path, now)
	default:
		if IsTempPath(ev.Path) {
			m.dropTemp(ev.Path)
			return
		}
		if ev.Kind == EvRenameFrom {
			m.noteRenameFrom(ev.Path, now)
		}
		m.onGone(ev.Path)
	}
}

// noteRenameFrom 在一个老名字被丢弃之前，把它当时的身份记一笔。
//
// 只有「非临时名」的改名走才要记：临时名改成最终名就是下载完成的标准收尾，
// 记下它的身份等于把右半边否决掉 —— 那会把 T4 硬案子① 判成搬不动。
//
// 身份取 prev（上一次采样）而不是当场再采一次：改名当时文件多半还被占用着、
// 或者已经不存在了（改名成功那一刻老路径就没了），手上已有的读数才是它改名前的样子。
func (m *Machine) noteRenameFrom(p string, now time.Time) {
	e := m.entries[p]
	if e == nil {
		return
	}
	if e.state.inFlight() {
		// 在途的名字改名走是我们自己搬家的动作，不是用户重命名，不该拿去否决任何人。
		return
	}
	id := e.prev
	if id == nil {
		id = e.base
	}
	if id == nil {
		return // 一个读数都还没有，说不清它是什么，就别假装知道
	}
	m.renamedAway = append(m.renamedAway, renamedRecord{id: *id, at: now})
	if len(m.renamedAway) > renameVetoCap {
		m.renamedAway = append(m.renamedAway[:0], m.renamedAway[len(m.renamedAway)-renameVetoCap:]...)
	}
}

// matchesRenamed 回答「这份读数和最近改名走开的某个东西是不是同一个」。
//
// 判据用 sameIdentity（size + mtime + fileIndex）：改名在 NTFS 上换名字不换对象，
// 三个数全都不动；而新下载一定是新的 fileIndex、mtime 也是刚刚。
//
// 一条记录被用掉之后不划掉：一次重命名之后紧跟着的可能还有第二次（用户连着改，
// 或者一批文件一起改），留着下次照样能认。过期由 renameVetoWindow 管，
// 上限由 renameVetoCap 管，两个都在 Tick 里顺手做，不需要别的清理入口。
// 代价写进 README：编号读不到（exFAT）时只剩 size + mtime 两个数，
// 一次「和某个刚改名的文件分毫不差的新下载」会被漏掉。
func (m *Machine) matchesRenamed(s Sample) bool {
	for _, r := range m.renamedAway {
		if sameIdentity(r.id, s) {
			return true
		}
	}
	return false
}

func (m *Machine) pruneRenamed(now time.Time) {
	if len(m.renamedAway) == 0 {
		return
	}
	kept := m.renamedAway[:0]
	for _, r := range m.renamedAway {
		if now.Sub(r.at) < renameVetoWindow {
			kept = append(kept, r)
		}
	}
	m.renamedAway = kept
}

func (m *Machine) onArrive(p string, now time.Time) {
	if IsTempPath(p) {
		m.liveTemps[p] = struct{}{}
		return
	}
	// 这个名字出现了，之前那次「临时名消失」就不是取消而是改名。
	delete(m.aborts, p)

	if IsDenied(p) {
		return
	}

	e, ok := m.entries[p]
	if !ok {
		// 陌生新名字。它可能是 Chrome 改名上岗的最终名，也可能是用户刚刚把一份老文件
		// 重命名成了这个样子 —— 两者的事件形状一模一样，只有当场采一次样能分开。
		//
		// 只在「我们不认识这个路径」时多花这一次 stat：认识的走下面那条身份否决，
		// 在途的碰都不碰。采样失败就当没这回事（下载器改名之后紧接着占用它很常见），
		// 交给下一轮 Tick 判定，别把「读不到」说成「不是改名来的」以外的一切结论。
		if s, err := m.sam.Sample(p); err == nil {
			if m.matchesRenamed(s) {
				cp := s
				m.entries[p] = &entry{state: StateBaseline, base: &cp, lastChange: now}
				// 它确实是个新出现在目录里的名字，消音照旧：刚改名走的下载不能说成取消。
				m.newArrivals++
				return
			}
		}
		m.entries[p] = &entry{state: StateWatching, candidate: true, since: now, lastChange: now}
		m.newArrivals++
		return
	}
	if e.state.inFlight() {
		// 搬运期间我们自己也会在目标名上写 tmp 再改名，别再插手。
		return
	}
	if e.state == StateStuck {
		//  stuck 之后又有动静：文件名被重写过或者下载续上了，预算从头算。
		m.promote(e, now, now)
		return
	}
	e.lastChange = now
	if !e.candidate {
		m.promote(e, now, now)
	}
}

func (m *Machine) onGone(p string) {
	e, ok := m.entries[p]
	if !ok || e.state.inFlight() {
		return
	}
	delete(m.entries, p)
}

// dropTemp 处理临时名消失。它同时解除对应最终名的阻塞 ——
// 「没有任何存活的临时路径指向 P」这一条判据就靠这里。
//
// 是不是「下载被取消」不在这里下结论：改名对的 Create(最终名) 就在下一条事件里，
// 先记一笔，交给 Tick 判定（见 Machine.aborts）。
func (m *Machine) dropTemp(temp string) {
	delete(m.liveTemps, temp)
	if final := FinalPathFor(temp); final != "" {
		m.aborts[final] = temp
	}
}

// Tick 推进一轮判定：所有候选各采一次样，判据齐了就地交给搬运器。
// 调用方按 500ms 的节拍驱动它。
//
// 采样顺序按路径字典序，为的是 Outcome 的次序可预测 —— 表驱动测试要断言整批结果，
// 随机顺序会让同一份输入每次跑出不同答案。
func (m *Machine) Tick() []Outcome {
	now := m.clk.Now()
	var out []Outcome
	m.pruneRenamed(now)
	out = append(out, m.flushAborts()...)

	for _, p := range m.candidates() {
		e := m.entries[p]
		if e == nil {
			continue
		}

		s, err := m.sam.Sample(p)
		if err != nil {
			delete(m.entries, p)
			continue
		}
		// 只有普通文件有资格被搬：目录（别人往 Downloads 拷一整个文件夹）
		// 与隐藏/系统文件在这里挡，光靠名字清单挡不住。
		if s.IsDir || s.Hidden || IsDenied(p) {
			delete(m.entries, p)
			continue
		}
		if e.base != nil && sameObject(s, *e.base) {
			// 还是当初那一个文件对象，就不是新下载：被人重新通知一遍也好、
			// 被打开改一笔再存盘也好（字节数和时间戳都变了但 fileIndex 没变），
			// 搬走它都是把用户的老文件当成下载。见 sameObject。
			e.state = StateBaseline
			e.candidate = false
			continue
		}

		m.observe(e, s, now)

		reason := m.block(e, p, s, now)
		switch {
		case reason == "":
			e.state = StateReady
			e.reason = ""
			out = append(out, Outcome{Kind: OutClaim, Path: p, Sample: s})
		case now.Sub(e.since) >= m.maxSettle:
			// 预算烧完。文件原地不动，只落一条日志加一个提示 ——
			// 再试下去也不会更好，而「一直重试」会让人以为工具坏了。
			e.state = StateStuck
			e.reason = reason
			out = append(out, Outcome{Kind: OutStuck, Path: p, Reason: reason})
		default:
			e.state = StateSettling
			e.reason = reason
		}
	}
	return out
}

// flushAborts 结算上一轮记下的「临时名没了」。
//
// 到这一轮为止，那个最终名要么已经出现过（onArrive 已经把记录删掉），
// 要么我们手上已经有一条它的候选记录 —— 两种都说明磁盘上确实有这个东西，
// 报「下载被取消」就是假话，静默丢掉。
//
// 窗口里冒出过新名字时也整批丢掉：下载器改名之后的名字不一定还是「去掉后缀」
// 那个形态（改成什么我们无从知道），这时那条「未完成」说的正是刚刚搬成功了的文件。
// 剩下的才是真被取消的下载：它连名字都没留下，只能靠这一行流水让用户知道「那次下载没成」。
func (m *Machine) flushAborts() []Outcome {
	n := m.newArrivals
	m.newArrivals = 0
	if len(m.aborts) == 0 {
		return nil
	}
	finals := make([]string, 0, len(m.aborts))
	for f := range m.aborts {
		finals = append(finals, f)
	}
	sort.Strings(finals)

	var out []Outcome
	for _, f := range finals {
		temp := m.aborts[f]
		delete(m.aborts, f)
		if n > 0 {
			continue // 这一轮里有新名字出现，说明改名多半成功了，别说它失败了
		}
		if _, tracked := m.entries[f]; tracked {
			continue
		}
		out = append(out, Outcome{Kind: OutAborted, Path: f, Reason: "临时名 " + temp + " 消失了，下载没完成"})
	}
	return out
}

// observe 记下这次读数，并把它转成判据要用的两个量：稳定次数与最后动静时刻。
func (m *Machine) observe(e *entry, s Sample, now time.Time) {
	if s.Mtime.After(e.lastChange) {
		e.lastChange = s.Mtime
	}
	if e.prev != nil && e.prev.equal(s) {
		e.sameRun++
	} else {
		// size 变了就是还在写。mtime 不能完全信：下载器会预分配最终大小、
		// 也可能写完最后几 KB 就不动 mtime，那样静置窗口会形同虚设。
		if e.prev != nil && e.prev.Size != s.Size {
			e.lastChange = now
		}
		e.sameRun = 1
	}
	cp := s
	e.prev = &cp
}

// block 返回第一条不成立的判据，齐了返回空串。
//
// 顺序是按价钱排的：字节数与名字是现成的，静置和稳定只看内存，
// 占用探测要开一次文件句柄，所以放最后 —— 一个正在下的文件根本走不到那句。
func (m *Machine) block(e *entry, p string, s Sample, now time.Time) string {
	if s.Size <= 0 {
		return "文件还是 0 字节"
	}
	if t := m.liveTempFor(p); t != "" {
		return "它的临时名还挂着"
	}
	if d := now.Sub(e.lastChange); d < m.quiet {
		return "还在写入"
	}
	if e.sameRun < minStableReadings {
		return "连续采样还没稳定"
	}
	if m.lock.Locked(p) {
		return "文件还被别的程序占用"
	}
	return ""
}

func (m *Machine) liveTempFor(p string) string {
	for _, t := range TempPathsFor(p) {
		if _, ok := m.liveTemps[t]; ok {
			return t
		}
	}
	return ""
}

func (m *Machine) candidates() []string {
	ps := make([]string, 0, len(m.entries))
	for p, e := range m.entries {
		if e.candidate && (e.state == StateWatching || e.state == StateSettling) {
			ps = append(ps, p)
		}
	}
	sort.Strings(ps)
	return ps
}

// MarkClaimed 是设计书里「先记账再动字节」的那道缝：账本写成功了才调，
// 之后机器就不再判定这个路径了。
func (m *Machine) MarkClaimed(p string) { m.set(p, StateClaimed) }

func (m *Machine) MarkMoving(p string) { m.set(p, StateMoving) }

// MarkDone 把记录删掉。留档由账本与 moves.jsonl 负责，机器只关心在途的事。
func (m *Machine) MarkDone(p string) { delete(m.entries, p) }

func (m *Machine) MarkAbandoned(p string) {
	if e := m.entries[p]; e != nil {
		e.state = StateAbandoned
		e.candidate = false
	}
}

// MarkFailed 退回重试：累加次数、重新立静置基准（占用说明它其实还没准备好），
// 但预算起点不动，所以搬不动的文件最终一定会变成 Stuck 而不是永远在试。
func (m *Machine) MarkFailed(p, reason string) {
	e := m.entries[p]
	if e == nil {
		return
	}
	e.state = StateSettling
	e.candidate = true
	e.tries++
	e.prev = nil
	e.sameRun = 0
	e.reason = reason
	e.lastChange = m.clk.Now()
}

func (m *Machine) set(p string, s State) {
	if e := m.entries[p]; e != nil {
		e.state = s
	}
}

// StateOf 给测试与界面用；路径没被追踪过就返回 false，别把「不认识」说成「搬完了」。
func (m *Machine) StateOf(p string) (State, bool) {
	e := m.entries[p]
	if e == nil {
		return 0, false
	}
	return e.state, true
}

// TriesOf 是已经试过几轮搬运。账本记的就是这个数，排查「为什么一直搬不动」全靠它。
func (m *Machine) TriesOf(p string) int {
	if e := m.entries[p]; e != nil {
		return e.tries
	}
	return 0
}

// BlockReason 返回当前卡住判据的那一句，判据齐了返回空串。
// 界面与 Stuck 提示文案共用这一句，所以它必须和判定用的是同一段代码。
func (m *Machine) BlockReason(p string) string {
	e := m.entries[p]
	if e == nil {
		return ""
	}
	if e.reason != "" {
		return e.reason
	}
	if !e.candidate || e.state.inFlight() {
		return ""
	}
	s, err := m.sam.Sample(p)
	if err != nil {
		return "文件已经不在了"
	}
	return m.block(e, p, s, m.clk.Now())
}

// Pending 是「机器手上还有几个正在等的文件」，药丸的状态点用它决定要不要显示忙。
// Stuck 与 Abandoned 不算：它们已经不再推进，把界面永远点亮成「忙」是假话。
func (m *Machine) Pending() int {
	n := 0
	for _, e := range m.entries {
		if e.state.inFlight() || (e.candidate && (e.state == StateWatching || e.state == StateSettling)) {
			n++
		}
	}
	return n
}

// LiveDownloads 数存活的临时名个数。
func (m *Machine) LiveDownloads() int { return len(m.liveTemps) }
