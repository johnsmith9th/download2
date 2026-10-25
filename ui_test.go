package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"download2/internal/journal"
)

// sizeDIP 是 Go 与 DOM 之间那份几何约定的另一半：CSS 里三块各按自己的变量长，
// 而窗口高度是这里算出来的。两处不一致的表现是「裁字」或「一条点不动的透明区」，
// 所以这几个数得钉住。
func TestSizeDIP(t *testing.T) {
	cases := []struct {
		name     string
		expanded bool
		toast    bool
		w, h     int
	}{
		{"药丸", false, false, pillW, pillH},
		{"展开", true, false, panelW, pillH + stackGap + panelH},
		{"药丸加提示", false, true, pillW, pillH + stackGap + toastH},
		{"展开加提示", true, true, panelW, pillH + stackGap + panelH + stackGap + toastH},
	}
	for _, c := range cases {
		gotW, gotH := sizeDIP(c.expanded, c.toast)
		if gotW != c.w || gotH != c.h {
			t.Errorf("%s: 尺寸是 %dx%d, 想要 %dx%d", c.name, gotW, gotH, c.w, c.h)
		}
	}
}

// 提示条再大也不能把宽度改掉：DOM 里 toast 和药丸同宽，窗口一旦加宽就会露出
// 一条谁也点不到的透明边。
func TestToastDoesNotWiden(t *testing.T) {
	for _, exp := range []bool{false, true} {
		w0, h0 := sizeDIP(exp, false)
		w1, h1 := sizeDIP(exp, true)
		if w0 != w1 {
			t.Errorf("expanded=%v 时提示条把宽度从 %d 改成了 %d", exp, w0, w1)
		}
		if h1 <= h0 {
			t.Errorf("expanded=%v 时提示条没把窗口加高：%d → %d", exp, h0, h1)
		}
	}
}

func TestDipToPhys(t *testing.T) {
	// 本机实测 150% 缩放：320×260 → 480×390，DIP→物理就是这一条式子。
	if got := dipToPhys(236, 1.5); got != 354 {
		t.Errorf("236 DIP 在 150%% 下是 %d, 想要 354", got)
	}
	if got := dipToPhys(40, 1.0); got != 40 {
		t.Errorf("100%% 下 40 DIP 成了 %d", got)
	}
	// 展开态那一条：面板改高度时这里的 340 要跟着动（四处钉同一个数，见 ui.go）。
	if got := dipToPhys(340, 1.25); got != 425 {
		t.Errorf("340 DIP 在 125%% 下是 %d, 想要 425", got)
	}
	// 四舍五入而不是截断：133.333 截成 133 会让每段高度各差 1 像素，
	// 攒起来就是「面板底部被切掉一行」。
	if got := dipToPhys(339, 1.25); got != 424 { // 423.75
		t.Errorf("339 DIP 在 125%% 下是 %d, 想要 424", got)
	}
	if got := dipToPhys(430, 1.25); got != 538 { // 537.5：正好半分，向上取
		t.Errorf("430 DIP 在 125%% 下是 %d, 想要 538", got)
	}
}

// decideDrop 判的是「往上放不放得下」，不是「药丸在上半屏还是下半屏」。
// 这两件事在中间那条线上不一样，而选错的代价是面板被屏幕切掉一截。
func TestDecideDrop(t *testing.T) {
	wa := application.Rect{X: 0, Y: 0, Width: 2560, Height: 1400} // 2K 那档屏幕减掉任务栏
	ph := dipToPhys(386, 1.5)                                     // 展开态整叠的物理高度
	pillHPhys := dipToPhys(pillH, 1.5)
	pill := func(y int) application.Rect {
		return application.Rect{X: 1500, Y: y, Width: dipToPhys(pillW, 1.5), Height: pillHPhys}
	}

	cases := []struct {
		name string
		y    int
		want string
	}{
		{"贴底：往上放得下", wa.Height - pillHPhys - 10, dropUp},
		{"贴顶：只能往下弹", 40, dropDown},
		{"偏上：上边差一截、下边够用", 400, dropDown},
		// 判据是「往上放不放得下」，不是上半屏/下半屏：这条线上两个答案会分叉。
		{"往上刚好放下（>= 是有意的）", ph - pillHPhys, dropUp},
		{"再往下一像素就不算放得下了", ph - pillHPhys - 1, dropDown},
	}
	// 最后两条要成立，下边必须真的够用 —— 否则它们测的是「两边都放不下」那条支路。
	if wa.Height < 2*ph {
		t.Fatalf("这条测试的工作区太矮：Height=%d 放不下两份展开态 %d", wa.Height, 2*ph)
	}
	for _, c := range cases {
		if got := decideDrop(pill(c.y), wa, ph); got != c.want {
			t.Errorf("%s: 药丸在 y=%d 时判成 %q, 想要 %q", c.name, c.y, got, c.want)
		}
	}

	// 两边都放不下：选余量大的那一边，之后 applyGeometry 会把它夹进工作区。
	// 判据本身必须能区分「上边宽裕一点」和「下边宽裕一点」，否则矮屏幕上永远只试一边。
	short := application.Rect{X: 0, Y: 0, Width: 1920, Height: 260}
	if got := decideDrop(pill(180), short, ph); got != dropUp {
		t.Errorf("上面余量更大时判成 %q, 想要 %q", got, dropUp)
	}
	if got := decideDrop(pill(20), short, ph); got != dropDown {
		t.Errorf("下面余量更大时判成 %q, 想要 %q", got, dropDown)
	}

	// 读不到工作区（GetScreen 失败）时不许猜：猜错的方向会把面板整个推出屏幕。
	if got := decideDrop(pill(40), application.Rect{}, ph); got != dropUp {
		t.Errorf("工作区读不到时判成 %q, 想要 %q", got, dropUp)
	}
}

// pillRect 钉的是「收起态窗口底边 = 药丸底边」这条约定。
// 提示条亮着时窗口顶边比药丸顶边高出一条 toast —— 不切这一刀，decideDrop
// 会以为下面比上面宽裕，在只有提示条出现的那一轮里翻错方向。
func TestPillRectTracksTheWindowBottomEdge(t *testing.T) {
	const scale = 1.5
	pillPhys := dipToPhys(pillH, scale)

	// 纯药丸：窗口就是药丸。
	win := application.Rect{X: 1500, Y: 990, Width: dipToPhys(pillW, scale), Height: pillPhys}
	if got := pillRect(win, scale); got != win {
		t.Errorf("药丸自身那段 = %+v, 想要 %+v", got, win)
	}

	// 药丸 + 提示条：底边不动，顶边多出一条 toast，药丸那段要从底边往回切。
	toastWin := application.Rect{X: win.X, Y: win.Y - dipToPhys(stackGap+toastH, scale), Width: win.Width, Height: win.Height + dipToPhys(stackGap+toastH, scale)}
	got := pillRect(toastWin, scale)
	if got.Y != win.Y || got.Height != pillPhys {
		t.Errorf("带提示条时药丸那段是 y=%d h=%d, 想要 y=%d h=%d", got.Y, got.Height, win.Y, pillPhys)
	}
}

// anchorY 钉的是这一条约定：形态切换前后，药丸在屏幕上的那一段一动不许动。
//
// 旧的实现只用「这一轮的方向」去锚「上一轮的矩形」，于是收起配置页时 drop 已复位成 up、
// 而窗口还按 down 排着（药丸在顶边），按 y+h 锚就把药丸推下整整一个面板高 ——
// 用户看到的是「关掉配置页，工具条自己跳走了」。
func TestAnchorYKeepsThePillPut(t *testing.T) {
	const scale = 1.5
	pillPhys := dipToPhys(pillH, scale)
	hOf := func(expanded, toast bool) int {
		_, dh := sizeDIP(expanded, toast)
		return dipToPhys(dh, scale)
	}
	// 药丸在窗口里占的那一段顶边，由该窗口排列时用的方向定。
	pillTop := func(win application.Rect, drop string) int {
		if drop == dropDown {
			return win.Y
		}
		return win.Y + win.Height - pillPhys
	}

	cases := []struct {
		name           string
		win            application.Rect
		prevDrop, drop string
		ph             int
	}{
		{"收起态向上展开", application.Rect{X: 1500, Y: 927, Height: hOf(false, false)}, dropUp, dropUp, hOf(true, false)},
		{"面板收回（up：面板原本压在上方）", application.Rect{X: 1500, Y: 408, Height: hOf(true, false)}, dropUp, dropUp, hOf(false, false)},
		{"面板收回（down：面板原本压在下方）", application.Rect{X: 1500, Y: 100, Height: hOf(true, false)}, dropDown, dropUp, hOf(false, false)},
		{"贴顶展开、提示条还亮着", application.Rect{X: 1500, Y: 900, Height: hOf(false, true)}, dropUp, dropDown, hOf(true, false)},
		{"down 态提示条亮起", application.Rect{X: 1500, Y: 100, Height: hOf(true, false)}, dropDown, dropDown, hOf(true, true)},
		{"up 态提示条熄灭", application.Rect{X: 1500, Y: 408, Height: hOf(true, true)}, dropUp, dropUp, hOf(true, false)},
		{"方向是零值时按 up 算", application.Rect{X: 1500, Y: 408, Height: hOf(true, false)}, "", dropUp, hOf(false, false)},
	}
	for _, c := range cases {
		got := anchorY(c.win, scale, c.prevDrop, c.drop, c.ph)
		// 约定本身：新矩形里的药丸顶边 == 旧的药丸顶边。
		newWin := application.Rect{X: c.win.X, Y: got, Height: c.ph}
		if p := pillTop(newWin, c.drop); p != pillTop(c.win, c.prevDrop) {
			t.Errorf("%s: 药丸顶边从 %d 动到了 %d", c.name, pillTop(c.win, c.prevDrop), p)
		}
	}

	// 单独把那条 bug 钉住：贴顶收起时不许落到旧面板底边下面去。
	downWin := application.Rect{X: 1500, Y: 100, Height: hOf(true, false)}
	if got := anchorY(downWin, scale, dropDown, dropUp, hOf(false, false)); got != 100 {
		t.Errorf("贴顶收起把窗口顶边放到了 y=%d, 想要 100（旧实现按 y+h-ph 给 %d，药丸一跳 %d 像素）",
			got, downWin.Y+downWin.Height-hOf(false, false), downWin.Y+downWin.Height-hOf(false, false)-100)
	}
}

func TestNormalizeDrop(t *testing.T) {
	for _, s := range []string{dropUp, "", "UP", "Down", "  "} {
		if got := normalizeDrop(s); got != dropUp {
			t.Errorf("normalizeDrop(%q) = %q, 想要 %q", s, got, dropUp)
		}
	}
	if got := normalizeDrop(dropDown); got != dropDown {
		t.Errorf("normalizeDrop(down) = %q", got)
	}
}

func TestClamp(t *testing.T) {
	if got := clamp(-5, 0, 100); got != 0 {
		t.Errorf("夹下界给 %d", got)
	}
	if got := clamp(500, 0, 100); got != 100 {
		t.Errorf("夹上界给 %d", got)
	}
	// 窗口比工作区还宽时 hi<lo：这时夹到 lo，别让 min/max 打架给出一个负宽度的位置。
	if got := clamp(50, 10, -20); got != 10 {
		t.Errorf("夹不住的时候给 %d, 想要 10", got)
	}
}

// —— 日志那两颗按钮 ——

// newTestUI 立一套各就各位的 UI：hub 的数据目录、监控/转移目录、运行日志全在临时目录里，
// 所以这些测试一个真文件也不会碰。
func newTestUI(t *testing.T) (*UI, *Hub, *logSink) {
	t.Helper()
	set := readySettings(t.TempDir(), t.TempDir())
	hub, err := newHub(t.TempDir(), quietLog(), set)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(hub.Shutdown)
	sink, log, _ := newTestSink(t)
	return newUI(hub, log, "test", sink), hub, sink
}

// 一次都还没搬过时「查看日志」不是故障：Go 那边既不建文件也不报错，
// 前端拿到 opened=false 才会写「还没有搬运记录」而不是行内红字。
// 这条测试也顺保证它不会去开记事本 —— 存在的文件才会被交给 notepad。
func TestOpenLogAnswersInsteadOfFailingWhenNoMovesYet(t *testing.T) {
	u, _, _ := newTestUI(t)
	r := u.OpenLog()
	if !r.OK {
		t.Fatalf("该回一个 ok 信封，拿到 %+v", r)
	}
	got, cast := r.Data.(logTarget)
	if !cast {
		t.Fatalf("负载形状变了：%T", r.Data)
	}
	if got.Opened {
		t.Error("文件还没生成，opened 不该是 true")
	}
	if base := filepath.Base(got.Path); base != "moves.jsonl" {
		t.Errorf("报出来的是 %q，这颗按钮指的该是搬运流水", got.Path)
	}
}

// 清空一次：流水空了、运行日志从开头重写、崩溃账本一笔不少。
// 这三条是同一次点击的后果，所以放在一个测试里 —— 拆开就钉不住「谁被清了、谁不许被清」。
func TestClearLogEmptiesBothAndSparesTheLedger(t *testing.T) {
	u, hub, sink := newTestUI(t)
	now := time.Now()
	if err := hub.jr.LogMove(journal.Move{TS: now, Src: `C:\a.zip`, Dst: `D:\a.zip`, Outcome: journal.OutcomeMoved}); err != nil {
		t.Fatal(err)
	}
	if err := hub.jr.Append(journal.Record{ID: "id-1", TS: now, State: journal.StateCopied, Src: `C:\a.zip`, SrcSize: 10}); err != nil {
		t.Fatal(err)
	}
	u.log.Info("清空之前的一句")
	pendingPath := filepath.Join(hub.jr.Dir(), "pending.jsonl")
	beforeLedger := string(readFile(t, pendingPath))

	r := u.ClearLog()
	if !r.OK {
		t.Fatalf("清空失败：%+v", r.Err)
	}
	if raw := readFile(t, hub.jr.MovesPath()); len(raw) != 0 {
		t.Errorf("流水没清空，还剩 %d 字节", len(raw))
	}
	// 截断而不是删除：路径还在，「查看日志」下一按开得出了一个空文件。
	if _, exists := hub.MovesLog(); !exists {
		t.Error("清空之后 moves.jsonl 的路径本身也没了")
	}
	logNow := string(readFile(t, sink.Path()))
	if strings.Contains(logNow, "清空之前的一句") {
		t.Errorf("运行日志没换底：%s", logNow)
	}
	if !strings.Contains(logNow, "日志已清空") {
		t.Error("换底之后没在新日志的第一行写下这次清空")
	}
	if after := string(readFile(t, pendingPath)); after != beforeLedger {
		t.Errorf("清空日志动了崩溃账本：%q → %q", beforeLedger, after)
	}
	if recs, _, err := hub.jr.Pending(); err != nil || len(recs) != 1 {
		t.Errorf("账本里那一笔读不出来了：%d 条, %v", len(recs), err)
	}
	cleared, cast := r.Data.(clearedLog)
	if !cast || cleared.Moves != hub.jr.MovesPath() || cleared.Runtime != sink.Path() {
		t.Errorf("答复里两个路径不对：%+v", r.Data)
	}
}

// 第二次启动可以比页面的监听还早到（实测只差 124ms）：那一句不能只靠推，推丢了就永远没人说。
// 所以账一直欠到页面自己来取；取是一次性的，第二次问就该答「没欠的了」，
// 否则每重读一次都要补一句「已有实例在运行」。
// 启动参数没轮上那句走同一个口子：启动那一次页面必然还没挂上监听，推等于没说。
func TestHandoffIsOwedUntilThePageAsks(t *testing.T) {
	u := &UI{log: slog.New(slog.DiscardHandler)}
	u.announceHandoff() // win 是 nil：这一句推不出去，账必须还在
	if pending, _ := owed(t, u); !pending {
		t.Error("页面没听见的那一次交接该报 pending，让它自己补上")
	}
	if pending, _ := owed(t, u); pending {
		t.Error("同一句不该欠第二次")
	}
	u.announceHandoff()
	if pending, _ := owed(t, u); !pending {
		t.Error("第二次启动又欠上了，该重新报一次")
	}
}

// 参数被作废那一句同样要欠到页面来取，而且不能把交接那一笔顶掉（两句都要说）。
func TestLaunchNoticeIsOwedOnce(t *testing.T) {
	u := &UI{log: slog.New(slog.DiscardHandler)}
	if pending, notice := owed(t, u); pending || notice != "" {
		t.Errorf("没发生过任何事就该两手空空：%v %q", pending, notice)
	}
	const msg = "--lang 没用上：界面语言只认 auto、zh、en"
	u.oweNotice(msg)
	u.announceHandoff()
	pending, notice := owed(t, u)
	if !pending || notice != msg {
		t.Errorf("交接与参数两句都要带走，结果 pending=%v notice=%q", pending, notice)
	}
	if pending, notice := owed(t, u); pending || notice != "" {
		t.Errorf("取到就该清账，第二次问不该再答出来：%v %q", pending, notice)
	}
	u.oweNotice("") // 没话说的轮次不许把上一句的账擦掉，也不许凭空欠出一句
	if _, notice := owed(t, u); notice != "" {
		t.Errorf("空串不该欠出东西，结果 %q", notice)
	}
}

// owed 取一次账，顺带钉住信封的形状：页面按这两个键名读，改了名这里先红。
func owed(t *testing.T, u *UI) (bool, string) {
	t.Helper()
	r := u.TakeHandoff()
	if !r.OK {
		t.Fatalf("答复连信封都没了：%+v", r)
	}
	d, ok := r.Data.(map[string]any)
	if !ok {
		t.Fatalf("信封形状变了：%T", r.Data)
	}
	pending, _ := d["pending"].(bool)
	notice, _ := d["notice"].(string)
	return pending, notice
}
