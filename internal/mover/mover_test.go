package mover

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"download2/internal/apperr"
	"download2/internal/detect"
	"download2/internal/journal"
)

// volsOn 按「路径落在哪个根下面」给卷号。
//
// 这是单测造得出跨卷的唯一办法：一块盘上放两个目录并不构成两个卷，
// 而跨卷那条路恰恰是最需要被跑到的一条（它是唯一会删源的路）。
// 真卷号本身另有 windows 那套测试对着真文件系统验。
type volsOn struct {
	srcRoot, dstRoot string
	srcID, dstID     uint64
}

func (v volsOn) VolumeID(p string) (uint64, error) {
	if within(p, v.dstRoot) {
		return v.dstID, nil
	}
	return v.srcID, nil
}

// env 是一条用例的现场：假的 Downloads、归档目录、真账本、缝好的搬运器。
type env struct {
	t    *testing.T
	src  string
	dst  string
	jr   *journal.Journal
	m    *Mover
	cfg  Config
	slep *sleeps

	// pre 记的是「用例开始前就在那儿的文件」。统一的残留检查只该管我们造出来的东西，
	// 陌生文件（包括用户自己的 .download2.tmp）本来就该原地留着。
	pre map[string]bool
}

type sleeps struct {
	n     int
	eachs []time.Duration
}

// newEnv 建现场。cfg 里没填的两项目的（SourceDir/TargetDir）用建出来的目录顶上，
// 用例要「故意不填」或「填个错位置」时改 e.m.settings 就行。
func newEnv(t *testing.T, cfg Config, mut func(*env, *Options)) *env {
	t.Helper()

	root := t.TempDir()
	src := filepath.Join(root, "downloads")
	dst := filepath.Join(root, "归档")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建 %s: %v", d, err)
		}
	}
	if cfg.SourceDir == "" {
		cfg.SourceDir = src
	}
	if cfg.TargetDir == "" {
		cfg.TargetDir = dst
	}

	jr, err := journal.New(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, src: src, dst: dst, jr: jr, cfg: cfg, slep: &sleeps{}, pre: map[string]bool{}}
	o := Options{
		Settings: func() Config { return e.cfg },
		Book:     jr,
		Clock:    fixedClock(time.Date(2026, 10, 7, 15, 4, 5, 0, time.Local)),
		Sleep:    func(d time.Duration) { e.slep.n++; e.slep.eachs = append(e.slep.eachs, d) },
		Volume:   volsOn{srcRoot: src, dstRoot: cfg.TargetDir, srcID: 7, dstID: 7},
	}
	if mut != nil {
		mut(e, &o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.m = m

	// 「.download2.tmp 绝不残留」不靠每条用例自己想：在这里统一验。
	t.Cleanup(e.checkNoTmp)
	return e
}

// cross 把两端的卷号拆开，用例就走上复制那条路。
func cross(e *env, o *Options) {
	o.Volume = volsOn{srcRoot: e.src, dstRoot: e.cfg.TargetDir, srcID: 7, dstID: 8}
}

func (e *env) checkNoTmp() {
	for _, root := range []string{e.src, e.cfg.TargetDir} {
		if root == "" {
			continue
		}
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // 用例自己删掉的目录不算问题
			}
			if !fi.IsDir() && strings.HasSuffix(p, journal.TmpSuffix) && !e.pre[filepath.Clean(p)] {
				e.t.Errorf("%s 残留在我们自己的中间名上", p)
			}
			return nil
		})
	}
}

// put 在假的 Downloads 里造一个文件（相对路径，内容随便一句中文能辨认）。
func (e *env) put(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// blob 造一个指定大小的文件，字节全是 'x'。
func (e *env) blob(rel string, size int) string {
	e.t.Helper()
	p := filepath.Join(e.src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// keep 声明「这个文件在用例开始前就在那儿，我们不负责清它」。
func (e *env) keep(p string) string {
	e.pre[filepath.Clean(p)] = true
	return p
}

// foreign 在归档目录里造一个「别人的文件」，用来验我们不覆盖、也不删。
func (e *env) foreign(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.cfg.TargetDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.pre[filepath.Clean(p)] = true
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}

func fiOf(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("看 %s: %v", p, err)
	}
	return fi
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// trail 读出账本的每一行（append-only 的全部历史，不只是每个 ID 的最新状态）。
//
// 断言要落在历史上而不是最新状态上：「先记账再动字节」只有从顺序才看得出来 ——
// done 写在删源之前是错的、写在之后才对，而最新状态里两种写法长得一样。
func (e *env) trail() []journal.Record {
	e.t.Helper()
	return recordsOf(e.t, filepath.Join(e.jr.Dir(), "pending.jsonl"))
}

func recordsOf(t *testing.T, path string) []journal.Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("读 %s: %v", path, err)
	}
	var out []journal.Record
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r journal.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%s 有一行读不懂: %v\n%s", path, err, line)
		}
		out = append(out, r)
	}
	return out
}

func states(recs []journal.Record) string {
	names := make([]string, 0, len(recs))
	for _, r := range recs {
		names = append(names, string(r.State))
	}
	return strings.Join(names, " → ")
}

func (e *env) moves() []journal.Move {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.jr.Dir(), "moves.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		e.t.Fatalf("读流水: %v", err)
	}
	var out []journal.Move
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var mv journal.Move
		if err := json.Unmarshal([]byte(line), &mv); err != nil {
			e.t.Fatalf("流水有一行读不懂: %v", err)
		}
		out = append(out, mv)
	}
	return out
}

// replaySafe 验失败之后最要紧的一件事：账本交给重放，重放只会说「重新观察」，
// 绝不会说「删了这个」。搬运失败时最怕的就是账本把现场说歪。
func (e *env) replaySafe() {
	e.t.Helper()
	for _, r := range e.trail() {
		if r.State.Resolved() {
			continue
		}
		a := journal.Plan(r, e.view(r))
		switch a.Kind {
		case journal.ActionRewatch, journal.ActionNone:
		default:
			e.t.Errorf("%s 的账本重放出 %s（%s），失败之后不该有比重新观察更重的动作",
				r.Src, a.Kind, a.Reason)
		}
	}
}

func (e *env) view(r journal.Record) journal.View {
	v := journal.View{}
	if s, err := detect.StatSample(r.Src); err == nil {
		v.SrcExists, v.SrcSize, v.SrcMtime, v.SrcIndex = true, s.Size, s.Mtime, s.Index
	}
	if r.Tmp != "" {
		v.TmpExists = exists(r.Tmp)
	}
	if s, err := detect.StatSample(r.Dst); err == nil && !s.IsDir {
		v.DstExists, v.DstSize = true, s.Size
	}
	return v
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// TestSameVolumeMoveIsOneRename 走同卷那条路：一次改名、零字节拷贝。
func TestSameVolumeMoveIsOneRename(t *testing.T) {
	e := newEnv(t, Config{DateSubdirs: true}, nil)
	old := time.Date(2026, 3, 5, 9, 0, 0, 0, time.Local)
	src := e.blob("电影.zip", 4096)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	want := filepath.Join(e.cfg.TargetDir, "2026-03-05", "电影.zip")
	if res.Dst != want {
		t.Errorf("落地在 %s, 想要 %s", res.Dst, want)
	}
	if res.Method != MethodRename {
		t.Errorf("手段是 %s, 想要 rename", res.Method)
	}
	if res.Dated != "2026-03-05" {
		t.Errorf("日期目录是 %q, 想要 2026-03-05", res.Dated)
	}
	if exists(src) {
		t.Error("源还在原地")
	}
	if got := fiOf(t, want); got.Size() != 4096 {
		t.Errorf("落地文件 %d 字节, 想要 4096", got.Size())
	}
	// 改名不动 mtime：日期分目录按源文件的 mtime 归，这个时间一错用户的归档就乱了。
	if got := fiOf(t, want).ModTime(); !got.Equal(old) {
		t.Errorf("改名动了 mtime, 想要 %v 得到 %v", old, got)
	}
	if got := states(e.trail()); got != "claimed → done" {
		t.Errorf("账本历史是 %s, 想要 claimed → done", got)
	}
	// 改名路一格都不许记中间名。「先改成中间名、再改成最终名」并不会更原子 —— 改名本来就是原子的 ——
	// 却会造出一个真正致命的现场：源已消失、目标未就位、只剩那个中间名。
	// 重放表对这一格的动作是 ActionCleanTmp，等于把用户文件的唯一副本删掉。
	for _, r := range e.trail() {
		if r.Tmp != "" {
			t.Errorf("%s 那行记了中间名 %q，改名路不该有", r.State, r.Tmp)
			break
		}
	}
	ms := e.moves()
	if len(ms) != 1 || ms[0].Outcome != journal.OutcomeMoved || ms[0].Method != MethodRename {
		t.Errorf("流水是 %+v, 想要恰好一行 moved/rename", ms)
	}
	if ms[0].Dst != want {
		t.Errorf("流水记的目标是 %s, 想要 %s", ms[0].Dst, want)
	}
}

// TestFlatTargetWhenDateSubdirsOff 关掉分目录就落在转移目录顶层。
func TestFlatTargetWhenDateSubdirsOff(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	src := e.put("说明.txt", "一句说明")
	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if want := filepath.Join(e.cfg.TargetDir, "说明.txt"); res.Dst != want {
		t.Errorf("落在 %s, 想要 %s", res.Dst, want)
	}
	if res.Dated != "" {
		t.Errorf("没开分目录却报了日期 %q", res.Dated)
	}
}

// TestCrossVolumeMoveCopiesThenVerifies 走跨卷那条路：复制、核对、才删源。
func TestCrossVolumeMoveCopiesThenVerifies(t *testing.T) {
	e := newEnv(t, Config{}, cross)
	src := e.put("发票.pdf", "一张发票的内容")
	old := time.Date(2026, 2, 1, 8, 30, 0, 0, time.Local)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if res.Method != MethodCopy {
		t.Errorf("手段是 %s, 想要 copy（卷号不同）", res.Method)
	}
	if exists(src) {
		t.Error("目标已核对过了，源却还留着")
	}
	if got := read(t, res.Dst); got != "一张发票的内容" {
		t.Errorf("落地内容被改动了: %q", got)
	}
	got := states(e.trail())
	if want := "claimed → moving → copied → done"; got != want {
		t.Errorf("账本历史是 %s, 想要 %s", got, want)
	}
	recs := e.trail()
	last := recs[len(recs)-1]
	if want := int64(len("一张发票的内容")); last.DstSize != want {
		t.Errorf("done 那行的 DstSize 是 %d, 想要 %d", last.DstSize, want)
	}
	if last.Tmp == "" {
		t.Error("账本没记下中间名叫什么，重放时无从查它是不是我们的东西")
	}
	// 副本必须继承源的 mtime：跨卷复制会把时间刷成「现在」，而用户是按日期找文件的。
	if got := fiOf(t, res.Dst).ModTime(); !got.Equal(old) {
		t.Errorf("副本 mtime 是 %v, 想要跟上源的 %v", got, old)
	}
}

// TestCopyThatWritesTooLittleNeverDeletesSource 是最要紧的一条：
// 复制少了字节时，源必须完整留在原地，坏副本必须被清掉。
func TestCopyThatWritesTooLittleNeverDeletesSource(t *testing.T) {
	e := newEnv(t, Config{}, func(en *env, o *Options) {
		cross(en, o)
		// 只写一半就「成功」返回：正是跨卷复制中途被打断的形状。
		o.Copy = func(dst io.Writer, src io.Reader) (int64, error) {
			return io.Copy(dst, io.LimitReader(src, 3))
		}
	})
	src := e.put("大文件.bin", "1234567890")

	_, err := e.m.Move(src)
	if err == nil {
		t.Fatal("字节数对不上居然报成功了")
	}
	if code := apperr.Of(err).Code; code != apperr.EFileOpFailed {
		t.Errorf("码是 %s, 想要 E_FILE_OP_FAILED", code)
	}
	if !exists(src) {
		t.Fatal("源被删了 —— 这一条是整个包的红线")
	}
	if got := read(t, src); got != "1234567890" {
		t.Errorf("源内容变了: %q", got)
	}
	if exists(filepath.Join(e.cfg.TargetDir, "大文件.bin")) {
		t.Error("没核对过的副本上岗了")
	}
	if s := states(e.trail()); strings.Contains(s, "copied") || strings.Contains(s, "done") {
		t.Errorf("账本历史是 %s，可副本根本没核对过", s)
	}
	if len(e.moves()) != 0 {
		t.Error("没搬成却记了流水")
	}
	e.replaySafe()
}

// TestVerifyRejectsSourceThatGrewAfterDetection 单独钉 verify 的第二道判据：
// 判定之后源又变了（下载器回来续传了），这一轮整个作废。
func TestVerifyRejectsSourceThatGrewAfterDetection(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	src := e.put("a.bin", "0123456789")
	tmp := filepath.Join(e.cfg.TargetDir, "a.bin"+journal.TmpSuffix)
	t.Cleanup(func() { _ = os.Remove(tmp) })
	if err := os.WriteFile(tmp, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 副本与源都 10 字节，但判定时看到的是 4 字节。
	j := &job{sample: detect.Sample{Size: 4}, rec: journal.Record{Src: src}}
	err := e.m.verify(j, src, tmp)
	if err == nil {
		t.Fatal("源在判定后变过却没被拦下")
	}
	if !strings.Contains(err.Error(), "判定之后又变了") {
		t.Errorf("文案是 %q, 要说清是哪一道闸拦的", err.Error())
	}
	if j.dstSize != 0 {
		t.Error("没通过核对却填了落地字节数")
	}

	// 第一道闸：副本比源短。
	if err := os.WriteFile(tmp, []byte("01"), 0o600); err != nil {
		t.Fatal(err)
	}
	j2 := &job{sample: detect.Sample{Size: 10}, rec: journal.Record{Src: src}}
	if err := e.m.verify(j2, src, tmp); err == nil {
		t.Error("副本比源短却没被拦下")
	} else if !strings.Contains(err.Error(), "对不上就不删源") {
		t.Errorf("文案是 %q, 想要说清「不删源」", err.Error())
	}
	if err := os.WriteFile(tmp, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	j3 := &job{sample: detect.Sample{Size: 10}, rec: journal.Record{Src: src}}
	if err := e.m.verify(j3, src, tmp); err != nil {
		t.Errorf("三个数齐了还被拦: %v", err)
	}
	if j3.dstSize != 10 {
		t.Errorf("核对过了却没记落地字节数: %d", j3.dstSize)
	}
}

// TestConflictAppendsNumbers 是本机最容易踩的一条：目标里已有同名文件时加号，
// 而不是覆盖 —— Windows 的改名撞上已存在文件是会成功并吃掉对方的。
func TestConflictAppendsNumbers(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	old := e.foreign("发票.pdf", "上个月的旧版")
	src := e.put("发票.pdf", "这个月的新版")

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if want := filepath.Join(e.cfg.TargetDir, "发票 (1).pdf"); res.Dst != want {
		t.Errorf("落在 %s, 想要 %s", res.Dst, want)
	}
	if got := read(t, old); got != "上个月的旧版" {
		t.Errorf("旧版被吃掉了: %q", got)
	}
	if got := read(t, res.Dst); got != "这个月的新版" {
		t.Errorf("新版内容不对: %q", got)
	}

	// 再加一号：(1) 也被占了，应该出 (2)。
	if err := os.WriteFile(filepath.Join(e.cfg.TargetDir, "发票 (1).pdf"), []byte("占位"), 0o600); err != nil {
		t.Fatal(err)
	}
	res2, err := e.m.Move(e.put("发票.pdf", "第三版"))
	if err != nil {
		t.Fatalf("第二次搬运失败: %v", err)
	}
	if want := filepath.Join(e.cfg.TargetDir, "发票 (2).pdf"); res2.Dst != want {
		t.Errorf("落在 %s, 想要 %s", res2.Dst, want)
	}
}

// TestCrossVolumeConflictUsesNumbersToo 验复制那条路的加号：
// 中间名、最终名都得让开已有文件。
func TestCrossVolumeConflictUsesNumbersToo(t *testing.T) {
	e := newEnv(t, Config{}, cross)
	e.foreign("a.bin", "旧的")
	src := e.put("a.bin", "新的")

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if want := filepath.Join(e.cfg.TargetDir, "a (1).bin"); res.Dst != want {
		t.Errorf("落在 %s, 想要 %s", res.Dst, want)
	}
	if got := read(t, filepath.Join(e.cfg.TargetDir, "a.bin")); got != "旧的" {
		t.Errorf("旧文件被盖了: %q", got)
	}
}

// TestRenameRaceRepicksName 验的是「挑完名字之后有人抢先占了它」：
// 改名必须失败后重挑，而不是覆盖。
func TestRenameRaceRepicksName(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	old := e.foreign("report.pdf", "别人的")
	src := e.put("report.pdf", "我的")

	real := e.m.rename
	var calls int
	e.m.rename = func(s, d string) error {
		calls++
		if calls == 1 {
			// 模拟 TOCTOU：先把我们要用的名字占上，再让真正的改名去撞它。
			if err := os.WriteFile(filepath.Join(e.cfg.TargetDir, "report (1).pdf"), []byte("抢"), 0o600); err != nil {
				return err
			}
		}
		return real(s, d)
	}

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if res.Conflicts != 1 {
		t.Errorf("重挑了 %d 次, 想要 1", res.Conflicts)
	}
	if want := filepath.Join(e.cfg.TargetDir, "report (2).pdf"); res.Dst != want {
		t.Errorf("落在 %s, 想要 %s", res.Dst, want)
	}
	if got := read(t, old); got != "别人的" {
		t.Errorf("旧文件被动了: %q", got)
	}
	if got := read(t, filepath.Join(e.cfg.TargetDir, "report (1).pdf")); got != "抢" {
		t.Errorf("抢位的那个文件被我们改了: %q", got)
	}
	if s := states(e.trail()); s != "claimed → claimed → done" {
		t.Errorf("账本历史是 %s, 想要换名字那次也记上一笔", s)
	}
}

// TestBusyRenameRetriesThenGivesUpWithoutTouchingSource 是占用那条路：
// 等得完就搬，等不完就如实报错，并且一个字节都不动。
func TestBusyRenameRetriesThenGivesUpWithoutTouchingSource(t *testing.T) {
	e := newEnv(t, Config{}, nil)
	src := e.put("占用中.docx", "内容")
	e.m.rename = func(string, string) error { return errBusy }

	res, err := e.m.Move(src)
	if err == nil {
		t.Fatal("一直占用居然报成功了")
	}
	if msg := apperr.Of(err).Msg; !strings.Contains(msg, "占用") {
		t.Errorf("界面文案是 %q, 要说「还被占用」", msg)
	}
	// 末次失败之后不再等：那一下等完也不会再有第九次尝试，白挂住搬运线程。
	if e.slep.n != renameTries-1 {
		t.Errorf("等了 %d 轮, 想要 %d 轮", e.slep.n, renameTries-1)
	}
	if res.Tries != renameTries-1 {
		t.Errorf("报告里的重试次数是 %d, 想要 %d", res.Tries, renameTries-1)
	}
	want := "250ms 500ms 1s 2s 4s 250ms 500ms"
	if got := ticks(e.slep.eachs); got != want {
		t.Errorf("等待节拍是 %s, 想要 %s", ticks(e.slep.eachs), want)
	}
	if got := read(t, src); got != "内容" {
		t.Errorf("源被动了: %q", got)
	}
	if exists(filepath.Join(e.cfg.TargetDir, "占用中.docx")) {
		t.Error("目标名上出现了没搬成的文件")
	}
	if s := states(e.trail()); s != "claimed" {
		t.Errorf("账本历史是 %s, 想要只有一行 claimed", s)
	}
	e.replaySafe()
}

// TestPublishRetriesWhileTheCopyIsBusy：副本刚写完，实时扫描很可能正捧着它。
// 「换上最终名」和「把源搬走」一样是改名，一样要吃占用，不能一撞就散。
func TestPublishRetriesWhileTheCopyIsBusy(t *testing.T) {
	e := newEnv(t, Config{}, cross)
	src := e.put("发票.pdf", "一张发票的内容")

	real := e.m.rename
	var calls int
	e.m.rename = func(s, d string) error {
		calls++
		if calls == 1 {
			return errBusy
		}
		return real(s, d)
	}

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("占用了一下就没搬成: %v", err)
	}
	if res.Tries != 1 {
		t.Errorf("重试了 %d 次, 想要 1 次", res.Tries)
	}
	if got := read(t, res.Dst); got != "一张发票的内容" {
		t.Errorf("落地内容被改动了: %q", got)
	}
	if exists(src) {
		t.Error("副本已就位，源却还留着")
	}
	if got := states(e.trail()); got != "claimed → moving → copied → done" {
		t.Errorf("账本历史是 %s, 想要 claimed → moving → copied → done", got)
	}
}

func ticks(ds []time.Duration) string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.String())
	}
	return strings.Join(out, " ")
}

// TestPendingDeleteIsADebtNotAFailure：目标已就位、源还被占用时搬运算成功，
// 但账本必须留下欠条，否则那个源永远没人补删。
func TestPendingDeleteIsADebtNotAFailure(t *testing.T) {
	e := newEnv(t, Config{}, cross)
	src := e.put("视频.mp4", "一大段视频")
	e.m.remove = func(string) error { return errBusy }

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("欠一次补删不是失败: %v", err)
	}
	if !res.PendingDelete {
		t.Error("结果没说清欠补删")
	}
	if !exists(src) {
		t.Error("源明明删不掉，却被报告成没了")
	}
	if got := read(t, res.Dst); got != "一大段视频" {
		t.Errorf("落地内容不对: %q", got)
	}
	want := "claimed → moving → copied → pendingDelete"
	if got := states(e.trail()); got != want {
		t.Errorf("账本历史是 %s, 想要 %s", got, want)
	}
	ms := e.moves()
	if len(ms) != 1 || !strings.Contains(ms[0].Reason, "补删") {
		t.Errorf("流水是 %+v, 想要一行写清欠条", ms)
	}

	// 欠条必须能被重放读成「补删」，而不是重新观察。
	recs := e.trail()
	last := recs[len(recs)-1]
	if a := journal.Plan(last, e.view(last)); a.Kind != journal.ActionFinishDelete {
		t.Errorf("重放说 %s, 想要 finishDelete（字节数核对过了才允许删源）", a.Kind)
	}
}

// TestTakenTmpNameIsNeverDestroyed：归档里已经躺着一个 .download2.tmp 的陌生文件时，
// 我们不删它，换一个带记录 ID 的中间名把活干完。
func TestTakenTmpNameIsNeverDestroyed(t *testing.T) {
	e := newEnv(t, Config{}, cross)
	stranger := e.foreign("照片.jpg"+journal.TmpSuffix, "别人家的临时文件")
	src := e.put("照片.jpg", "一张照片")

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if got := read(t, stranger); got != "别人家的临时文件" {
		t.Errorf("陌生文件被当成自己的垃圾删了/改了: %q", got)
	}
	if got := read(t, res.Dst); got != "一张照片" {
		t.Errorf("落地内容不对: %q", got)
	}
	if r := e.trail()[0]; r.Tmp == stranger {
		t.Error("账本把陌生文件当成了自己的中间名")
	}
}

// TestVolumeIDUnknownFallsBackToCopy：问不到卷号就不赌同卷，走会核对字节的复制路。
func TestVolumeIDUnknownFallsBackToCopy(t *testing.T) {
	e := newEnv(t, Config{}, func(en *env, o *Options) {
		o.Volume = volsOn{srcRoot: en.src, dstRoot: en.cfg.TargetDir, srcID: 0, dstID: 0}
	})
	res, err := e.m.Move(e.put("a.bin", "1234"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodCopy {
		t.Errorf("手段是 %s, 想要 copy（卷号 0 不能当同卷证据）", res.Method)
	}
}

// TestVolumeIDSaysNoButRenameSaysCrossDevice 是那条免费兜底：
// 卷号骗我们说同卷时，改名报「不是同一个设备」就当场换成复制路，一个字节都还没动。
func TestVolumeIDSaysNoButRenameSaysCrossDevice(t *testing.T) {
	e := newEnv(t, Config{}, nil) // 同卷 → 走改名
	src := e.put("a.bin", "1234")
	want := filepath.Join(e.cfg.TargetDir, "a.bin")

	first := true
	real := e.m.rename
	e.m.rename = func(s, d string) error {
		if first {
			first = false
			return errNotSameDeviceForTest
		}
		return real(s, d)
	}

	res, err := e.m.Move(src)
	if err != nil {
		t.Fatalf("换路之后应该搬成: %v", err)
	}
	if res.Method != MethodCopy {
		t.Errorf("手段是 %s, 想要 copy", res.Method)
	}
	if got := read(t, want); got != "1234" {
		t.Errorf("落地内容不对: %q", got)
	}
	if s := states(e.trail()); s != "claimed → claimed → moving → copied → done" {
		t.Errorf("账本历史是 %s, 想要换路那一笔也被记上", s)
	}
}

// TestRefusals 是动手之前的否决清单：每条都要有码、有中文、并且一个字节都没动。
func TestRefusals(t *testing.T) {
	cases := []struct {
		name string
		// setup 可以改 e.cfg（比如故意不填目标）并造出要搬的那个文件。
		setup func(*env) string
		code  apperr.Code
		msg   string
	}{
		{
			name: "没配转移目录",
			setup: func(e *env) string {
				e.cfg.TargetDir = ""
				return e.put("a.txt", "x")
			},
			code: apperr.ENotConfigured, msg: "转移目录",
		},
		{
			name: "转移目录是相对路径",
			setup: func(e *env) string {
				e.cfg.TargetDir = "归档"
				return e.put("a.txt", "x")
			},
			code: apperr.EInvalidArg, msg: "绝对路径",
		},
		{
			name:  "下载中间名",
			setup: func(e *env) string { return e.keep(e.put("movie.mp4.crdownload", "半截")) },
			code:  apperr.EPathForbidden, msg: "中间名",
		},
		{
			name: "我们自己残留的中间名",
			setup: func(e *env) string {
				// 它必须留在 Downloads 里不动，所以是本用例的「开始前就在」文件。
				return e.keep(e.put("movie.mp4"+journal.TmpSuffix, "残留"))
			},
			code: apperr.EPathForbidden, msg: "中间名",
		},
		{
			name:  "Office 属主锁文件",
			setup: func(e *env) string { return e.put("~$报告.docx", "锁") },
			code:  apperr.EPathForbidden, msg: "永远不参与搬运",
		},
		{
			name:  "系统文件",
			setup: func(e *env) string { return e.put("desktop.ini", "[.ShellClassInfo]") },
			code:  apperr.EPathForbidden, msg: "永远不参与搬运",
		},
		{
			name:  "0 字节",
			setup: func(e *env) string { return e.put("空.zip", "") },
			code:  apperr.EInvalidArg, msg: "0 字节",
		},
		{
			name: "目录",
			setup: func(e *env) string {
				d := filepath.Join(e.src, "一整个文件夹")
				if err := os.MkdirAll(d, 0o755); err != nil {
					e.t.Fatal(err)
				}
				return d
			},
			code: apperr.EPathForbidden, msg: "目录",
		},
		{
			name: "源已经在转移目录里",
			setup: func(e *env) string {
				d := filepath.Join(e.cfg.TargetDir, "里面")
				if err := os.MkdirAll(d, 0o755); err != nil {
					e.t.Fatal(err)
				}
				p := filepath.Join(d, "已经在里面.txt")
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					e.t.Fatal(err)
				}
				return p
			},
			code: apperr.EPathForbidden, msg: "不再搬一次",
		},
		{
			name: "转移目录落在监控目录里面",
			setup: func(e *env) string {
				d := filepath.Join(e.src, "归档子目录")
				if err := os.MkdirAll(d, 0o755); err != nil {
					e.t.Fatal(err)
				}
				e.cfg.TargetDir = d
				return e.put("顶层.txt", "x")
			},
			code: apperr.EPathForbidden, msg: "再搬一次",
		},
		{
			name: "文件已经不在了",
			setup: func(e *env) string {
				p := e.put("刚才还在.txt", "x")
				if err := os.Remove(p); err != nil {
					e.t.Fatal(err)
				}
				return p
			},
			code: apperr.EFileOpFailed, msg: "读不到",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{}, nil)
			src := c.setup(e)
			_, err := e.m.Move(src)
			if err == nil {
				t.Fatalf("%s 被搬走了", c.name)
			}
			ae := apperr.Of(err)
			if ae.Code != c.code {
				t.Errorf("码是 %s, 想要 %s（%v）", ae.Code, c.code, err)
			}
			if !strings.Contains(ae.Msg, c.msg) {
				t.Errorf("文案是 %q, 想要含 %q", ae.Msg, c.msg)
			}
			if !strings.HasSuffix(ae.Msg, "。") {
				t.Errorf("界面文案该是一句完整的中文: %q", ae.Msg)
			}
			e.replaySafe()
		})
	}
}
