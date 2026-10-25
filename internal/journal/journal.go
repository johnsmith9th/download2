// Package journal 是崩溃账本：pending.jsonl 记「我打算对这个文件做什么、做到哪一步了」，
// moves.jsonl 记「搬完了，从哪儿到哪儿」。
//
// 为什么要有这个包：搬运是多步操作，任何一步之间都可能被断电、被杀进程、被系统更新重启掉。
// 只有先把意图写进磁盘，重启后才知道哪些文件处于半完成状态——否则要么把半成品当完成搬走，
// 要么让一个已经复制过去、还没删源的文件永久失踪（用户看到的就是「文件少了」或「文件多了」）。
//
// 包本身不碰 src/dst 的字节，也不 walk 目录：它只记账，外加提供一个纯函数 Plan，
// 把「账本 + 现场观察」翻成一组该执行的动作。副作用留给调用方，这样重放表能被完全表驱动测。
package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State 是账本里一条搬运记录走到哪一步。
//
// 这些值是磁盘上的字符串，改了就和老账本对不上——老账本读不出来等于崩溃后失忆，
// 所以只加不改。
type State string

const (
	// StateClaimed：判定完成、已记账，还没动任何字节。
	StateClaimed State = "claimed"
	// StateMoving：同卷改名路——已发布到 .download2.tmp，等着换上最终名。
	StateMoving State = "moving"
	// StateCopied：跨卷复制路——目标已就位并校验过字节数，源还没删。
	StateCopied State = "copied"
	// StatePendingDelete：目标已上岗，删源时被占用挡住了，欠一次补删。
	StatePendingDelete State = "pendingDelete"
	// StateDone：全绿收工。
	StateDone State = "done"
	// StateAbandoned：说不清状态，东西留在原地，只记日志。永不盲删。
	StateAbandoned State = "abandoned"
)

// Resolved 为 true 的状态不会再被重放，7 天后从 pending 里清走。
func (s State) Resolved() bool {
	return s == StateDone || s == StateAbandoned
}

// TmpSuffix 是我们自己造的中间文件名后缀。
//
// 常量放在这个包，因为重放时要靠它做安全判据：账本里的 tmp 字段必须以它结尾，
// 否则那多半是别人家的临时文件，我们没资格删。mover 建临时名时也从这里取，两处共用一条口径。
//
// 它刻意以 ".tmp" 收尾：判定侧的临时名清单里本来就有 .tmp，于是同一份清单
// 既挡住下载器的中间名，也挡住我们自己残留的中间名 —— 不需要第二条清单。
// （早先是 ".dlguard-tmp"，那个形态躲得过 .tmp 这条判据，残留会被当成一次正常下载再搬一遍。）
const TmpSuffix = ".download2.tmp"

// legacyTmpSuffix 是老产品名 DLGuard 时期写进磁盘的那个后缀。
//
// 改名只换今天新建的中间名，换不掉已经躺在下载目录里的残留和已经落账的 tmp 字段。
// 判据要是只认新后缀，这些老残留就成了「别人家的临时文件」：重放不敢删，
// 而它们明明以 .tmp 结尾，于是会被判定侧当成一次正常下载再搬一遍 —— 正是要防的那件事。
const legacyTmpSuffix = ".dlguard.tmp"

// HasOwnTmpSuffix 判断一个名字是不是我们自己造的中间名（认新旧两个后缀）。
//
// 长度用严格大于而不是大于等于：名字整个等于后缀时主名是空的，那不是我们的产物。
func HasOwnTmpSuffix(name string) bool {
	for _, s := range []string{TmpSuffix, legacyTmpSuffix} {
		if len(name) > len(s) && name[len(name)-len(s):] == s {
			return true
		}
	}
	return false
}

// Record 是一行 JSON 的形态。字段名就是磁盘契约，改一个就要动老账本。
type Record struct {
	ID    string    `json:"id"`
	TS    time.Time `json:"ts"`
	State State     `json:"state"`

	Src      string    `json:"src"`
	SrcSize  int64     `json:"srcSize"`
	SrcMtime time.Time `json:"srcMtime"`
	// SrcIndex 是文件的身份证（NTFS 的 FileId，取不到时退化成 Inode/ino）。
	// 重放时拿它确认「这个路径上还是当初那个文件」，而不是同名重建的另一个。
	SrcIndex uint64 `json:"srcIndex"`

	Tmp     string `json:"tmp,omitempty"`
	Dst     string `json:"dst,omitempty"`
	DstSize int64  `json:"dstSize,omitempty"`

	// Method 是这次搬运打算用的手段：rename（同卷，两次改名）或 copy（跨卷/UNC）。
	// 记下来是因为压缩时它要变成流水里的一行，而人看到 copy 就该预期慢。
	Method string `json:"method,omitempty"`

	Tries  int    `json:"tries,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// NewID 由「源路径 + 大小 + mtime + 文件索引」算出，不用随机数：
// 同一个文件被重新观察到时得到同一个 ID，账本因此不会给一次搬运记两笔。
func NewID(src string, size int64, mtime time.Time, index uint64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n%d\n%d", src, size, mtime.UnixNano(), index)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SameFile 判断磁盘上的现状是否还是记账时那个文件。
//
// 只比路径是不够的：下载器可以在我们 claim 之后删掉再建同名文件，
// 那时 size/mtime/索引至少有一个会变，而我们绝不能按老账本去动一个新文件。
func (r Record) SameFile(size int64, mtime time.Time, index uint64) bool {
	return r.SrcSize == size && r.SrcIndex == index && r.SrcMtime.Equal(mtime)
}

// HasOwnTmp 是删临时文件前的唯一许可判据。
func (r Record) HasOwnTmp() bool { return HasOwnTmpSuffix(r.Tmp) }

// Observer 由调用方实现：账本包自己不看文件系统，重放表因此能被纯函数测。
// 实现要给出「这个路径还在不在、多大、哪个文件」，而不是让本包去猜。
type Observer interface {
	Observe(Record) (View, error)
}

// Journal 是 %APPDATA%\download2 下两个文件的门面。
type Journal struct {
	dir     string
	pending pendingFile
	moves   movesLog
}

func New(dir string) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("journal: 数据目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: 建不出数据目录: %w", err)
	}
	return &Journal{
		dir:     dir,
		pending: pendingFile{path: filepath.Join(dir, "pending.jsonl")},
		moves:   movesLog{path: filepath.Join(dir, "moves.jsonl")},
	}, nil
}

func (j *Journal) Dir() string { return j.dir }

// MovesPath 是搬运流水在磁盘上的位置。「查看日志」那颗按钮要的就是这个路径，
// 所以它由 Journal 报出来，而不是让调用方去拼 filepath.Join(dir, "moves.jsonl") ——
// 名字在这儿改过一次（早先是 app.log 的说法还留在设计稿里），拼第二遍就会指向一个不存在的文件。
func (j *Journal) MovesPath() string { return j.moves.path }

// Append 落一条账本记录。**必须在动任何字节之前调用成功**：
// 记账失败还继续搬，崩溃后就没人记得我们动过这个文件。
func (j *Journal) Append(r Record) error { return j.pending.append(r) }

// Pending 读出当前状态表。第二个返回值是被跳过、读不懂的行数，调用方要把它写进日志——
// 静默吞掉账本行等于对崩溃历史撒谎。
func (j *Journal) Pending() ([]Record, int, error) { return j.pending.load() }

// Replay 是启动时的第一步：先重放，再建目录基线。
func (j *Journal) Replay(o Observer) ([]Action, int, error) {
	recs, skipped, err := j.pending.load()
	if err != nil {
		return nil, skipped, err
	}
	actions, err := PlanAll(recs, o.Observe)
	return actions, skipped, err
}

// LogMove 落一行人类可读的流水。
func (j *Journal) LogMove(ev Move) error { return j.moves.append(ev) }

// ClearMoves 把流水清成空。**pending.jsonl 一个字节都不许动**：那是崩溃账本，
// 清掉它等于让下一次启动对上一次崩溃失忆——半搬完的文件会被当成一次新下载再搬一遍。
//
// 就地截断而不是删文件：截完路径还在，「查看日志」下一按就有一个空文件可开；
// 而删掉会把「还一次都没搬过」和「刚被清空」压成同一种表现。
// 流水是每行开一次、写完就关，所以这里没有本进程的活句柄要顾虑。
func (j *Journal) ClearMoves() error {
	if err := os.Truncate(j.moves.path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 一行都还没写过：清空一份空流水不是故障
		}
		return fmt.Errorf("journal: 清不掉流水: %w", err)
	}
	return nil
}

// Compact 把解决完且已超过 keepFor 的记录从 pending 挪走。
//
// 为什么必须做：pending 是启动时逐条读、逐条 stat 的，让它无限增长等于让每次启动越来越慢。
// abandoned 的那批在挪走前先补一行流水——那是它唯一一次被人类读到的机会。
func (j *Journal) Compact(now time.Time) (int, error) {
	recs, _, err := j.pending.load()
	if err != nil {
		return 0, err
	}

	keep := make([]Record, 0, len(recs))
	var dropped int
	for _, r := range recs {
		if !r.State.Resolved() || now.Sub(r.TS) <= keepFor {
			keep = append(keep, r)
			continue
		}
		// 先进流水再删账：顺序反过来，中途崩溃就得到一条账本里没有、流水里也没有的记录。
		if r.State == StateAbandoned {
			if err := j.moves.append(Move{TS: r.TS, Src: r.Src, Dst: r.Dst, Size: r.SrcSize,
				Method: r.Method, Outcome: OutcomeAbandoned, Reason: r.Reason}); err != nil {
				return dropped, err
			}
		}
		dropped++
	}
	if dropped == 0 {
		return 0, nil
	}
	if err := j.pending.rewrite(keep); err != nil {
		return dropped, err
	}
	return dropped, nil
}
