package main

import (
	"log/slog"
	"math"
	"os/exec"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"download2/internal/apperr"
	"download2/internal/config"
)

// UI 是全应用唯一绑给前端的类型，且声明在 package main。
//
// 为什么只此一个：绑定键是 PkgPath.TypeName.MethodName（bindings.go 里建键、Get 精确匹配），
// FQN 含包名这件事意味着多绑一个类型就多一条会变的字符串。绑成一个，
// 「叫不动后端」的排查就从「翻遍所有包」缩成「核对一个前缀」。
type UI struct {
	app     *application.App
	win     *application.WebviewWindow
	hub     *Hub
	log     *slog.Logger
	version string
	// logs 是运行日志那个文件的把手。「清空日志」必须换掉它的底，
	// 而这件事只有拿着句柄的一方做得对（理由见 main.go 的 logSink）。
	logs *logSink

	mu       sync.Mutex
	expanded bool
	toast    bool
	// drop 是面板往哪边长（up/down），在「收起→展开」那一刻定，之后不重算。
	// 药丸被拖到别处再展开才会重新翻一次方向。
	// 收起时复位成 up：那样「窗口底边 = 药丸底边」这条约定在收起态永远成立，
	// pillRect 才敢从窗口矩形里直接切出药丸那一段。
	drop   string
	placed bool // Restore 只真正摆一次位：后备定时器和 RuntimeReady 会各来一次
	// handoff 是欠页面的那一句：第二次启动来了，但页面那一次没听见。
	handoff bool
	// notice 是欠页面的另一句：某条启动参数没轮上，而命令行那边没有控制台可看。
	// 与 handoff 同一笔账、同一个出口（TakeHandoff），因为启动那一次页面必然还没挂上监听。
	notice string
}

// Restore 按锚点把药丸放到工作区右下角。RuntimeReady 与一个定时器都会调它，
// 谁先到算谁的 —— 位置只在第一次有意义，之后一切都跟着用户拖。
func (u *UI) Restore() {
	u.mu.Lock()
	if u.placed {
		u.mu.Unlock()
		return
	}
	u.placed = true
	u.mu.Unlock()

	// anchored 那条支路不看 prevDrop（还没有可信的当前几何），这里照实传当前方向。
	u.applyGeometry(true, u.dropDir())
	u.emitMode() // 页面刚活，把 Go 认定的形态推过去，DOM 照着画
}

// bringToFront 给「双击了第二个实例」用：把已有药丸露出来，而不是再挂一颗。
func (u *UI) bringToFront() {
	if u.win == nil {
		return
	}
	u.win.Show()
	u.win.Focus()
}

// announceHandoff 对界面交代一句：刚才那一次启动没有多挂一颗药丸，它把参数交给了正在跑的这一颗。
//
// 不发这一句，交接在屏幕上就只剩「配色当场变了」或者「界面当场换成英文」—— 那和
// 「旧进程被新进程顶掉了」长得一模一样，本机真就这么误读过。
//
// 推一条事件，但**不**拿它当账：实测第二次启动只差 124ms 时，事件按 wails 的说法发了出去
// （日志里有那一句），药丸却仍是上一轮的样子 —— 页面那头的监听还没挂上，这一条就掉在水里了。
// 所以这笔账一直欠到页面自己来取（TakeHandoff），首屏那次取就把错过的补回来。
func (u *UI) announceHandoff() {
	u.mu.Lock()
	u.handoff = true
	u.mu.Unlock()
	u.log.Info("第二次启动把参数交给了这一颗，欠页面一句")
	if u.win != nil {
		u.win.EmitEvent("download2:secondLaunch")
	}
}

// oweNotice 把「某条启动参数没轮上」这句话欠给界面。
//
// 只记日志不算说过：`-H windowsgui` 之后没有控制台，打错一个词的人看到的只是
// 「药丸还是那副样子」，与「参数根本没传进去」长得一模一样。话在这里存着，
// 由页面自己来取（TakeHandoff）—— 启动那一次它必然还没挂上监听，推是推不到的。
func (u *UI) oweNotice(msg string) {
	if msg == "" {
		return
	}
	u.mu.Lock()
	u.notice = msg
	u.mu.Unlock()
	u.log.Info("这句话欠界面", "notice", msg)
}

// TakeHandoff 是页面来结那两笔账：有一次交接它没听见就答 pending，有一条参数没轮上
// 就答 notice，两个都取到即清。首屏加载完取一次，收到事件时再取一次 ——
// 两条路只会有一条真拿到，所以不会重复说。
func (u *UI) TakeHandoff() Result {
	u.mu.Lock()
	pending := u.handoff
	notice := u.notice
	u.handoff = false
	u.notice = ""
	u.mu.Unlock()
	return okResult(map[string]any{"pending": pending, "notice": notice})
}

// 药丸、面板、提示条的尺寸，单位 DIP（不是物理像素）。
//
// frontend/js/devstub.js 里的 layout 必须与这里一致：DOM 侧的高度变量是 Go 通过
// state.layout 写下去的，两处不一致的结果是「浏览器里看着对、真窗口里裁字」。
//
// panelH 是三个 Tab 里最高的那一栏定下的，不是凑的：面板高度由 Go 算，
// 内容超高时面板自己滚。改它要同时改 devstub.js 的 layout 和 TestSizeDIP 的期望
// （两处钉的是同一个数）。
const (
	pillW, pillH   = 236, 40
	panelW, panelH = 360, 340
	toastH         = 52
	stackGap       = 6 // #shell 的 flex gap：三块之间各留一条
)

// 面板往哪边长。值直接进 DOM 的 data-drop，也进 download2:mode 的负载，
// 所以两边共用这两个词，不再各自翻译一次。
const (
	dropUp   = "up"   // 默认：药丸在下方，面板往上长
	dropDown = "down" // 药丸贴近屏幕顶：整叠东西翻过来，面板排在药丸下面
)

func newUI(hub *Hub, log *slog.Logger, version string, logs *logSink) *UI {
	return &UI{hub: hub, log: log, version: version, drop: dropUp, logs: logs}
}

// —— Result 信封 ——
//
// 绑定方法一律不返回 error：v3 里返回 error 会让前端的 Promise 直接 reject，
// 而 reject 到不了我们的中文文案 —— 前端只能笼统报「叫不动后端」，
// 把「转移目录不能设在监控目录里面」这种明确判决降级成了传输故障。
type Result struct {
	OK   bool      `json:"ok"`
	Data any       `json:"data"`
	Err  *errorDTO `json:"err"`
}

type errorDTO struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func okResult(data any) Result { return Result{OK: true, Data: data} }

func errResult(err error) Result {
	e := apperr.Of(err)
	msg := e.Msg
	if msg == "" {
		msg = "出错了，详情在日志里。"
	}
	return Result{OK: false, Err: &errorDTO{Code: string(e.Code), Msg: msg}}
}

// —— 几何 ——

// sizeDIP 给定形态下窗口应有多大（逻辑像素）。
//
// 提示条算进窗口高度而不是浮在窗口外面：宿主矩形始终是实心的，
// 圆角只画在 CSS 里，这样 WebView2 的子 HWND 覆盖每一个可命中的像素
// —— 源码点名的 #4871（frameless+transparent 时未覆盖处点击穿到桌面）就是这么绕开的。
func sizeDIP(expanded, toast bool) (int, int) {
	w, h, parts := pillW, pillH, 1
	if expanded {
		w, h, parts = panelW, pillH+panelH, parts+1
	}
	if toast {
		h += toastH
		parts++
	}
	return w, h + stackGap*(parts-1)
}

func dipToPhys(v int, scale float64) int { return int(math.Round(float64(v) * scale)) }

// screen 读窗口所在屏幕。失败时返回 nil，调用方放弃夹取而不是猜一个工作区。
func (u *UI) screen() *application.Screen {
	if u.win == nil {
		return nil
	}
	s, err := u.win.GetScreen()
	if err != nil {
		u.log.Warn("读不到窗口所在屏幕", "err", err)
		return nil
	}
	if s == nil || s.PhysicalWorkArea.Width <= 0 || s.PhysicalWorkArea.Height <= 0 {
		u.log.Warn("屏幕工作区读出来是空的", "screen", s)
		return nil
	}
	return s
}

// scaleOf：本机实测 150% 缩放时 logical×1.5=physical（320×260 → 480×390）。
// 读不到缩放就按 100% 算 —— 宁可药丸小一点，也不要一个猜出来的 1.5 把窗口推出屏幕。
func scaleOf(s *application.Screen) float64 {
	if s == nil || float64(s.ScaleFactor) <= 0 {
		return 1
	}
	return float64(s.ScaleFactor)
}

// decideDrop 回答「面板这次往哪边长」。纯函数：进来的全是物理像素矩形。
//
// 判据是「往上放不放得下」而不是「药丸在上半屏还是下半屏」：后者会在中间那条线上
// 无谓地翻一次，而真正让人恼火的是展开之后面板被屏幕切掉一截。
// 两边都放不下时选余量更大的一边 —— 之后 applyGeometry 还会把它夹进工作区，
// 那时药丸本身可能被挪一下：这是「面板完整可见」优先于「药丸一动不动」的取舍。
func decideDrop(pill, wa application.Rect, ph int) string {
	if wa.Width <= 0 || wa.Height <= 0 {
		return dropUp // 读不到工作区就按老规矩往上长：猜一个方向可能把面板整个推出屏幕
	}
	up := pill.Y + pill.Height - wa.Y
	down := wa.Y + wa.Height - pill.Y
	switch {
	case up >= ph:
		return dropUp
	case down >= ph:
		return dropDown
	case down > up:
		return dropDown
	default:
		return dropUp
	}
}

// normalizeDrop 把任何不是 "down" 的值都当成 "up"，包括零值 ""。
// 几何、DOM 负载和 state 三处都要归一，各写一遍迟早漏一处 —— 漏掉的表现是
// DOM 拿到 drop:"" 时不翻转，而窗口已经往下长了。
func normalizeDrop(d string) string {
	if d == dropDown {
		return dropDown
	}
	return dropUp
}

// shape 一次把当前形态读全。分开三个 getter 会让下面的几何读到
// 「expanded 是新的、drop 是旧的」这种半更新状态，而方向恰恰决定了锚哪条边。
func (u *UI) shape() (expanded bool, toast bool, drop string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.expanded, u.toast, u.drop
}

// pillRect 从窗口矩形里切出药丸实际占的那一段。
//
// 收起态整叠东西里药丸永远贴底（提示条排在它上面，方向收起时复位成 up），
// 所以窗口底边就是药丸底边。不切这一刀的话，提示条正亮着时窗口顶边比药丸顶边
// 高出一条 toast —— decideDrop 会以为下面比上面宽裕，翻错方向。
func pillRect(win application.Rect, scale float64) application.Rect {
	pw, ph := dipToPhys(pillW, scale), dipToPhys(pillH, scale)
	return application.Rect{X: win.X + win.Width - pw, Y: win.Y + win.Height - ph, Width: pw, Height: ph}
}

// dropForExpand 回答「这次展开往哪边长」。只在收起→展开那一刻调用。
//
// 任何一处读不到就退回 up：up 是这一版之前的唯一行为，猜错方向的表现是
// 面板被屏幕切一截，而猜本身不该改变药丸的位置。
func (u *UI) dropForExpand() string {
	if u.win == nil {
		return dropUp
	}
	sc := u.screen()
	if sc == nil {
		return dropUp
	}
	pb := u.win.PhysicalBounds()
	if pb.Width == 0 || pb.Height == 0 {
		return dropUp
	}
	scale := scaleOf(sc)
	_, toast, _ := u.shape()
	_, dh := sizeDIP(true, toast)
	return decideDrop(pillRect(pb, scale), sc.PhysicalWorkArea, dipToPhys(dh, scale))
}

// anchorY 给出重排后窗口的顶边，让**药丸在屏幕上的位置不变**。
//
// 锚哪条边要分两次看，因为一次形态切换里新旧方向的含义不同：
//   - 旧矩形里的药丸边由 prevDrop 定 —— up 时它贴窗口底边，down 时贴顶边。
//     提示条和面板永远排在药丸另一侧，所以不必知道它们当时占多高。
//   - 新窗口要锚的边由 drop 定 —— up 让窗口底边接住药丸底边，down 让顶边接住药丸顶边。
//
// 只用新方向锚旧矩形就是「关掉配置页、工具条自己跳走」的根因：SetExpanded(false) 会
// 先把 drop 复位成 up，而那一刻窗口还按 down 排着（药丸在顶边），按 `y+h` 锚等于把
// 药丸推到旧面板底边下面去，一跳就是整整一个面板高。
func anchorY(pb application.Rect, scale float64, prevDrop, drop string, ph int) int {
	pillPhys := dipToPhys(pillH, scale)
	pillBottom := pb.Y + pb.Height
	if normalizeDrop(prevDrop) == dropDown {
		pillBottom = pb.Y + pillPhys
	}
	if normalizeDrop(drop) == dropDown {
		return pillBottom - pillPhys
	}
	return pillBottom - ph
}

// applyGeometry 按当前形态重排窗口，**药丸那条边锚定、右下角对齐**：
// 药丸不动，面板和提示条往空的那侧长；宽度变大时向左长。
//
// prevDrop 是「当前 PhysicalBounds 所按的那个方向」，必须由调用方在原子里改状态之前取走。
//
// anchored=true 用于第一次摆位（那时还没有「当前右下角」可读：
// 建窗后立刻读几何全是 0，这是 T0 实测到的），届时按设置里记的
// 「到工作区右/下边的距离」落位 —— 换显示器、改分辨率都靠它。
func (u *UI) applyGeometry(anchored bool, prevDrop string) {
	if u.win == nil {
		return
	}
	expanded, toast, drop := u.shape()
	drop = normalizeDrop(drop)

	s := u.hub.cur()
	sc := u.screen()
	scale := scaleOf(sc)
	dw, dh := sizeDIP(expanded, toast)
	pw, ph := dipToPhys(dw, scale), dipToPhys(dh, scale)

	pb := u.win.PhysicalBounds()
	x, y := pb.X, pb.Y
	if anchored || pb.Width == 0 || pb.Height == 0 {
		// 没有可信的当前几何：按锚点放。没有可信的工作区：那就别挪。
		// 此刻一定是收起态（Restore 只在启动那一次跑），窗口就是药丸本身，
		// 所以「底边锚定」对两种方向都成立。
		if sc == nil {
			return
		}
		wa := sc.PhysicalWorkArea
		x = wa.X + wa.Width - s.Anchor.RightInset - pw
		y = wa.Y + wa.Height - s.Anchor.BottomInset - ph
	} else {
		// 药丸那条边是锚：见 anchorY —— 锚旧矩形的哪条边由 prevDrop 定，新窗口锚哪条边由 drop 定。
		// 两种模式下药丸的可见位置都一动不动 —— 这是整个几何约定的目的。
		x = pb.X + pb.Width - pw
		y = anchorY(pb, scale, prevDrop, drop, ph)
		if sc != nil {
			wa := sc.PhysicalWorkArea
			x = clamp(x, wa.X, wa.X+wa.Width-pw)
			y = clamp(y, wa.Y, wa.Y+wa.Height-ph)
		}
	}
	u.win.SetPhysicalBounds(application.Rect{X: x, Y: y, Width: pw, Height: ph})
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// saveAnchor 记下窗口到工作区右/下边的物理像素距离。
//
// 值没变就不写盘：我们每次自己改尺寸都会惹出一条 WindowDidMove，
// 而底边锚定的那次改变换不了那两个距离 —— 于是「展开一次面板 = 一次没意义的写文件」。
func (u *UI) saveAnchor() {
	if u.win == nil {
		return
	}
	pb := u.win.PhysicalBounds()
	if pb.Width == 0 || pb.Height == 0 {
		return
	}
	sc := u.screen()
	if sc == nil {
		return
	}
	wa := sc.PhysicalWorkArea
	next := config.Anchor{
		Expanded:    u.isExpanded(),
		RightInset:  wa.X + wa.Width - (pb.X + pb.Width),
		BottomInset: wa.Y + wa.Height - (pb.Y + pb.Height),
	}
	if u.hub.cur().Anchor == next {
		return
	}
	if err := u.hub.SetAnchor(next); err != nil {
		u.log.Warn("窗口位置没记住", "err", err)
	}
}

func (u *UI) isExpanded() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.expanded
}

func (u *UI) flags() uiFlags {
	expanded, toast, drop := u.shape()
	st := u.hub.Status()
	return uiFlags{Expanded: expanded, Drop: normalizeDrop(drop), ToastOn: toast, Watching: st.Watching}
}

// —— 绑定方法：FQN 全是 main.UI.<方法名> ——

type uiFlags struct {
	Expanded bool `json:"expanded"`
	// Drop 是面板往哪边长（"up"/"down"）。DOM 靠它翻 #shell 的排列顺序，
	// 少了这个字段窗口几何和页面内容会各翻各的。
	Drop     string `json:"drop"`
	ToastOn  bool   `json:"toastOn"`
	Watching bool   `json:"watching"`
}

type layoutBox struct {
	PillW  int `json:"pillW"`
	PillH  int `json:"pillH"`
	PanelW int `json:"panelW"`
	PanelH int `json:"panelH"`
	ToastH int `json:"toastH"`
}

type statePayload struct {
	Settings config.Settings `json:"settings"`
	UI       uiFlags         `json:"ui"`
	Layout   layoutBox       `json:"layout"`
	Status   status          `json:"status"`
	Version  string          `json:"version"`
	Backend  string          `json:"backend"`
}

// State 是前端唯一的真相入口：设置、形态、尺寸、监控状态一次给全。
//
// 尺寸由 Go 报给 DOM（layout），DOM 不自算窗口该多高 —— 缩放失败或两边各算一套时，
// 「窗口比内容大一块透明区」会表现为一块点不动的死区，那种 bug 在界面里根本读不出来。
func (u *UI) State() Result {
	s := u.hub.cur()
	return okResult(statePayload{
		Settings: s,
		UI:       u.flags(),
		Layout:   layoutBox{PillW: pillW, PillH: pillH, PanelW: panelW, PanelH: panelH, ToastH: toastH},
		Status:   u.hub.Status(),
		Version:  u.version,
		Backend:  "native",
	})
}

// SetExpanded 原地展开/收起：几何在 Go 定，DOM 由 download2:mode 事件跟。
//
// 方向只在这一刻决定：展开期间用户拖药丸会让整叠东西跟着走，如果那时重算方向，
// 面板会在眼皮底下翻个面 —— 那不是他按下的按钮要干的事，所以收起时才复位成 up。
func (u *UI) SetExpanded(v bool) Result {
	// dropForExpand / isExpanded 内部都要拿锁，所以这里绝不能持锁调它们。
	// prev 必须在改状态之前取：它是「当前窗口矩形按哪个方向排的」，几何要靠它认出药丸那条边。
	prev := u.dropDir()
	drop := prev
	switch {
	case !v:
		// 收起时复位成 up：药丸回到原来那条边上，而且「窗口底边 = 药丸底边」重新成立，
		// 下次展开才有可靠的药丸矩形可量。
		drop = dropUp
	case !u.isExpanded():
		drop = u.dropForExpand()
	}
	u.mu.Lock()
	u.expanded, u.drop = v, drop
	u.mu.Unlock()
	u.applyGeometry(false, prev)
	u.emitMode()
	return okResult(map[string]any{"expanded": v, "drop": drop})
}

func (u *UI) dropDir() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return normalizeDrop(u.drop)
}

func (u *UI) emitMode() {
	if u.win == nil {
		return
	}
	expanded, _, drop := u.shape()
	u.win.EmitEvent("download2:mode", map[string]any{"expanded": expanded, "drop": normalizeDrop(drop)})
}

// SetToast 让提示条那一条高度真实存在或真实消失。
//
// 提示画在同一个窗口里，所以它要么占一段窗口高度、要么不占；
// 「窗口一直留着那块透明区但没画东西」= 一条谁也看不见的点击死区。
func (u *UI) SetToast(v bool) Result {
	u.mu.Lock()
	u.toast = v
	u.mu.Unlock()
	// 提示条不改方向，所以 prev 就是当前方向 —— 但药丸在哪条边上仍要由它认。
	u.applyGeometry(false, u.dropDir())
	return okResult(map[string]bool{"toast": v})
}

// SetPaused 暂停/继续。暂停 = 把事件源拔掉，见 Hub.SetPaused。
func (u *UI) SetPaused(v bool) Result {
	if err := u.hub.SetPaused(v); err != nil {
		return errResult(err)
	}
	return okResult(u.hub.Status())
}

// Save 收配置页的差量。data 里回一份新的 settings 与 status，前端据此重画，
// 而不是自己拼一份「大概是这个样子」的状态。
func (u *UI) Save(d saveDTO) Result {
	if err := u.hub.Save(d); err != nil {
		return errResult(err)
	}
	return okResult(map[string]any{
		"settings": u.hub.cur(),
		"status":   u.hub.Status(),
	})
}

// BrowseTarget 开原生选目录对话框。只能在 Go 侧、同步调：
// v3 没有 JS 侧的选目录入口，而异步版本会把窗口归属丢掉。
func (u *UI) BrowseTarget() Result {
	if u.app == nil || u.win == nil {
		return errResult(apperr.New(apperr.EInternal, "窗口还没就绪，选不了目录。"))
	}
	path, err := u.app.Dialog.OpenFile().
		CanChooseDirectories(true).
		CanChooseFiles(false).
		SetTitle("选转移目录").
		AttachToWindow(u.win).
		PromptForSingleSelection()
	if err != nil {
		u.log.Error("选目录", "err", err)
		return errResult(apperr.Wrap(apperr.EFileOpFailed, "打开选目录对话框没成", err))
	}
	if path == "" {
		// 取消不是错误，但前端需要一个能显示在行内的说法；
		// 回 ok 加空路径会让输入框被清空，看起来像是取消把设置改了。
		return errResult(apperr.New(apperr.EInvalidArg, "没选目录，转移目录保持原样。"))
	}
	u.log.Info("选目录", "path", path)
	return okResult(map[string]string{"targetDir": path})
}

// OpenTargetDir 在资源管理器里打开转移目录。
//
// 用 Start 不等退出：explorer.exe 就算成功也把退出码给出非零，
// 等它就等于把一次「看一眼」报成失败。
func (u *UI) OpenTargetDir() Result {
	dir := u.hub.cur().TargetDir
	if dir == "" {
		return errResult(apperr.New(apperr.ENotConfigured, "还没设转移目录。"))
	}
	cmd := exec.Command("explorer", dir)
	if err := cmd.Start(); err != nil {
		return errResult(apperr.Wrap(apperr.EFileOpFailed, "打不开那个目录", err))
	}
	go func() { _ = cmd.Wait() }()
	return okResult(nil)
}

// OpenTodayDir 在资源管理器里打开「今天」落到的那一档目录；那一档还不存在时打开转移目录本身。
//
// 目录不存在时不建、也不算失败：「今天还没搬过东西」是一个答案，不是一次故障，
// 而为了回答它就造出一个空文件夹，等于让每一次「看一眼」都留下写盘副作用。
// 落点没了就退到转移目录（见 Hub.TodayDir 的 Opened），两处都没了才什么都不开，
// 由前端拿着 opened="" 说清楚。
func (u *UI) OpenTodayDir() Result {
	t, err := u.hub.TodayDir(time.Now())
	if err != nil {
		return errResult(err)
	}
	if t.Opened != "" {
		cmd := exec.Command("explorer", t.Opened)
		if err := cmd.Start(); err != nil {
			return errResult(apperr.Wrap(apperr.EFileOpFailed, "打不开那个目录", err))
		}
		go func() { _ = cmd.Wait() }()
	}
	return okResult(t)
}

// logTarget 是「查看日志」的答复。opened 的口径和「查看」那颗按钮一致：它说的是
// 「到底开没开成一个窗口」，前端据此决定说「已打开」还是「还没有搬运记录」。
type logTarget struct {
	Path   string `json:"path"`
	Opened bool   `json:"opened"`
}

// clearedLog 是「清空日志」的答复：两份日志各自的路径。界面上要说清清了哪两个文件，
// 否则「清空日志」按下去没反应，用户不知道它到底动没动东西。
type clearedLog struct {
	Moves   string `json:"moves"`
	Runtime string `json:"runtime"`
}

// OpenLog 用记事本打开搬运流水 moves.jsonl。
//
// 为什么是 notepad 而不是 explorer 或 cmd start：.jsonl 在本机没有文件关联
// （实测 `cmd /c assoc .jsonl` 给出空），把文件路径交给 explorer 只会换来一个
// 「你要用什么方式打开」的对话框；记事本每台 Windows 都带着，实测连带空格的路径也开得了。
// 流水还没生成时不报错、也不提前建一个空文件：那是「一次都还没搬过」，界面那句中文说得出。
func (u *UI) OpenLog() Result {
	path, exists := u.hub.MovesLog()
	if !exists {
		return okResult(logTarget{Path: path})
	}
	// 路径是 exec 的参数表交出去的，不经过 shell，所以这里不用自己加引号或转义。
	cmd := exec.Command("notepad", path)
	if err := cmd.Start(); err != nil {
		return errResult(apperr.Wrap(apperr.EFileOpFailed, "打不开记事本，流水还在原来那个文件里", err).With("path", path))
	}
	// 不等它退出：记事本关窗前那个进程一直活着，而我们只要窗口开出来（口径同 OpenTargetDir）。
	go func() { _ = cmd.Wait() }()
	return okResult(logTarget{Path: path, Opened: true})
}

// ClearLog 一次清两份：搬运流水就地截断，运行日志换底重开。按用户的要求不做二次确认。
//
// 顺序是「先流水、后运行日志」：换完底才写得进这一句「日志刚被清过」，
// 于是它正好落在一份新日志的第一行 —— 用户回头翻日志，第一句就是他自己刚干的事。
//
// pending.jsonl（崩溃账本）永远不在这条路上：清掉它等于让下一次启动对上一次崩溃失忆，
// 半搬完的文件会被当成一次新下载再搬一遍。
func (u *UI) ClearLog() Result {
	if err := u.hub.ClearMovesLog(); err != nil {
		return errResult(err)
	}
	if err := u.logs.Reset(); err != nil {
		return errResult(err)
	}
	moves, _ := u.hub.MovesLog()
	runtime := u.logs.Path()
	u.log.Info("日志已清空：搬运流水与运行日志", "moves", moves, "runtime", runtime)
	return okResult(clearedLog{Moves: moves, Runtime: runtime})
}

// SetTheme 切深色/浅色：点一下就换，即时生效并落盘。
//
// 它不走 Save：主题不是一个「等待保存的差量」，把偏好做成表单字段
// 会让用户以为不点保存就没换上。
func (u *UI) SetTheme(name string) Result {
	if err := u.hub.SetTheme(name); err != nil {
		return errResult(err)
	}
	return okResult(map[string]string{"theme": u.hub.cur().Theme})
}

// SetLang 换界面语言。和 SetTheme 一样当场落盘生效，不走 Save ——
// 语言不是一个「等待保存的差量」，把它做成表单字段会让用户以为不点保存就没换上。
//
// 回的是后端认下的那个值而不是传进来的那个：认不出的词已经被回落成 auto，
// 前端拿回值再查一次字典，两边才不会一个说中文一个说英文。
func (u *UI) SetLang(code string) Result {
	if err := u.hub.SetLang(code); err != nil {
		return errResult(err)
	}
	return okResult(map[string]string{"lang": u.hub.cur().Lang})
}

// RelocateSource 重新定位监控目录（见 Hub.Relocate：它当场生效，不等保存）。
func (u *UI) RelocateSource() Result {
	dir, err := u.hub.Relocate()
	if err != nil {
		return errResult(err)
	}
	u.hub.pushStatus("")
	return okResult(map[string]string{"sourceDir": dir})
}

// Quit 收摊。OnShutdown 与 main 都会调 Hub.Shutdown，而它只做一遍。
//
// 这里的 Reply 很可能发不出去（事件循环已经在往下走），前端对此无所谓：
// 药丸消失就是它要的结果。
func (u *UI) Quit() Result {
	if u.app == nil {
		return errResult(apperr.New(apperr.EInternal, "应用还没建起来。"))
	}
	go func() {
		time.Sleep(30 * time.Millisecond) // 让 State/status 那两条在途回复先落
		u.app.Quit()
	}()
	return okResult(nil)
}
