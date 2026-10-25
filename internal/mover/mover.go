// Package mover 是全项目唯一碰 src/dst 字节的地方。
//
// 为什么和 detect 拆开：两边失败模式完全不同。「它下载完了吗」错了会把半成品搬走，
// 「怎么安全地搬」错了会把用户的文件弄丢，而后者需要的现场（真文件、真句柄、真卷号）
// 是前者根本不该掺和的。拆开后每一侧都能有自己的替身。
//
// 全包装进两条路：同一个卷上是一次改名（零字节拷贝），跨卷或网络盘是复制 + 校验字节数。
// 只有一条规矩贯穿到底：**任何一步都不得让源文件先于目标消失**。
package mover

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"download2/internal/apperr"
	"download2/internal/detect"
	"download2/internal/journal"
)

// Method* 是两种手段的稳定名字，同时也是账本里 method 字段的取值（磁盘契约，改了和老账本对不上）。
const (
	MethodRename = "rename"
	MethodCopy   = "copy"
	// DateLayout 是日期子目录的目录名格式。它同时是磁盘上的目录契约和界面「查看」
	// 要拼的那个名字，所以两处必须用同一个常量：飘开了，查看按钮就会打开一个不存在的目录。
	DateLayout = "2006-01-02"
)

const (
	// renameTries 是占用时的改名尝试次数。
	renameTries = 8
	// conflictTries 是「目标名被人抢了」之后重挑名字的轮数。
	conflictTries = 5
)

// renameWaits 是占用重试之间的等待，一轮用完就从头循环。
//
// 250ms 那一下治的是防病毒软件的实时扫描，4s 那一下治的是资源管理器预览面板。
// 五档走一遍是 7.75 秒；八次尝试之间总共等 8.5 秒（末次失败不再等，见 renameBusy）。
// 够挡住绝大多数「马上就松手」的占用，又不至于让一个坏文件把搬运线程挂住。
var renameWaits = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// Config 是搬运要用到的那几项设置的一次快照。
//
// 快照由调用方给（Mover 不持有设置）：面板随时可能改转移目录，
// 而一次搬运中途换目录是说不清的行为 —— 所以每次 Move 开头取一次，整次都用它。
type Config struct {
	SourceDir   string
	TargetDir   string
	DateSubdirs bool
}

func (c Config) usable() error {
	if c.TargetDir == "" {
		return apperr.New(apperr.ENotConfigured, "还没有设置转移目录，先在面板里配好。")
	}
	if !filepath.IsAbs(c.TargetDir) {
		return apperr.New(apperr.EInvalidArg, "转移目录必须是绝对路径。")
	}
	return nil
}

// Book 是搬运对磁盘的记账要求。真实的 *journal.Journal 正好满足它；
// 做成接口只为了单测里能检查「账本里到底出现了几行、都是什么状态」。
type Book interface {
	Append(journal.Record) error
	LogMove(journal.Move) error
}

// Volumer 回答「这个路径落在哪块存储上」。
//
// 这是 mover 唯一需要问的 OS 事实，也是单测里唯一需要作假的：
// 「跨卷」在一台只有一块盘的机器上造不出真现场，而跨卷那条路恰恰最需要被跑到。
type Volumer interface {
	VolumeID(path string) (uint64, error)
}

type realVolume struct{}

func (realVolume) VolumeID(p string) (uint64, error) { return volumeID(p) }

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options 是装配入口。生产只需要 Settings、Book、Logger 三项，剩下的全是测试缝。
type Options struct {
	// Settings 每次搬运被调用一次，返回当时的设置快照。
	Settings func() Config
	Book     Book
	Logger   *slog.Logger

	Clock  Clock
	Sleep  func(time.Duration)
	Volume Volumer
	// Copy 默认 io.Copy；测试用它造出「只拷了一半」。
	Copy func(dst io.Writer, src io.Reader) (int64, error)
	// Rename 默认 renameNoClobber；测试用它造出占用与撞名。
	Rename func(src, dst string) error
	// Remove 默认 os.Remove；测试用它造出「目标已就位、源还被占用」。
	Remove func(string) error
}

// Mover 一次一次地搬文件，自己不管并发：一次 Move 只处理一个文件，
// 谁来决定搬哪个、什么时候重试由调用方（状态机 + watch 的那一条 goroutine）负责。
type Mover struct {
	settings func() Config
	book     Book
	log      *slog.Logger
	clk      Clock
	sleep    func(time.Duration)
	vols     Volumer
	copy     func(io.Writer, io.Reader) (int64, error)
	rename   func(string, string) error
	remove   func(string) error
}

func New(o Options) (*Mover, error) {
	if o.Settings == nil {
		return nil, errors.New("mover: 没有设置来源")
	}
	if o.Book == nil {
		return nil, errors.New("mover: 没有账本")
	}
	m := &Mover{
		settings: o.Settings,
		book:     o.Book,
		log:      o.Logger,
		clk:      o.Clock,
		sleep:    o.Sleep,
		vols:     o.Volume,
		copy:     o.Copy,
		rename:   o.Rename,
		remove:   o.Remove,
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.clk == nil {
		m.clk = realClock{}
	}
	if m.sleep == nil {
		m.sleep = time.Sleep
	}
	if m.vols == nil {
		m.vols = realVolume{}
	}
	if m.copy == nil {
		m.copy = io.Copy
	}
	if m.rename == nil {
		m.rename = renameNoClobber
	}
	if m.remove == nil {
		m.remove = os.Remove
	}
	return m, nil
}

// Result 是一次搬运的读数。
type Result struct {
	Src    string
	Dst    string
	Size   int64
	Method string
	// Dated 是实际用到的日期子目录名，没开分目录时为空。
	Dated string
	// Tries 是占用重试里等过的次数，冲突是「目标名被抢后重挑了几轮」。
	Tries     int
	Conflicts int
	MS        int64
	// PendingDelete 为 true 表示文件已经安全落在目标，但源还欠一次补删。
	// 账本里已经有 pendingDelete 那一行，所以这不是失败，是一笔欠条。
	PendingDelete bool
}

// job 是一次搬运的全部现场：一条会随进展更新的账本记录，加上几个读数。
//
// 记录本身就是计划（Dst 是打算落地的名字、Tmp 是中间名、Method 是用哪条路），
// 所以推进搬运 = 改这几个字段再追加一行账；崩溃后重放看到的是同一份东西。
type job struct {
	rec    journal.Record
	sample detect.Sample

	// tries 是占用重试里真正等过几轮，conflicts 是目标名被抢后重挑了几次。
	// 两个都只是读数：界面和日志要能说出「这次搬得不顺」。
	tries     int
	conflicts int

	// dstSize 是「已经在目标上核对过的字节数」，目标就位之前一直是 0。
	// 它要进账本当删源的许可，所以绝不拿源的数字代替 —— 改名那条路是例外，
	// 改名不动字节，落地后照抄源大小是成立的。
	dstSize int64
}

// stamped 拿当前的计划抄一份要落账的记录：时间戳、状态、一句进日志的原因。
//
// DstSize 只在目标已经就位时才填，因为账本里那个字段的含义是
// 「我们核对过目标有这些字节」，重放时它是删源的唯一许可。
func (m *Mover) stamped(j *job, state journal.State, reason string) journal.Record {
	r := j.rec
	r.TS = m.clk.Now()
	r.State = state
	r.Reason = reason
	r.Tries = j.tries
	switch state {
	case journal.StateCopied, journal.StatePendingDelete, journal.StateDone:
		r.DstSize = j.dstSize
	default:
		r.DstSize = 0
	}
	return r
}

// Move 把 src 搬到配置好的目标里，成功返回落地路径。
//
// 返回的 error 一律是 *apperr.Error，Msg 可以直接上界面。
// 失败时源文件一定还在原地（这是全包最重要的一条性质，mover_test 逐条钉住）。
func (m *Mover) Move(src string) (Result, error) {
	start := m.clk.Now()

	cfg := m.settings()
	if err := cfg.usable(); err != nil {
		return Result{}, err
	}
	src = filepath.Clean(src)
	if err := guard(src, cfg); err != nil {
		return Result{}, err
	}

	// 身份一律走 detect 的采样器：账本里记的必须和重放时问出来的是同一个数，
	// 两处各用一套算法会让 SameFile 永远判不相等。
	s, err := detect.StatSample(src)
	if err != nil {
		return Result{}, fileOp("读不到那个文件。", err)
	}
	if err := refuse(s); err != nil {
		return Result{}, err
	}

	dir, dated, err := m.destDir(cfg, s.Mtime)
	if err != nil {
		return Result{}, err
	}
	final, err := pickName(dir, filepath.Base(src))
	if err != nil {
		return Result{}, apperr.Wrap(apperr.EFileOpFailed, "在转移目录里挪不出一个空着的文件名。", err)
	}

	j := &job{
		sample: s,
		rec: journal.Record{
			ID:       journal.NewID(src, s.Size, s.Mtime, s.Index),
			TS:       m.clk.Now(),
			State:    journal.StateClaimed,
			Src:      src,
			SrcSize:  s.Size,
			SrcMtime: s.Mtime,
			SrcIndex: s.Index,
			Dst:      final,
			Method:   m.pickMethod(src, dir),
		},
	}
	// 中间名只有复制路才有。改名本身就是原子的，多一跳不增加任何东西，
	// 却会造出「源已消失、目标未就位、只剩中间名」这个现场 —— 重放表对它的判据是删掉中间名，
	// 而那时中间名就是用户文件的唯一副本。
	if j.rec.Method == MethodCopy {
		j.rec.Tmp = tmpName(final, j.rec.ID)
	}

	// 先记账，再动字节。这一步失败就一个字节都不许动 ——
	// 没记账的搬运在崩溃之后是查不到的，而「查不到」意味着重放时既不敢删也不敢留。
	if err := m.book.Append(j.rec); err != nil {
		return Result{}, apperr.Wrap(apperr.EFileOpFailed, "搬运前记不上账，所以一个字都没动。", err)
	}

	pending, err := m.run(j)
	res := Result{
		Src: j.rec.Src, Dst: j.rec.Dst, Size: s.Size, Method: j.rec.Method,
		Dated: dated, Tries: j.tries, Conflicts: j.conflicts,
		MS: m.clk.Now().Sub(start).Milliseconds(),
	}
	if err != nil {
		m.log.Warn("搬运没做成", "src", j.rec.Src, "err", err)
		return res, err
	}
	res.PendingDelete = pending

	if !pending {
		// 欠条那行账（pendingDelete）已经由复制路记上了，这里只管结清的那一笔。
		if err := m.book.Append(m.stamped(j, journal.StateDone, "")); err != nil {
			// 东西已经稳稳落在目标了，只是结不了账 —— 这不是搬运失败，
			// 报失败反而会让状态机去重试一个已经不存在的源文件。
			m.log.Error("搬完了但记不上 done", "dst", j.rec.Dst, "err", err)
		}
	}

	if err := m.book.LogMove(journal.Move{
		TS: m.clk.Now(), Src: j.rec.Src, Dst: j.rec.Dst, Size: s.Size,
		Method: j.rec.Method, Ms: res.MS, Outcome: journal.OutcomeMoved,
		Reason: debtNote(pending),
	}); err != nil {
		m.log.Error("流水记不上", "dst", j.rec.Dst, "err", err)
	}
	return res, nil
}

// run 把文件送上岗，然后把源处理干净。返回「是否欠一次补删」。
func (m *Mover) run(j *job) (bool, error) {
	if j.rec.Method == MethodRename {
		landed, err := m.byRename(j)
		if err != nil {
			return false, err
		}
		if landed {
			return false, nil
		}
		// landed=false 只有一个意思：MoveFileEx 说这两个路径不在同一个卷上。
		// 卷号判错时一个字节都还没动过，所以换路是免费的。
		j.rec.Method = MethodCopy
		j.rec.Tmp = tmpName(j.rec.Dst, j.rec.ID)
		if err := m.book.Append(m.stamped(j, journal.StateClaimed, "卷号说是同卷、改名说不是，改用复制")); err != nil {
			return false, apperr.Wrap(apperr.EFileOpFailed, "改用复制前记不上账。", err)
		}
	}
	return m.byCopy(j)
}

// byRename 是同卷那条路：一次改名，零字节拷贝。
//
// 返回 false 只表示「跨卷，换路」。这里绝不出现「改名失败但目标名上有了半个文件」：
// renameNoClobber 不开 REPLACE_EXISTING，撞名与跨卷都是原地不动。
func (m *Mover) byRename(j *job) (bool, error) {
	for c := 0; c < conflictTries; c++ {
		err := m.renameBusy(j, j.rec.Src, j.rec.Dst)
		switch {
		case err == nil:
			// 改名不动字节，所以落地大小照抄源是成立的。
			j.dstSize = j.sample.Size
			return true, nil
		case isCrossDevice(err):
			return false, nil
		case isAlreadyThere(err):
			// 挑完名字到真正改名之间有人把那个名字用了（另一个下载正好完成最常见）。
			// 重挑一个号，绝不覆盖 —— 覆盖是不可恢复的数据丢失。
			j.conflicts++
			alt, perr := pickName(filepath.Dir(j.rec.Dst), filepath.Base(j.rec.Src))
			if perr != nil {
				return false, apperr.Wrap(apperr.EFileOpFailed, "在转移目录里挪不出一个空着的文件名。", perr)
			}
			j.rec.Dst = alt
			if aerr := m.book.Append(m.stamped(j, journal.StateClaimed, "目标名被抢，换个名字重试")); aerr != nil {
				return false, apperr.Wrap(apperr.EFileOpFailed, "换名字前记不上账。", aerr)
			}
			continue
		default:
			return false, fileOp("搬不动那个文件。", err)
		}
	}
	return false, apperr.New(apperr.EFileOpFailed, "转移目录里撞名太多次，先停下。")
}

// renameBusy 带上占用重试，两段改名（源→最终名、副本→最终名）共用这一份预算。
//
// 设计书里写的「之后降为 5 秒轮询」刻意不在这段代码里做：一个永远被占用的文件
// 要是把搬运线程挂在 while 里，后面的下载就全都排不上队。这里 8.5 秒预算用完就如实报错，
// 由状态机退回观察、按自己的节拍再看一眼 —— 那个节拍本身就是 5 秒级的轮询，
// 而且有 MaxSettle 兜底，坏文件最终变成 Stuck 而不是一直占着队伍。
//
// 非占用的错误（撞名、跨卷）第一时间返回：那些不是「再等一下就好」的事。
func (m *Mover) renameBusy(j *job, from, to string) error {
	var err error
	for i := 0; i < renameTries; i++ {
		err = m.rename(from, to)
		if err == nil || !isBusy(err) {
			j.tries = i
			return err
		}
		if i == renameTries-1 {
			break
		}
		m.sleep(renameWaits[i%len(renameWaits)])
	}
	j.tries = renameTries - 1
	return err
}

// destDir 定下这一单落哪儿，顺手把目录建出来。
//
// 日期取**源文件的 mtime**而不是现在：一次搬运常常发生在下载完成的当天，
// 但「把上周下好的东西补搬一遍」时，用户期望它在下载那天的目录里。
func (m *Mover) destDir(cfg Config, mtime time.Time) (dir, dated string, err error) {
	dir = filepath.Clean(cfg.TargetDir)
	if cfg.DateSubdirs {
		dated = mtime.Format(DateLayout)
		dir = filepath.Join(dir, dated)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", apperr.Wrap(apperr.EFileOpFailed, "建不出转移目录。", err)
	}
	return dir, dated, nil
}

// pickMethod 决定用改名还是复制。
//
// 判据是两边的卷号相同。拿不准就用复制：复制那条路会校验落地字节数、校验过了才删源，
// 最坏只是慢。反过来把跨卷误判成同卷，代价是改名被 OS 拒绝、再回头换路（这条兜底在 run 里）。
func (m *Mover) pickMethod(src, dstDir string) string {
	a, err := m.vols.VolumeID(src)
	if err != nil {
		m.log.Debug("问不到源的卷号，改用复制", "src", src, "err", err)
		return MethodCopy
	}
	b, err := m.vols.VolumeID(dstDir)
	if err != nil {
		m.log.Debug("问不到目标的卷号，改用复制", "dst", dstDir, "err", err)
		return MethodCopy
	}
	// 序列号为 0 是这个文件系统不带卷号（exFAT、UFD 盘、部分网络盘），
	// 「两个 0」相等不能当成同卷证据。
	if a == 0 || b == 0 || a != b {
		return MethodCopy
	}
	return MethodRename
}

// guard 是动手之前的路径级否决：能在这里挡住的，就别留给 OS 去报错。
func guard(src string, cfg Config) error {
	if src == "" {
		return apperr.New(apperr.EInvalidArg, "没有要搬的文件。")
	}
	switch {
	case detect.IsDenied(src):
		return forbidden(src, "这类文件永远不参与搬运")
	case detect.IsTempPath(src):
		// 我们自己残留的 .download2.tmp 也以 .tmp 结尾，所以同一条规则既挡住下载器的中间名，
		// 也挡住我们的中间名 —— 不需要第二套清单。
		return forbidden(src, "这是还没下完的中间名")
	}
	if within(src, cfg.TargetDir) {
		return forbidden(src, "它已经在转移目录里了，不再搬一次")
	}
	// 目标落在监控目录里面会形成自搬环：搬过去的文件被再看见、再搬一次。
	// config 的校验早就拦这一条，这里是最后一道兜底。
	if within(cfg.TargetDir, cfg.SourceDir) {
		return apperr.New(apperr.EPathForbidden, "转移目录不能在监控目录里面，否则刚搬过去的文件会被再搬一次。")
	}
	return nil
}

// refuse 否决不适合搬的文件属性。名字挡不住的东西（目录、隐藏文件）由采样挡住。
func refuse(s detect.Sample) error {
	switch {
	case s.IsDir:
		return apperr.New(apperr.EPathForbidden, "目录不搬：别人拷一整个文件夹进来时，我们只该看到一个条目。")
	case s.Hidden:
		return apperr.New(apperr.EPathForbidden, "隐藏文件不搬。")
	case s.Size <= 0:
		return apperr.New(apperr.EInvalidArg, "0 字节的文件不搬：宁可盯住它，也不搬一个空壳。")
	}
	return nil
}

func forbidden(p, why string) error {
	return apperr.New(apperr.EPathForbidden, fmt.Sprintf("%s 不用搬：%s。", filepath.Base(p), why)).
		With("name", filepath.Base(p))
}

// fileOp 把 OS 的原文翻成一句能上界面的中文；原文只进日志。
func fileOp(msg string, err error) *apperr.Error {
	if isBusy(err) {
		return apperr.Wrap(apperr.EFileOpFailed, "文件还被别的程序占用，现在搬不动。", err)
	}
	return apperr.Wrap(apperr.EFileOpFailed, msg, err)
}

func debtNote(pending bool) string {
	if pending {
		return "已搬到目标，源文件还被占用，稍后补删"
	}
	return ""
}

// tmpName 是最终名的中间形态。
//
// 默认就是设计书里的 final + ".download2.tmp"。只有那个名字已经被别的东西占了才多挂一层记录 ID：
// 我们只删自己造的东西，而撞名时「换个名字」比「删掉一个陌生文件」便宜得多。
// 两种形态都以 TmpSuffix 结尾，所以账本的 HasOwnTmp 判据对它们同样成立。
func tmpName(final, id string) string {
	p := final + journal.TmpSuffix
	if pathFree(p) {
		return p
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return final + "." + short + journal.TmpSuffix
}

// within 判断 child 是不是落在 parent 里面（含相等）。
//
// 两边都先 Clean，再用平台的文件名比较规矩：Windows 不区分大小写，
// 而「同一个目录写成两种大小写」在真实配置里很常见 —— 判成两个目录就会自搬一次。
func within(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	c := filepath.Clean(child)
	p := filepath.Clean(parent)
	for {
		if eqPath(c, p) {
			return true
		}
		d := filepath.Dir(c)
		if d == c {
			return false
		}
		c = d
	}
}

func eqPath(a, b string) bool {
	if caseInsensitive() {
		return strings.EqualFold(a, b)
	}
	return a == b
}
