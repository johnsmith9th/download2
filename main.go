package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"download2/internal/apperr"
	"download2/internal/config"
)

// 嵌的是 frontend 本身而不是 frontend/dist：这台机器上 wails CLI 永不运行，
// 没有 codegen 也就没有「构建前先把前端拷过去」那一步。
// 留一个 dist 目录等于留一个「改错了文件还能跑」的入口 —— 改 src 而跑 dist，
// 看什么都对、界面上什么都不变。
//
//go:embed all:frontend
var embedded embed.FS

const (
	logName  = "download2_log.txt"
	uniqueID = "com.download2.pill"
)

// var 而不是 const：发布构建用 -ldflags "-X main.appVersion=<标签>" 把版本号盖进来，
// const 盖不动。不带 -Version 本地编时就是下面这个字面值。
var appVersion = "0.1.0"

func main() {
	// 谁是第一份要等到 application.New 才见分晓，而日志句柄是第一件事就开的。
	// 所以这里先探一眼（只问不占，见 single_instance_windows.go）：探到有人已经在跑，
	// 本轮就不清空那份共享的日志，也不假装自己要用这些参数。
	fresh := ownsSingleInstanceLock()
	log, logs := openLogger(fresh)

	var launchNotice string
	opt, oerr := parseLaunch(os.Args[1:])
	if oerr != nil {
		// 认不出的命令行不是一场故障：没有控制台就没有人能看见 usage，
		// 「参数打错了」的代价不该是「双击了没反应」。记一条日志，按设置里的值照常跑，
		// 另外把这句话欠给界面 —— 日志那一头是排查的人看的，这一头是打错字的人看的。
		log.Error("启动参数没认出来，本轮忽略", "args", os.Args[1:], "err", oerr)
		launchNotice = badLaunchNotice
	}

	dataDir, err := config.AppDataDir()
	if err != nil {
		log.Error("找不到数据目录，只能退出", "err", err)
		os.Exit(1)
	}
	store, err := config.NewStore(dataDir)
	if err != nil {
		log.Error("设置目录用不了", "err", err)
		os.Exit(1)
	}

	loaded, lerr := store.Load()
	if lerr != nil {
		// 坏档已由 config 另存，内存里用的是默认值。这里必须留话：
		// 静默按默认值跑，用户的转移目录就在他眼皮底下没了。
		log.Error("设置读不出，本轮先用默认值", "err", lerr)
	}
	set := loaded.Settings
	if set.SourceDir == "" {
		if d, derr := config.DownloadsDir(); derr == nil {
			set.SourceDir = d
		} else {
			log.Error("解析不出下载目录", "err", derr)
		}
	}

	hub, err := newHub(dataDir, log, set)
	if err != nil {
		log.Error("起不来", "err", err)
		os.Exit(1)
	}

	// 参数在 hub 建好之后才打上去：转移目录那条要拿监控目录一起算，
	// 而那份设置已经在 hub 里了。打不上去不是致命伤 —— 盘上那一份还在，
	// 本轮照搬设置里的值，只是没换成用户敲的那一个。
	//
	// 探出有人已经在跑的这一轮一条都不打：本轮活不到用得上它的地方（application.New 会把
	// 这几条转给那颗药丸然后退出），而这里每打一条都往共享日志里落一句「已在本轮生效」——
	// 那句是假话，上一次读这份日志的人正是被它带偏的。
	if fresh {
		launchNotice = joinNotice(launchNotice, applyLaunch(opt, hub, log))
	} else {
		log.Info("本轮不是唯一的一份，参数交给正在跑的那一颗", "args", os.Args[1:])
		launchNotice = "" // 这一句在本轮活不到界面前，交给那一条真在跑的药丸去说
	}

	webFS, err := fs.Sub(embedded, "frontend")
	if err != nil {
		log.Error("embed sub", "err", err)
		os.Exit(1)
	}

	ui := newUI(hub, log, appVersion, logs)

	app := application.New(application.Options{
		Name:     "Download2",
		Logger:   log,
		LogLevel: slog.LevelDebug,
		Services: []application.Service{application.NewService(ui)},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(webFS),
		},
		MarshalError: marshalErr,
		PanicHandler: func(d *application.PanicDetails) {
			// 一个绑定方法里的 bug 不该把整个应用带走（v3 会把 panic 转成 CallError），
			// 但「转成了错误」和「为什么转」必须留下痕迹，否则界面只会显示一句通用文案。
			log.Error("panic", "err", d.Error, "stack", d.StackTrace)
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: uniqueID,
			OnSecondInstanceLaunch: func(d application.SecondInstanceData) {
				// 真正的危险不是「桌面上多了一颗药丸」，而是两个实例同时监听同一个
				// 下载目录：它们会抢着改同一个文件名，一个搬成功、另一个记一次失败。
				// 所以第二个实例只负责把已有的那颗叫到前面来。
				log.Info("已经有在跑的实例，把药丸叫到前面")
				ui.bringToFront()
				// 它带进来的那几条参数是给「已经在跑的这一颗」的，不是给马上要退出的
				// 自己：参数在这里落地，第二次启动才谈得上有意义。d.Args 是整个 os.Args
				// （含 argv[0]），所以砍掉第一个再交给同一个解析函数。
				if len(d.Args) < 2 {
					return
				}
				opt, err := parseLaunch(d.Args[1:])
				var notice string
				if err != nil {
					log.Error("第二次启动的参数没认出来", "args", d.Args[1:], "err", err)
					notice = badLaunchNotice
				}
				if opt != (launch{}) {
					log.Info("第二次启动带了参数，打给已经在跑的这一颗", "args", d.Args[1:])
					notice = joinNotice(notice, applyLaunch(opt, hub, log))
				} else if notice == "" {
					return // 只是又双击了一次图标：叫到前面就够了，没有要改的、也没有要说的
				}
				if notice != "" {
					hub.pushStatus(notice)
					ui.oweNotice(notice)
				}
				// 交接必须在界面上说一句：只把配色一改，用户看到的和「旧进程被新进程顶掉了」
				// 就是同一件事（本机真就这么误读过一次）。排在 applyLaunch 之后发 ——
				// 页面收到这条会先重读一遍 State 再开口，读早了就是把上一轮的语言端出来，
				// 而它开口那句用的必须是新语言，否则中文药丸顶着一句英文说明。
				ui.announceHandoff()
			},
		},
		OnShutdown: hub.Shutdown,
	})

	ui.app = app
	ui.win = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "toolbar",
		Title:  "Download2",
		Width:  pillW,
		Height: pillH,
		// 先落在一个看得见的地方：真正的位置由锚点在工作区右下角算，
		// 而那一次改位置必须等窗口几何可读（建窗后立刻读全是 0，T0 实测）。
		X:               240,
		Y:               240,
		InitialPosition: application.WindowXY,
		Frameless:       true,
		AlwaysOnTop:     true,
		DisableResize:   true,
		// 逐像素 alpha 走的就这一条：DirectComposition + WS_EX_NOREDIRECTIONBITMAP。
		// 不用 WindowMask —— 那是 layered 窗口的另一条路，源码注释警告它的命中区
		// 不会被 SetPos 单独刷新，正好会毁掉一个会动的药丸。
		BackgroundType:   application.BackgroundTypeTransparent,
		BackgroundColour: application.NewRGBA(0, 0, 0, 0),
		DevToolsEnabled:  true,
		Windows: application.WindowsWindow{
			HiddenOnTaskbar:                   true,
			DisableIcon:                       true,
			DisableFramelessWindowDecorations: true,
			WindowDidMoveDebounceMS:           300,
		},
	})

	// 往界面推事件的口子在这里缝一次，之后 hub 不再知道窗口的存在。
	hub.emit = func(name string, payload any) { ui.win.EmitEvent(name, payload) }

	// 监控先起来，再等界面：「页面加载失败但下载照搬」是能用的程序，
	// 「界面漂亮但一个文件都不搬」不是。
	if err := hub.Start(); err != nil {
		log.Error("监控没起来，药丸会在上面说明原因", "err", err)
	}
	if loaded.CorruptPath != "" {
		hub.pushStatus("设置文件读坏了，已另存为 " + filepath.Base(loaded.CorruptPath) + "，本轮用的是默认设置。")
	}
	if launchNotice != "" {
		hub.pushStatus(launchNotice)
		ui.oweNotice(launchNotice)
	}

	// RuntimeReady 是唯一一个「页面真的活了、几何真的可读了」的时刻；
	// 400ms 那个后备是给「事件没送到但界面已经出来了」那种情况兜底 ——
	// 两条路都走 Restore，谁先到算谁的。
	ui.win.RegisterHook(events.Common.WindowRuntimeReady, func(*application.WindowEvent) { ui.Restore() })
	ui.win.RegisterHook(events.Common.WindowDidMove, func(*application.WindowEvent) { ui.saveAnchor() })
	time.AfterFunc(400*time.Millisecond, ui.Restore)

	if err := app.Run(); err != nil {
		log.Error("app.Run", "err", err)
	}
	hub.Shutdown()
	log.Info("已退出")
	_ = logs.Close()
}

// launch 是那几条「只对本次启动生效」的命令行选项；空串一律表示「没提这一条」。
type launch struct {
	destFolder string
	lang       string
	theme      string
}

// parseLaunch 读出启动参数。用 ContinueOnError 而不是 flag.Parse：-H windowsgui 之后没有
// 控制台，默认那条路是「把 usage 打到谁也看不见的 stderr，然后 exit 2」，用户看到的只是
// 双击没反应。这里把错误原样交回调用方记日志，程序照常用设置里的值起来。
// `-lang en`、`--lang en`、`--lang=en` 三种写法由 flag 包一并认。
//
// 局部变量刻意不叫 fs：那个名字在本文件里是 io/fs，`fs.Sub` 就在 main 里用着。
func parseLaunch(args []string) (launch, error) {
	var l launch
	flags := flag.NewFlagSet("download2", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&l.destFolder, "dest-folder", "", "本次运行的转移目录（不写进设置文件）")
	flags.StringVar(&l.lang, "lang", "", "本次运行的界面语言：auto、zh、en")
	flags.StringVar(&l.theme, "theme", "", "本次运行的配色：dark、light")
	err := flags.Parse(args)
	return l, err
}

// badLaunchNotice 是给「打错一个参数名的人」看的那一句。三条名字都写全，但**不**带上
// 各自认哪几个词，也不抄 flag 包那句 "flag provided but not defined: -lan" —— toast 只有
// 两行（`.t-text` 的 -webkit-line-clamp: 2，约 60 个半角位），加上就截尾；原文在日志里。
const badLaunchNotice = "启动参数没认出来，只认 --dest-folder、--lang、--theme"

// joinNotice 把两段要说的话并成一句；空的那一段不占位置，也不留一个孤零零的分号。
func joinNotice(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "；" + b
}

// applyLaunch 把这几条参数逐条打到本轮的设置上，并返回要给药丸与日志的那一句总结（没话说是
// 空串）。三条各判各的：一个值写错了不该让另外两条一起作废。
func applyLaunch(l launch, hub *Hub, log *slog.Logger) string {
	var bad []string
	for _, p := range []struct {
		flag  string
		value string
		apply func(string) error
	}{
		{"--dest-folder", l.destFolder, hub.UseDestFolder},
		{"--lang", l.lang, hub.UseLang},
		{"--theme", l.theme, hub.UseTheme},
	} {
		if p.value == "" {
			continue
		}
		if err := p.apply(p.value); err != nil {
			e := apperr.Of(err)
			log.Error("启动参数没用上，本轮仍用设置里那一份",
				"flag", p.flag, "value", p.value, "code", string(e.Code), "err", e.Err)
			bad = append(bad, p.flag+" 没用上："+e.Msg)
			continue
		}
		log.Info("启动参数已在本轮生效，不写进设置文件", "flag", p.flag, "value", p.value)
	}
	return strings.Join(bad, "；")
}

// marshalErr 给 v3 的错误序列化用：绑定方法自己不返回 error（见 Result 那段注释），
// 所以这里只剩一条路能走到 —— 传输层自己造的错误。给它一个码和一句中文，别给 Go 的原文。
func marshalErr(err error) []byte {
	e := apperr.Of(err)
	buf, merr := json.Marshal(errorDTO{Code: string(e.Code), Msg: e.Msg})
	if merr != nil {
		return nil // 返回 nil 让 wails 用它自己的默认序列化
	}
	return buf
}

// openLogger：-H windowsgui 之后没有控制台，出问题只能看 exe 旁边的日志。
// 建不出文件也照常跑（句柄留 nil，Write 把这条吃掉）：一个写不出的日志不该拦住监控本身。
//
// fresh 是「本轮探不出别的实例」，只有那种轮才清空日志重开一份。
func openLogger(fresh bool) (*slog.Logger, *logSink) {
	sink := &logSink{}
	if exe, err := os.Executable(); err == nil {
		sink.path = filepath.Join(filepath.Dir(exe), logName)
		sink.f, _ = openLogFile(sink.path, fresh)
	}
	return slog.New(slog.NewTextHandler(sink, nil)), sink
}

// openLogFile 开出 download2_log.txt 的两种姿势：本轮是唯一的一份就清空重开，否则只往里加。
//
// 为什么要分：日志路径在 exe 旁边，两份进程用的是同一个文件，而单实例判定发生得比这里晚
// （见 main 里那句 ownsSingleInstanceLock）。后起的那一份要是照 os.Create 那样 O_TRUNC 打开，
// 它就在自己退出之前把正在跑的那一份的日志从 0 抹掉 —— 排障时读到的日志会缺前半程，
// 而这正是最容易以为「旧进程被顶掉了」的那一次。
//
// 「只往里加」这件事不在这里的 flags 上，在 logSink.Write 那一次 Seek：本机实测
// Windows 上的 os.O_APPEND 不是 POSIX 那个语义（打开时把指针挪到末尾，之后每次写仍从
// 自己那个偏移走），两份进程都带 O_APPEND 照样互相盖。
func openLogFile(path string, fresh bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if fresh {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o644)
}

// logSink 是一个能换底的日志写入口：slog 的 handler 一生只认它一次，
// 而「清空运行日志」要把底下那个文件整个换掉才清得干净。
//
// 为什么不能就地 truncate 那个文件：它是本进程自己开着的，SetEndOfFile 不会把句柄里的
// 写位置挪回 0 —— 下一条日志从老偏移写回去，前面垫一串 NUL，于是「清空」之后打开它看到的是
// 一段空白加半句话。为什么也不能只 DeleteFile：句柄会进入 delete-pending，文件从目录里消失
// 而写还在继续，等于运行日志从此静默，真出问题时无声无息。
type logSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return len(p), nil
	}
	// 每写一句之前都追到文件末尾，这才是「只往里加」。见 openLogFile 里那段实测：
	// Windows 上靠 os.O_APPEND 得不到这个语义，而这份文件可能有第二份进程也在写。
	if _, err := s.f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	if _, err := s.f.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Path 是运行日志在磁盘上的位置；解析不出 exe 位置时为空。
func (s *logSink) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Reset 关掉句柄、删掉文件、重开一个空的。
//
// 删不掉不当失败：紧接着那次打开带 O_TRUNC，落到的终态同样是「一份空日志」，
// 而用户按这一下要的就是那个终态，不是中间某一步的退出码。
// 换底这几微秒里别的线程写的日志会被丢掉 —— 一句日志换一份能清干净的运行日志，值。
func (s *logSink) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return apperr.New(apperr.EFileOpFailed, "找不到程序自己的位置，运行日志没地方放。")
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	_ = os.Remove(s.path)
	f, err := openLogFile(s.path, true)
	if err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "运行日志清不掉，原来那份还在", err).With("path", s.path)
	}
	s.f = f
	return nil
}

func (s *logSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
