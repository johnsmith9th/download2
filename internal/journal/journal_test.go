package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func rec(state State, mut func(*Record)) Record {
	r := Record{
		ID:       "id-1",
		TS:       t0,
		State:    state,
		Src:      `C:\Users\me\Downloads\a.zip`,
		SrcSize:  1000,
		SrcMtime: t0.Add(-time.Minute),
		SrcIndex: 4242,
		Dst:      `D:\归档\2026-10-07\a.zip`,
		Method:   "rename",
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

// view 造一个「和账本完全对得上」的现场，用例只改它要变的那一项。
func view(r Record) View {
	return View{
		SrcExists: true, SrcSize: r.SrcSize, SrcMtime: r.SrcMtime, SrcIndex: r.SrcIndex,
		DstExists: r.Dst != "", DstSize: r.SrcSize,
	}
}

// —— 重放表：这是崩溃恢复的判据本身，逐条钉死 ——

func TestReplayTable(t *testing.T) {
	cases := []struct {
		name  string
		rec   Record
		view  func(Record) View
		want  ActionKind
		saidW string // Reason 里必须出现的词
	}{
		{
			name: "已结清的不再处理",
			rec:  rec(StateDone, nil),
			view: view,
			want: ActionNone,
		},
		{
			name: "abandoned 也不再处理",
			rec:  rec(StateAbandoned, nil),
			view: view,
			want: ActionNone,
		},
		{
			name: "claimed 且目标还没就位 → 重新观察",
			rec:  rec(StateClaimed, func(r *Record) { r.Dst = "" }),
			view: func(r Record) View { v := view(r); v.DstExists = false; return v },
			want: ActionRewatch,
		},
		{
			name: "moving 且 tmp 是我们的 → 先清 tmp",
			rec:  rec(StateMoving, func(r *Record) { r.Tmp = r.Dst + TmpSuffix; r.Dst = "" }),
			view: func(r Record) View { v := view(r); v.DstExists = false; v.TmpExists = true; return v },
			want: ActionCleanTmp,
		},
		{
			name:  "moving 但 tmp 名字不像我们的 → 绝不删",
			rec:   rec(StateMoving, func(r *Record) { r.Tmp = `D:\别人留的文件.xlsx`; r.Dst = "" }),
			view:  func(r Record) View { v := view(r); v.DstExists = false; v.TmpExists = true; return v },
			want:  ActionAbandon,
			saidW: "没资格删",
		},
		{
			name: "copied 且字节数一致 → 补删源",
			rec:  rec(StateCopied, nil),
			view: view,
			want: ActionFinishDelete,
		},
		{
			name: "pendingDelete 且字节数一致 → 补删源",
			rec:  rec(StatePendingDelete, nil),
			view: view,
			want: ActionFinishDelete,
		},
		{
			name:  "copied 但目标字节少了 → 保住源",
			rec:   rec(StateCopied, nil),
			view:  func(r Record) View { v := view(r); v.DstSize = r.SrcSize - 1; return v },
			want:  ActionAbandon,
			saidW: "对不上就绝不删源",
		},
		{
			name: "copied 但目标被截成 0 字节 → 保住源",
			rec:  rec(StateCopied, nil),
			view: func(r Record) View { v := view(r); v.DstSize = 0; return v },
			want: ActionAbandon,
		},
		{
			name: "账本记的源大小是 0 → 不配删源",
			rec:  rec(StateCopied, func(r *Record) { r.SrcSize = 0 }),
			view: func(r Record) View { v := view(r); v.SrcSize = 0; v.DstSize = 0; return v },
			want: ActionAbandon,
		},
		{
			name: "copied 但目标不见了 → 回观察，别删源",
			rec:  rec(StateCopied, nil),
			view: func(r Record) View { v := view(r); v.DstExists = false; return v },
			want: ActionRewatch,
		},
		{
			name:  "claimed 但目标名上已经有文件 → 两边都留着",
			rec:   rec(StateClaimed, nil),
			view:  view,
			want:  ActionAbandon,
			saidW: "已经有别的文件",
		},
		{
			name: "源没了、目标在 → 上次其实成功了",
			rec:  rec(StateMoving, func(r *Record) { r.Tmp = r.Dst + TmpSuffix }),
			view: func(r Record) View { v := view(r); v.SrcExists = false; return v },
			want: ActionAdopt,
		},
		{
			name: "源没了、目标在但 0 字节 → 说不清",
			rec:  rec(StateMoving, nil),
			view: func(r Record) View { v := view(r); v.SrcExists = false; v.DstSize = 0; return v },
			want: ActionAbandon,
		},
		{
			name: "两边都没了 → 销账",
			rec:  rec(StateClaimed, func(r *Record) { r.Dst = "" }),
			view: func(r Record) View { return View{} },
			want: ActionForget,
		},
		{
			name: "两边都没了但 tmp 还在 → 清 tmp",
			rec:  rec(StateMoving, func(r *Record) { r.Tmp = r.Dst + TmpSuffix; r.Dst = "" }),
			view: func(r Record) View { return View{TmpExists: true} },
			want: ActionCleanTmp,
		},
		{
			// 这一条是全表里最重要的一条：路径没变不等于文件没变。
			// 下载器可以在 claim 之后删掉再建同名文件，按老账本动它就毁了一个新文件。
			name:  "源路径上换成另一个文件了",
			rec:   rec(StateCopied, nil),
			view:  func(r Record) View { v := view(r); v.SrcSize = r.SrcSize + 1; return v },
			want:  ActionAbandon,
			saidW: "不是记账时那个文件",
		},
		{
			name: "源 mtime 变了也算换文件",
			rec:  rec(StateCopied, nil),
			view: func(r Record) View { v := view(r); v.SrcMtime = r.SrcMtime.Add(time.Second); return v },
			want: ActionAbandon,
		},
		{
			name: "源索引变了也算换文件",
			rec:  rec(StateCopied, nil),
			view: func(r Record) View { v := view(r); v.SrcIndex = r.SrcIndex + 1; return v },
			want: ActionAbandon,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Plan(c.rec, c.view(c.rec))
			if a.Kind != c.want {
				t.Errorf("动作 = %s, 想要 %s (%s)", a.Kind, c.want, a.Reason)
			}
			if c.saidW != "" && !strings.Contains(a.Reason, c.saidW) {
				t.Errorf("理由 %q 里没写 %q", a.Reason, c.saidW)
			}
			if a.Record.ID != c.rec.ID {
				t.Error("动作没带上它来自哪条记录，执行侧没法回写状态")
			}
		})
	}
}

// 全表唯一的删源分支必须真的只有一个——将来加分支时这条会拦住「顺手删点别的」。
// 不变式：只有 copied / pendingDelete，且目标确实在、字节数和账本完全一致，才允许 ActionFinishDelete。
func TestOnlyPathsThatMayDeleteTheSource(t *testing.T) {
	var allowed int
	for _, st := range []State{StateClaimed, StateMoving, StateCopied, StatePendingDelete} {
		r := rec(st, nil)
		for _, v := range allViews(r) {
			a := Plan(r, v)
			if a.Kind != ActionFinishDelete {
				continue
			}
			if st != StateCopied && st != StatePendingDelete {
				t.Errorf("%s 竟然走到了删源: %s", st, a.Reason)
			}
			if !v.DstExists {
				t.Errorf("目标不在却要删源: %s", a.Reason)
			}
			if v.DstSize != r.SrcSize || r.SrcSize <= 0 {
				t.Errorf("字节数没核对就要删源: dst=%d src=%d", v.DstSize, r.SrcSize)
			}
			allowed++
		}
	}
	// copied/pendingDelete 各两种现场（有没有残留 tmp）都能走到删源，仅此四种。
	if allowed != 4 {
		t.Errorf("允许删源的组合有 %d 种, 想要 4", allowed)
	}
}

func allViews(r Record) []View {
	base := view(r)
	tmp := base
	tmp.TmpExists = true
	noDst := base
	noDst.DstExists = false
	noDstSize := base
	noDstSize.DstSize = r.SrcSize - 1
	return []View{base, tmp, noDst, noDstSize, {}}
}

func TestPlanAllSurvivesBadObservations(t *testing.T) {
	// 转移目录在网络盘上而网络盘还没起来，是最常见的启动场景之一。
	// 这时把程序判死，等于让用户以为下载目录被搬空了。
	recs := []Record{rec(StateCopied, nil), rec(StateClaimed, func(r *Record) { r.ID = "id-2" })}
	boom := errors.New("网络路径不可达")
	actions, err := PlanAll(recs, func(r Record) (View, error) {
		if r.ID == "id-2" {
			return View{}, boom
		}
		return view(r), nil
	})
	if err != nil {
		t.Fatalf("看不了现场不该是致命错误: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("每条记录都要有一个决定, 得到 %d 条", len(actions))
	}
	if actions[0].Kind != ActionFinishDelete {
		t.Errorf("能看的记录被连坐了: %s", actions[0].Kind)
	}
	if actions[1].Kind != ActionAbandon {
		t.Errorf("看不见的记录 = %s, 想要 abandon（一个字节都不碰）", actions[1].Kind)
	}
	if !strings.Contains(actions[1].Reason, "不可达") {
		t.Errorf("理由里没带上原始故障: %q", actions[1].Reason)
	}
}

// —— ID 与身份判据 ——

func TestNewIDIsStableAndContentAddressed(t *testing.T) {
	a := NewID(`C:\a.zip`, 10, t0, 7)
	b := NewID(`C:\a.zip`, 10, t0, 7)
	if a != b {
		t.Errorf("同一个文件两次算出 %q 和 %q，账本会记两笔", a, b)
	}
	if len(a) != 16 {
		t.Errorf("ID 长度 = %d, 想要 16", len(a))
	}
	// 任一项变了都该是另一件事：名字相同但内容不同，绝不能沿用老账本。
	for _, other := range []string{
		NewID(`C:\a.zip`, 11, t0, 7),
		NewID(`C:\a.zip`, 10, t0.Add(time.Second), 7),
		NewID(`C:\a.zip`, 10, t0, 8),
		NewID(`C:\b\a.zip`, 10, t0, 7),
	} {
		if other == a {
			t.Error("输入变了 ID 却没变")
		}
	}
}

func TestHasOwnTmp(t *testing.T) {
	for _, good := range []string{
		`D:\a.zip` + TmpSuffix,
		`x` + TmpSuffix,
		`D:\a.zip` + legacyTmpSuffix, // 改名前落的账，重放时照样得有资格删
		`x` + legacyTmpSuffix,
	} {
		if !(Record{Tmp: good}).HasOwnTmp() {
			t.Errorf("%q 是我们造的临时名，却没被认出来", good)
		}
	}
	for _, bad := range []string{``, `a.zip.crdownload`, TmpSuffix, legacyTmpSuffix, `a.zip.tmp`, `下载中的文件.part`} {
		if (Record{Tmp: bad}).HasOwnTmp() {
			t.Errorf("%q 不是我们的完整临时名，却通过了检查", bad)
		}
	}
}

// —— pending.jsonl 的读写 ——

func openPending(t *testing.T) (pendingFile, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.jsonl")
	return pendingFile{path: path}, path
}

func TestPendingAppendLoadRoundTrip(t *testing.T) {
	f, path := openPending(t)

	if err := f.append(rec(StateClaimed, nil)); err != nil {
		t.Fatal(err)
	}
	if err := f.append(rec(StateMoving, func(r *Record) { r.Tmp = r.Dst + TmpSuffix })); err != nil {
		t.Fatal(err)
	}

	recs, skipped, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("跳过了 %d 行", skipped)
	}
	// 同一个 ID 的多行是同一次搬运的进度，必须归并成最新那一条。
	if len(recs) != 1 {
		t.Fatalf("两条同 ID 记录读回 %d 条", len(recs))
	}
	if recs[0].State != StateMoving {
		t.Errorf("状态 = %s, 想要最新那条 moving", recs[0].State)
	}
	if !recs[0].HasOwnTmp() {
		t.Error("tmp 字段没读回来，重放时就无法安全清理")
	}

	// 每行必须完整落盘并带换行：没有换行，下一次追加会把两条账粘成一条废账。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("最后一行没有以换行结尾")
	}
	if got := strings.Count(string(raw), "\n"); got != 2 {
		t.Errorf("文件里 %d 行, 想要 2 行", got)
	}
}

func TestPendingRejectsIncompleteRecords(t *testing.T) {
	f, _ := openPending(t)

	for name, mut := range map[string]func(*Record){
		"没 ID": func(r *Record) { r.ID = "" },
		"没时间":  func(r *Record) { r.TS = time.Time{} },
		"没源":   func(r *Record) { r.Src = "" },
	} {
		r := rec(StateClaimed, mut)
		if err := f.append(r); err == nil {
			t.Errorf("%s 的记录竟然写进去了", name)
		}
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f, _ := openPending(t)
	recs, skipped, err := f.load()
	if err != nil {
		t.Fatalf("首次运行没有账本，这不是错误: %v", err)
	}
	if len(recs) != 0 || skipped != 0 {
		t.Errorf("空账本读出 %d 条 / 跳过 %d", len(recs), skipped)
	}
}

// 崩溃最常在最后一行留下半截 JSON。它必须被忽略而不是让整个包报错——
// 报错会让程序起不来，用户看到的是「我的下载文件再也没被动过」。
func TestTornLastLineIsIgnored(t *testing.T) {
	f, path := openPending(t)
	good := rec(StateClaimed, nil)
	if err := f.append(good); err != nil {
		t.Fatal(err)
	}
	// 手工追加一条被腰斩的记录，模拟写到一半断电。
	bad := `{"id":"id-2","ts":"2026-10-07T12:00:00Z","state":"claimed","src":"C:\\x`
	if err := os.WriteFile(path, append(mustRead(t, path), []byte(bad)...), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, skipped, err := f.load()
	if err != nil {
		t.Fatalf("撕裂的尾行是致命的: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("读出 %d 条, 想要完好那条", len(recs))
	}
	if recs[0].ID != good.ID {
		t.Errorf("读回的是 %q", recs[0].ID)
	}
	if skipped != 0 {
		t.Errorf("撕裂的尾行不该计入 skipped（它连一行都算不上）: %d", skipped)
	}

	// 修完尾巴必须还能继续追加，且新行独立成行。
	if err := f.append(rec(StateDone, func(r *Record) { r.ID = "id-3" })); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err = f.load()
	if err != nil || skipped != 0 {
		t.Fatalf("再读一次出问题: %v / 跳过 %d", err, skipped)
	}
	if len(recs) != 2 {
		t.Fatalf("追加后读出 %d 条, 想要 2", len(recs))
	}
}

// 完整但读不懂的行（老版本字段、用户手改）跳过并计数，其余照读。
func TestGarbageCompleteLineIsCountedNotFatal(t *testing.T) {
	f, path := openPending(t)
	if err := f.append(rec(StateClaimed, func(r *Record) { r.ID = "id-a" })); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("{oops}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.append(rec(StateCopied, func(r *Record) { r.ID = "id-b" })); err != nil {
		t.Fatal(err)
	}

	recs, skipped, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, 想要 1（调用方要把它写进日志）", skipped)
	}
	if len(recs) != 2 || recs[0].ID != "id-a" || recs[1].ID != "id-b" {
		t.Errorf("好行没读全: %+v", recs)
	}
}

// 同一个 ID 的多次进度要归并，且顺序按第一次出现——启动日志要能讲出「先 a 后 b」。
func TestLoadKeepsFirstSeenOrder(t *testing.T) {
	f, _ := openPending(t)
	for _, r := range []Record{
		rec(StateClaimed, func(r *Record) { r.ID = "one" }),
		rec(StateClaimed, func(r *Record) { r.ID = "two" }),
		rec(StateMoving, func(r *Record) { r.ID = "one"; r.Tmp = r.Dst + TmpSuffix }),
		rec(StateCopied, func(r *Record) { r.ID = "two" }),
	} {
		if err := f.append(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, _, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("归并后 %d 条", len(recs))
	}
	if recs[0].ID != "one" || recs[0].State != StateMoving {
		t.Errorf("第一条 = %s/%s", recs[0].ID, recs[0].State)
	}
	if recs[1].ID != "two" || recs[1].State != StateCopied {
		t.Errorf("第二条 = %s/%s", recs[1].ID, recs[1].State)
	}
}

func TestRewriteIsAtomicAndReadable(t *testing.T) {
	f, path := openPending(t)
	if err := f.append(rec(StateDone, func(r *Record) { r.ID = "gone" })); err != nil {
		t.Fatal(err)
	}
	if err := f.append(rec(StateCopied, func(r *Record) { r.ID = "keep" })); err != nil {
		t.Fatal(err)
	}
	all, _, _ := f.load()

	var keep []Record
	for _, r := range all {
		if r.ID == "keep" {
			keep = append(keep, r)
		}
	}
	if err := f.rewrite(keep); err != nil {
		t.Fatal(err)
	}

	got, skipped, err := f.load()
	if err != nil || skipped != 0 {
		t.Fatalf("%v / %d", err, skipped)
	}
	if len(got) != 1 || got[0].ID != "keep" {
		t.Errorf("重写后读出 %+v", got)
	}
	// 重写留下的旧档要能在现场被捡回来。
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("没有 .bak: %v", err)
	}
	// 压缩用完不能把临时文件撒在数据目录里。
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "pending-*.jsonl.tmp"))
	if len(matches) != 0 {
		t.Errorf("留下了临时文件: %v", matches)
	}
}

func TestCompactKeepsUnresolvedEvenWhenOld(t *testing.T) {
	dir := t.TempDir()
	j, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := t0.Add(-30 * 24 * time.Hour)

	// 一直被供应商占用的文件会在账本里躺很久，压缩绝不能把它当旧账抹掉。
	if err := j.Append(rec(StateCopied, func(r *Record) { r.ID = "stuck"; r.TS = old; r.Src = `C:\Users\me\Downloads\stuck.zip` })); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(rec(StateDone, func(r *Record) { r.ID = "long-ago"; r.TS = old; r.Src = `C:\Users\me\Downloads\long-ago.zip` })); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(rec(StateAbandoned, func(r *Record) {
		r.ID = "junk"
		r.TS = old
		r.Src = `C:\Users\me\Downloads\junk.zip`
		r.Reason = "目标名上已经有别的文件"
	})); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(rec(StateDone, func(r *Record) { r.ID = "recent"; r.Src = `C:\Users\me\Downloads\recent.zip` })); err != nil {
		t.Fatal(err)
	}

	dropped, err := j.Compact(t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 只有「已结清且超过 keepFor」的两条会被清走：recent 太新，stuck 还没结清。
	if dropped != 2 {
		t.Errorf("清走 %d 条, 想要 2", dropped)
	}
	recs, _, err := j.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("压缩后剩 %d 条: %+v", len(recs), recs)
	}
	if recs[0].ID != "stuck" || recs[1].ID != "recent" {
		t.Errorf("留下的不对: %s %s", recs[0].ID, recs[1].ID)
	}

	// abandoned 被抹掉前必须落到人读的流水里，否则这条异常就彻底没人知道了；
	// done 的不补——它在搬成功当时已经写过一行，再写一遍是重复记账。
	raw, err := os.ReadFile(filepath.Join(dir, "moves.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(raw), "\n")
	if lines != 1 {
		t.Errorf("流水多了 %d 行, 想要只补 1 行", lines)
	}
	if !strings.Contains(string(raw), "junk.zip") || !strings.Contains(string(raw), `"outcome":"abandoned"`) {
		t.Errorf("abandoned 没进流水: %s", shortHead(raw))
	}
	if strings.Contains(string(raw), "long-ago.zip") {
		t.Error("done 的记录被重复写进流水")
	}

	// 第二次压缩无事可做，不该再动文件。
	before, _ := os.ReadFile(filepath.Join(dir, "pending.jsonl"))
	if d, err := j.Compact(t0.Add(time.Hour)); err != nil || d != 0 {
		t.Errorf("重复压缩: %d %v", d, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "pending.jsonl"))
	if string(before) != string(after) {
		t.Error("无事可做时仍然重写了账本")
	}
}

func TestNewCreatesDirAndPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "download2")
	j, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("数据目录没建出来: %v", err)
	}
	if j.Dir() != dir {
		t.Errorf("Dir() = %q", j.Dir())
	}
	if _, err := New(""); err == nil {
		t.Error("空目录该拒绝：那会把账本写到当前工作目录去")
	}
	// 还没写过任何东西时，Pending 与 Replay 都该安静地给出空。
	recs, skipped, err := j.Pending()
	if err != nil || len(recs) != 0 || skipped != 0 {
		t.Errorf("%d %d %v", len(recs), skipped, err)
	}
}

func TestReplayUsesTheLedgerAndTheObserver(t *testing.T) {
	dir := t.TempDir()
	j, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 崩溃前记下「已复制、还没删源」。
	r := rec(StateCopied, func(r *Record) { r.ID = "x" })
	if err := j.Append(r); err != nil {
		t.Fatal(err)
	}

	actions, skipped, err := j.Replay(stubObserver{v: view(r)})
	if err != nil || skipped != 0 {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ActionFinishDelete {
		t.Fatalf("重放给出 %+v", actions)
	}
	if actions[0].Record.ID != "x" {
		t.Error("动作没带回记录")
	}
}

type stubObserver struct {
	v   View
	err error
}

func (o stubObserver) Observe(Record) (View, error) { return o.v, o.err }

func TestLogMoveRequiresOutcome(t *testing.T) {
	j, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := j.LogMove(Move{TS: t0, Src: "a"}); err == nil {
		t.Error("没有 outcome 的流水竟然写进去了")
	}
	if err := j.LogMove(Move{Src: "a", Outcome: OutcomeMoved}); err == nil {
		t.Error("没有时间戳的流水竟然写进去了")
	}
	if err := j.LogMove(Move{TS: t0, Src: `C:\a.zip`, Dst: `D:\a.zip`, Size: 10,
		Method: "rename", Outcome: OutcomeMoved, Ms: 3}); err != nil {
		t.Fatal(err)
	}
	// 流水要能被 grep 与 Excel 之外的工具读：一行一个 JSON，且字段名稳定。
	raw, err := os.ReadFile(filepath.Join(j.Dir(), "moves.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("流水最后一行没换行")
	}
	var m map[string]any
	if err := json.Unmarshal(lastLine(raw), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "src", "dst", "size", "method", "outcome", "ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("流水里少了 %q 字段", k)
		}
	}
}

// MovesPath 报的是 Journal 自己那份路径：按钮拿它去开文件，所以这个名字只能有一处定义。
func TestMovesPathIsTheJournalsOwn(t *testing.T) {
	dir := t.TempDir()
	j, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "moves.jsonl"); j.MovesPath() != want {
		t.Errorf("MovesPath() = %q, 想要 %q", j.MovesPath(), want)
	}
}

// 「清空日志」那颗按钮动的只是 moves.jsonl，pending.jsonl 一个字节都不许少。
// 这条测试钉的就是那份分工：清完流水，崩溃账本还在、原来那一笔还读得出来。
func TestClearMovesEmptiesTheStreamOnly(t *testing.T) {
	j, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 一行都还没写过：清空一份不存在的流水不是故障（用户可能一次都没搬过）。
	if err := j.ClearMoves(); err != nil {
		t.Fatalf("清空一份还没生成的流水竟然报错: %v", err)
	}
	if _, err := os.Stat(j.MovesPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("MovesPath 指向的文件本不该存在：%v", err)
	}

	if err := j.Append(rec(StateCopied, nil)); err != nil {
		t.Fatal(err)
	}
	if err := j.LogMove(Move{TS: t0, Src: `C:\a.zip`, Dst: `D:\a.zip`, Outcome: OutcomeMoved}); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(j.Dir(), "pending.jsonl")
	before := mustRead(t, pendingPath)

	if err := j.ClearMoves(); err != nil {
		t.Fatal(err)
	}
	if raw := mustRead(t, j.MovesPath()); len(raw) != 0 {
		t.Errorf("流水没清空，还剩 %d 字节：%s", len(raw), shortHead(raw))
	}
	// 账本原样还在：它要是被这次「清空」带走了，下一次启动就对上次崩溃失忆。
	if after := mustRead(t, pendingPath); string(after) != string(before) {
		t.Errorf("清空流水动了崩溃账本：%s → %s", shortHead(before), shortHead(after))
	}
	recs, _, err := j.Pending()
	if err != nil || len(recs) != 1 {
		t.Errorf("账本读不出原来那一笔：%d 条, %v", len(recs), err)
	}
	// 截断之后路径还在，下一次搬运照样能往里追加（写入侧是 O_APPEND|O_CREATE）。
	if err := j.LogMove(Move{TS: t0, Src: `C:\b.zip`, Outcome: OutcomeStuck}); err != nil {
		t.Fatal(err)
	}
	if raw := mustRead(t, j.MovesPath()); !strings.Contains(string(raw), "b.zip") {
		t.Errorf("清空后的流水写不进新行：%s", shortHead(raw))
	}
}

func lastLine(raw []byte) []byte {
	s := strings.TrimRight(string(raw), "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return []byte(s)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// shortHead 用于失败信息：日志里可能带真实路径，只截一段、别把整份贴进测试输出。
func shortHead(raw []byte) string {
	s := string(raw)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
