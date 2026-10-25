package journal

import (
	"fmt"
	"time"
)

// View 是重放一条记录之前对现场的全部观察。
//
// 它是个结构体而不是接口，是因为观察这件事只有一种做法（stat 三个路径），
// 而把它做成数据让整张重放表能被纯函数式地测试——不需要真造文件。
// 采集由调用方负责：账本包不碰文件系统，也就不需要任何替身。
type View struct {
	SrcExists bool
	SrcSize   int64
	SrcMtime  time.Time
	SrcIndex  uint64

	// TmpExists 是对 Record.Tmp 那个路径的观察；Record.Tmp 为空时调用方应报 false。
	TmpExists bool

	DstExists bool
	DstSize   int64
}

// ActionKind 是重放决定要做的事。每一种都只对应一个副作用，方便执行侧逐条实现。
type ActionKind string

const (
	// ActionNone：这条记录已经结清，不需要任何动作。
	ActionNone ActionKind = "none"
	// ActionRewatch：回到 Watching，什么都不删，等文件重新静置。
	ActionRewatch ActionKind = "rewatch"
	// ActionCleanTmp：删掉我们自己的 .download2.tmp。
	// 删完之后要不要回 Watching 由现场决定：源还在就重新观察，源没了就直接销账。
	ActionCleanTmp ActionKind = "cleanTmp"
	// ActionFinishDelete：删源并记 done。这是全表里唯一会删用户文件的一支。
	ActionFinishDelete ActionKind = "finishDelete"
	// ActionAdopt：源不在、目标在 —— 上次其实已经搬成，只是没来得及记 done；补记，不动字节。
	ActionAdopt ActionKind = "adopt"
	// ActionAbandon：说不清状态。只记日志，一个字节都不碰。
	ActionAbandon ActionKind = "abandon"
	// ActionForget：两边都不在（用户在别处删了下载），从账本里去掉。
	ActionForget ActionKind = "forget"
)

// Action 带上了它来自哪条记录，执行侧才能在做完后写回对应的新状态。
type Action struct {
	Kind   ActionKind
	Record Record
	Reason string
}

// Plan 是崩溃重放的判据表。
//
// 三条贯穿全表的原则：
//  1. 拿不准就放弃。误删一个用户文件的代价远高于少搬一个文件。
//  2. 只有目标已就位**且字节数和账本一致**时才删源（ActionFinishDelete 是全表唯一的删源分支）。
//  3. 只删我们自己造的临时文件（后缀必须过 HasOwnTmp，新旧产品名的两个后缀都算）。
func Plan(r Record, v View) Action {
	if r.State.Resolved() {
		return Action{Kind: ActionNone, Record: r, Reason: "已结清"}
	}

	// 先验身份，再谈状态：路径还在不代表文件还是那个。
	// 下载器可以在 claim 之后删掉重建同名文件，按老账本动它会毁掉一个我们毫无所知的新文件。
	if v.SrcExists && !r.SameFile(v.SrcSize, v.SrcMtime, v.SrcIndex) {
		return Action{Kind: ActionAbandon, Record: r, Reason: "源路径上已经不是记账时那个文件"}
	}

	switch {
	case !v.SrcExists && !v.DstExists:
		if v.TmpExists {
			return Action{Kind: ActionCleanTmp, Record: r, Reason: "源与目标都不在了，只剩我们的临时文件"}
		}
		return Action{Kind: ActionForget, Record: r, Reason: "源与目标都不在了，用户在别处删了"}

	case !v.SrcExists && v.DstExists:
		if v.DstSize <= 0 {
			return Action{Kind: ActionAbandon, Record: r, Reason: "目标存在但是 0 字节，说不清"}
		}
		return Action{Kind: ActionAdopt, Record: r, Reason: "源已消失、目标就位，上次其实搬成功了"}

	case v.SrcExists && v.DstExists:
		switch r.State {
		case StateCopied, StatePendingDelete:
			// 只有字节数完全一致才敢删源：跨卷复制中途被打断时，目标是个半成品，
			// 这时删源就是把用户的文件删掉一半。
			if v.DstSize == r.SrcSize && r.SrcSize > 0 {
				return Action{Kind: ActionFinishDelete, Record: r, Reason: "目标字节数已核对一致，补删源"}
			}
			return Action{Kind: ActionAbandon, Record: r, Reason: fmt.Sprintf("目标 %d 字节、账本 %d 字节，对不上就绝不删源", v.DstSize, r.SrcSize)}
		default:
			// claimed/moving 阶段目标名上就该没有文件；有，说明别人已经占了那个名字。
			// 覆盖它是不可恢复的数据丢失，所以两边都留着，交给人判断。
			return Action{Kind: ActionAbandon, Record: r, Reason: "目标名上已经有别的文件"}
		}

	default: // 源在、目标不在
		if v.TmpExists {
			if !r.HasOwnTmp() {
				return Action{Kind: ActionAbandon, Record: r, Reason: "tmp 字段不像我们自己造的文件，没资格删"}
			}
			return Action{Kind: ActionCleanTmp, Record: r, Reason: "清掉上次没处理完的临时文件，重新静置观察"}
		}
		return Action{Kind: ActionRewatch, Record: r, Reason: "源还在、目标未就位，回到静置观察"}
	}
}

// PlanAll 逐条重放。viewFn 报错时不整体失败，而是把那条记成 Abandon：
// 转移目录在网络盘上而网络盘还没起来，是最常见的启动场景之一，
// 这时把程序判死等于让用户以为下载目录被搬空了。
func PlanAll(records []Record, viewFn func(Record) (View, error)) ([]Action, error) {
	out := make([]Action, 0, len(records))
	for _, r := range records {
		v, err := viewFn(r)
		if err != nil {
			out = append(out, Action{Kind: ActionAbandon, Record: r, Reason: fmt.Sprintf("看不了现场: %v", err)})
			continue
		}
		out = append(out, Plan(r, v))
	}
	return out, nil
}
