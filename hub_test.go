package main

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"download2/internal/apperr"
	"download2/internal/config"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func readySettings(src, dst string) config.Settings {
	s := config.Default()
	s.SourceDir = src
	s.TargetDir = dst
	return s
}

func dtoFor(s config.Settings) saveDTO {
	return saveDTO{
		TargetDir:        s.TargetDir,
		DateSubdirs:      s.DateSubdirs,
		ToastEnabled:     s.ToastEnabled,
		ToastSeconds:     s.ToastSeconds,
		QuietMs:          s.QuietMs,
		MaxSettleMs:      s.MaxSettleMs,
		TargetDirPresets: s.TargetDirPresets,
	}
}

func TestErrResultAlwaysGivesACodeAndChinese(t *testing.T) {
	r := errResult(apperr.New(apperr.EPathForbidden, "转移目录不能就是监控目录本身"))
	if r.OK || r.Err == nil {
		t.Fatalf("该是个失败信封，拿到的 %+v", r)
	}
	if r.Err.Code != string(apperr.EPathForbidden) || r.Err.Msg == "" {
		t.Errorf("码或文案不对：%+v", r.Err)
	}
	// 没包装过的错误一律 E_INTERNAL + 一句中文：Go 的原始报错贴到界面上会撑坏面板。
	r2 := errResult(errors.New("CreateFile whatever"))
	if r2.Err.Code != string(apperr.EInternal) {
		t.Errorf("未包装的错误给出 %s", r2.Err.Code)
	}
	// 码有了但文案空着：界面会显示一片空白，看起来像「保存成功但没反馈」。
	r3 := errResult(apperr.New(apperr.EInternal, ""))
	if r3.Err.Msg == "" {
		t.Error("空文案没兜底")
	}
}

// 保存只允许改面板上那几项：其余字段必须由当前设置继承。
// 这条一旦失守，表现是「点了一下保存，转移目录还在，但监控目录没了」。
func TestSavePreservesWhatThePanelDoesNotSend(t *testing.T) {
	set := readySettings(t.TempDir(), t.TempDir())
	set.Paused = true
	set.Anchor = config.Anchor{Expanded: true, RightInset: 11, BottomInset: 22}
	hub, err := newHub(t.TempDir(), quietLog(), set)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	next := set
	next.ToastSeconds = 9
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := hub.cur()
	if got.SourceDir != set.SourceDir || got.Paused != set.Paused || got.Anchor != set.Anchor {
		t.Errorf("保存把没送的字段洗掉了：source=%q paused=%v anchor=%+v", got.SourceDir, got.Paused, got.Anchor)
	}
	if got.ToastSeconds != 9 {
		t.Errorf("ToastSeconds 还是 %d", got.ToastSeconds)
	}
	// 落盘的那一份也要一致：内存对了盘上不对，下次启动就回到旧值。
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.ToastSeconds != 9 || loaded.Settings.SourceDir != set.SourceDir {
		t.Errorf("盘上的设置是 %+v", loaded.Settings)
	}
	if !loaded.FromDisk {
		t.Error("设置文件压根没写出来")
	}
}

// 改个提示秒数不该让监控断一秒：机器没换 = 监听没重建 = 观察中的状态还在。
func TestSaveDoesNotRebuildForNonRuntimeFields(t *testing.T) {
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	before := hub.machine.Load()
	next := hub.cur()
	next.ToastSeconds = 6
	next.DateSubdirs = !next.DateSubdirs // 只影响落点，不动判定
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if hub.machine.Load() != before {
		t.Error("只改了提示秒数和子目录，判定器却被重建了")
	}
}

func TestSaveRebuildsWhenDetectionChanged(t *testing.T) {
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	before := hub.machine.Load()
	next := hub.cur()
	next.QuietMs = 2_000
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if hub.machine.Load() == before {
		t.Error("静置阈值改了但判定器没重建 —— 那新阈值永远不会生效")
	}
}

// 校验不过就不该有任何东西落到盘上：一半生效的设置比一个明确的失败难查得多。
func TestRejectedSaveChangesNothing(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	// 先存一份好的：没有文件的时候「盘上没变」是空话，那句断言要落在一份真实存在的旧设置上。
	if err := hub.Save(dtoFor(hub.cur())); err != nil {
		t.Fatalf("先存一份好设置: %v", err)
	}
	kept := hub.machine.Load()

	bad := hub.cur()
	bad.TargetDir = src // 转移目录落在监控目录里：自搬环
	bad.QuietMs = 90
	if err := hub.Save(dtoFor(bad)); err == nil {
		t.Fatal("这种设置居然存下来了")
	} else if apperr.Of(err).Code != apperr.EPathForbidden {
		t.Errorf("给出的码是 %s, 想要 E_PATH_FORBIDDEN", apperr.Of(err).Code)
	}
	if hub.cur().TargetDir != dst {
		t.Errorf("内存里的转移目录被改成了 %q", hub.cur().TargetDir)
	}
	loaded, lerr := hub.store.Load()
	if lerr != nil {
		t.Fatalf("回读设置: %v", lerr)
	}
	if loaded.Settings.TargetDir != dst {
		t.Errorf("盘上的转移目录被改成了 %q", loaded.Settings.TargetDir)
	}
	if hub.machine.Load() != kept {
		t.Error("被拒绝的保存还重建了判定器")
	}
}

// 没配转移目录时连监听都不开：药丸活着、上面说清为什么没在监控，
// 而账本里一行也不会多。
func TestStartWithoutTargetDoesNotWatch(t *testing.T) {
	src := t.TempDir()
	s := config.Default()
	s.SourceDir = src
	s.TargetDir = ""
	hub, err := newHub(t.TempDir(), quietLog(), s)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	if err := hub.Start(); err != nil {
		t.Fatalf("没配转移目录不是监听故障，Start 不该报错：%v", err)
	}
	if hub.isRunning() {
		t.Error("还没配转移目录就开监听了")
	}
	if got := hub.Status().Reason; got != notConfigured {
		t.Errorf("药丸上没说清原因，reason=%q", got)
	}

	// 配上之后必须真的动起来，否则「先不监控」会变成一个永远醒不过来的状态。
	next := hub.cur()
	next.TargetDir = t.TempDir()
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hub.isRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.isRunning() {
		t.Error("配好转移目录之后监听仍然没起来")
	}
	if hub.Status().Reason != "" {
		t.Errorf("已经监控了还挂着原因：%q", hub.Status().Reason)
	}
}

// --dest-folder 换的是本轮的落点：内存与药丸上的 status 都跟着走，settings.json 一个字不变。
// 这条一旦失守，表现是「用完这一次临时目录，拖一下药丸它就变成永久设置了」。
func TestDestFolderAppliesForThisRunOnly(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	// 参数指一个还不存在的目录：命令行上没有「选目录」那颗按钮，建出来才是这套用法的常态。
	run := filepath.Join(t.TempDir(), "本轮专用")
	if err := hub.UseDestFolder(run); err != nil {
		t.Fatalf("UseDestFolder: %v", err)
	}
	if st, serr := os.Stat(run); serr != nil || !st.IsDir() {
		t.Errorf("参数指的目录没被建出来：%v", serr)
	}
	if hub.cur().TargetDir != run {
		t.Errorf("内存里的落点还是 %q", hub.cur().TargetDir)
	}
	hub.pushStatus("")
	if got := hub.Status().TargetDir; got != run {
		t.Errorf("药丸 tooltip 说的落点是 %q，本轮真正搬去的是 %q", got, run)
	}

	// 拖一次药丸：这是用户没有说过任何一句关于转移目录的话的一次写盘。
	if err := hub.SetAnchor(config.Anchor{RightInset: 7, BottomInset: 8}); err != nil {
		t.Fatalf("SetAnchor: %v", err)
	}
	loaded, lerr := hub.store.Load()
	if lerr != nil {
		t.Fatalf("回读设置: %v", lerr)
	}
	if loaded.Settings.TargetDir != dst {
		t.Errorf("一次性的参数跟着锚点落盘了：盘上现在是 %q", loaded.Settings.TargetDir)
	}
	if loaded.Settings.Anchor.RightInset != 7 {
		t.Errorf("锚点没存住：%+v", loaded.Settings.Anchor)
	}
	if hub.cur().TargetDir != run {
		t.Errorf("自动落盘把本轮的覆盖也洗掉了：现在内存里是 %q", hub.cur().TargetDir)
	}
}

// 一个会形成自搬环的临时落点和别的自搬环一样危险：拦下来，且什么都不改。
func TestDestFolderRejectedChangesNothing(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	inside := filepath.Join(src, "里面")
	err = hub.UseDestFolder(inside)
	if err == nil {
		t.Fatal("转移目录落在监控目录里面还收下了")
	}
	if code := apperr.Of(err).Code; code != apperr.EPathForbidden {
		t.Errorf("给出的码是 %s, 想要 E_PATH_FORBIDDEN", code)
	}
	if hub.cur().TargetDir != dst {
		t.Errorf("被拒绝的参数改了内存里的目录：%q", hub.cur().TargetDir)
	}
	if _, serr := os.Stat(inside); serr == nil {
		t.Error("校验没过却把目录建出来了")
	}
}

// 面板没有保存按钮：改任何一个控件都会把「当前显示的落点」原样送回来。
// 送回来的还是启动参数那一份 → 盘上继续存原本那份；送回来的是别的目录 → 用户重选了永久落点，
// 参数那份就此转正。
func TestPanelSaveDuringDestFolder(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	run := t.TempDir()
	if err := hub.UseDestFolder(run); err != nil {
		t.Fatalf("UseDestFolder: %v", err)
	}

	// 只改提示秒数：DTO 里的 targetDir 是本轮那份（前端回显），它不该变成永久设置。
	toastOnly := hub.cur()
	toastOnly.ToastSeconds = 7
	if err := hub.Save(dtoFor(toastOnly)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.ToastSeconds != 7 {
		t.Errorf("提示秒数没存住：%d", loaded.Settings.ToastSeconds)
	}
	if loaded.Settings.TargetDir != dst {
		t.Errorf("改一个跟目录无关的控件，就把一次性参数存成了永久设置：%q", loaded.Settings.TargetDir)
	}
	if hub.cur().TargetDir != run {
		t.Errorf("本轮的落点被改了：内存里现在是 %q", hub.cur().TargetDir)
	}

	// 重选一个别的目录才是「用户要它永久」：盘上跟内存都换成那一份。
	chosen := hub.cur()
	chosen.TargetDir = t.TempDir()
	if err := hub.Save(dtoFor(chosen)); err != nil {
		t.Fatalf("Save 重选: %v", err)
	}
	loaded, err = hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.TargetDir != chosen.TargetDir {
		t.Errorf("用户重选的落点没存住：盘上是 %q", loaded.Settings.TargetDir)
	}
	if hub.cur().TargetDir != chosen.TargetDir {
		t.Errorf("重选之后内存里还挂着启动参数那一份：%q", hub.cur().TargetDir)
	}

	// 参数已经让位，之后的保存就只是普通的保存：盘上跟着显示的走。
	again := hub.cur()
	again.ToastSeconds = 3
	if err := hub.Save(dtoFor(again)); err != nil {
		t.Fatalf("Save 之后: %v", err)
	}
	loaded, err = hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.TargetDir != chosen.TargetDir {
		t.Errorf("覆盖已经不存在了还把它当临时目录供着：%q", loaded.Settings.TargetDir)
	}
}

// 首次运行只配了启动参数（盘上没有转移目录）时，面板上任何一次保存就是把那个落点定成永久：
// 没有「原本那一份」要保，此时拒绝保存只会让其它控件全点不动。
func TestDestFolderAdoptsWhenDiskHasNoTarget(t *testing.T) {
	src := t.TempDir()
	s := config.Default()
	s.SourceDir = src
	hub, err := newHub(t.TempDir(), quietLog(), s)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	run := t.TempDir()
	if err := hub.UseDestFolder(run); err != nil {
		t.Fatalf("UseDestFolder: %v", err)
	}
	next := hub.cur()
	next.ToastSeconds = 5
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, lerr := hub.store.Load()
	if lerr != nil {
		t.Fatalf("回读设置: %v", lerr)
	}
	if loaded.Settings.TargetDir != run {
		t.Errorf("盘上的转移目录是 %q，本轮那份没转正", loaded.Settings.TargetDir)
	}
}

// 真搬一次：文件落到启动参数指的那个目录，而不是 settings.json 里那一个。
// 前面几条测的都是内存与盘上的字段对不对；字段对了而字节去了别处，才是这套用法最坏的
// 失败方式，所以这里让真监听、真判定、真搬运跑完一遍。
func TestDestFolderIsWhereTheFileActuallyLands(t *testing.T) {
	src, dst, run := t.TempDir(), t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })
	if err := hub.UseDestFolder(run); err != nil {
		t.Fatalf("UseDestFolder: %v", err)
	}
	if err := hub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	name := "本轮要搬的一个文件.zip"
	if err := os.WriteFile(filepath.Join(src, name), []byte("内容"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if foundIn(run, name) != "" {
			if foundIn(dst, name) != "" {
				t.Error("同一个文件在两个目录里各有一份")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if at := foundIn(dst, name); at != "" {
		t.Errorf("文件搬进了设置里那个目录 %q，参数的覆盖没起作用", at)
	} else {
		t.Errorf("25 秒之内没搬进参数指的目录 %q（也没在 %q）", run, dst)
	}
}

// foundIn 在这层目录里（含按日期开的那一档子目录）找 name，找到给完整路径，找不到给空串。
func foundIn(root, name string) string {
	var out string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			out = p
		}
		return nil
	})
	return out
}

// --lang 与 --theme 换的是本轮的样子：内存跟着走，settings.json 一个字不变。
// 这条一旦失守，表现是「用一次英文启动，之后每次开屏都是英文」—— 而拖一下药丸、
// 按一下暂停这类「跟语言毫无关系」的顺手落盘，正是把那句英文写死的时刻。
func TestLangAndThemeFlagsApplyForThisRunOnly(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	if err := hub.UseLang(config.LangEn); err != nil {
		t.Fatalf("UseLang: %v", err)
	}
	if err := hub.UseTheme(config.ThemeLight); err != nil {
		t.Fatalf("UseTheme: %v", err)
	}
	if s := hub.cur(); s.Lang != config.LangEn || s.Theme != config.ThemeLight {
		t.Errorf("本轮的外观没换过来：%+v", s)
	}

	// 两次用户没有说过任何一句关于语言或配色的自动落盘。
	if err := hub.SetAnchor(config.Anchor{RightInset: 7, BottomInset: 8}); err != nil {
		t.Fatalf("SetAnchor: %v", err)
	}
	if err := hub.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	loaded, lerr := hub.store.Load()
	if lerr != nil {
		t.Fatalf("回读设置: %v", lerr)
	}
	if loaded.Settings.Lang != config.LangAuto {
		t.Errorf("一次性的语言跟着锚点落盘了：盘上现在是 %q", loaded.Settings.Lang)
	}
	if loaded.Settings.Theme != config.ThemeDark {
		t.Errorf("一次性的配色跟着暂停落盘了：盘上现在是 %q", loaded.Settings.Theme)
	}
	if !loaded.Settings.Paused {
		t.Error("暂停没存住：那一条本来就该落盘")
	}
	if s := hub.cur(); s.Lang != config.LangEn || s.Theme != config.ThemeLight {
		t.Errorf("自动落盘把本轮的覆盖洗掉了：%+v", s)
	}
}

// 面板没有保存按钮，改任何控件都整份送回来。送回来的那份里根本没有 lang/theme 两个字段
// （见 saveDTO），它们只可能从 mergeInto 的「继承当前设置」那一步漏出去 —— 所以这里钉的
// 就是那一步：改个提示秒数不该把本轮那句英文、那个浅色写成永久设置。
func TestPanelSaveDuringLangAndThemeFlags(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	if err := hub.UseLang(config.LangEn); err != nil {
		t.Fatalf("UseLang: %v", err)
	}
	if err := hub.UseTheme(config.ThemeLight); err != nil {
		t.Fatalf("UseTheme: %v", err)
	}

	next := hub.cur()
	next.ToastSeconds = 7
	if err := hub.Save(dtoFor(next)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.ToastSeconds != 7 {
		t.Errorf("提示秒数没存住：%d", loaded.Settings.ToastSeconds)
	}
	if loaded.Settings.Lang != config.LangAuto || loaded.Settings.Theme != config.ThemeDark {
		t.Errorf("一次性的外观参数跟着一次保存落盘了：盘上现在是 %q/%q",
			loaded.Settings.Lang, loaded.Settings.Theme)
	}
	// 界面读的是 hub.cur()（State 那条路）：覆盖不能被这次保存洗掉。
	if s := hub.cur(); s.Lang != config.LangEn || s.Theme != config.ThemeLight {
		t.Errorf("保存把本轮的外观洗掉了：%+v", s)
	}
}

// 启动参数给了英文，用户在界面上按下配色那颗钮：他按下的是配色，进盘的就该只有配色。
// 顺带把那句一次性的英文写成永久设置，表现是「没人让它变成英文，下次开屏它就是英文」。
func TestClickingThemeDuringLangFlagPersistsOnlyTheme(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	if err := hub.UseLang(config.LangEn); err != nil {
		t.Fatalf("UseLang: %v", err)
	}
	if err := hub.SetTheme(config.ThemeLight); err != nil {
		t.Fatalf("SetTheme: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.Theme != config.ThemeLight {
		t.Errorf("用户按下的那一项没存住：盘上配色是 %q", loaded.Settings.Theme)
	}
	if loaded.Settings.Lang != config.LangAuto {
		t.Errorf("用户没按下的那条跟着落盘了：盘上语言是 %q", loaded.Settings.Lang)
	}
	if hub.cur().Lang != config.LangEn {
		t.Errorf("本轮那句英文被这次点配色洗掉了：%q", hub.cur().Lang)
	}
}

// 上一条的反面：参数给的是配色，用户点的是语言。
func TestClickingLangDuringThemeFlagPersistsOnlyLang(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	if err := hub.UseTheme(config.ThemeLight); err != nil {
		t.Fatalf("UseTheme: %v", err)
	}
	if err := hub.SetLang(config.LangZh); err != nil {
		t.Fatalf("SetLang: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if loaded.Settings.Lang != config.LangZh {
		t.Errorf("用户选的语言没存住：盘上是 %q", loaded.Settings.Lang)
	}
	if loaded.Settings.Theme != config.ThemeDark {
		t.Errorf("用户没碰的配色跟着落盘了：盘上是 %q", loaded.Settings.Theme)
	}
	if hub.cur().Theme != config.ThemeLight {
		t.Errorf("本轮那个浅色被洗掉了：%q", hub.cur().Theme)
	}
}

// 换外观不打断监控：这两项不进 affectsRuntime，重建会重新建基线，
// 正在传的那个文件就此变成「程序起来之前就存在的老文件」，永远搬不走。
func TestLangAndThemeFlagsDoNotRebuildTheWatch(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })
	if err := hub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hub.isRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.isRunning() {
		t.Fatal("配好目录之后监听仍然没起来")
	}
	first := hub.watchSinceNow()
	if first.IsZero() {
		t.Fatal("监听起来了却没盖对账起点")
	}

	if err := hub.UseLang(config.LangEn); err != nil {
		t.Fatalf("UseLang: %v", err)
	}
	if err := hub.UseTheme(config.ThemeLight); err != nil {
		t.Fatalf("UseTheme: %v", err)
	}
	if !hub.isRunning() {
		t.Error("换个语言配色把监控弄断了")
	}
	if second := hub.watchSinceNow(); !second.Equal(first) {
		t.Errorf("换外观重建了监听，对账起点往后挪了：%v → %v", first, second)
	}
}

// 命令行上写错一个词的人看不见界面反馈：UseLang/UseTheme 走「报错」而不是回落默认
// （口径和界面上那两颗钮相反，那里是偏好，这里是判决）。
func TestUseLangAndUseThemeRejectUnknownValues(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	for _, c := range []struct {
		apply func(string) error
		name  string
		bad   string
	}{
		{hub.UseLang, "lang", "french"},
		{hub.UseLang, "lang", "zh-CN"},
		{hub.UseTheme, "theme", "sepia"},
		{hub.UseTheme, "theme", "system"},
	} {
		err := c.apply(c.bad)
		if err == nil {
			t.Errorf("--%s=%q 竟然收下了：静默改回默认等于把用户的意图吞掉", c.name, c.bad)
			continue
		}
		if code := apperr.Of(err).Code; code != apperr.EInvalidArg {
			t.Errorf("--%s=%q 给出的是 %s, 想要 E_INVALID_ARG", c.name, c.bad, code)
		}
	}
	if s := hub.cur(); s.Lang != config.LangAuto || s.Theme != config.ThemeDark {
		t.Errorf("被拒绝的参数改了本轮的设置：%+v", s)
	}

	// 大小写与首尾空格不是一场错误：命令行上是人在敲。
	if err := hub.UseLang(" EN "); err != nil || hub.cur().Lang != config.LangEn {
		t.Errorf(`--lang=" EN " 给出 %v, 现在是 %q`, err, hub.cur().Lang)
	}
	if err := hub.UseTheme(" Light"); err != nil || hub.cur().Theme != config.ThemeLight {
		t.Errorf(`--theme=" Light" 给出 %v, 现在是 %q`, err, hub.cur().Theme)
	}
	// auto 是一个合法值（跟着系统走），不是「没填」。
	if err := hub.UseLang(config.LangAuto); err != nil {
		t.Errorf("--lang=auto 报错了：%v", err)
	}
}

// affectsRuntime 是「下一次重建才生效」的唯一判据表，暂停必须不在表里：
// 重建会重新建基线，正在传的那个文件会被打回「程序起来之前就存在的老文件」，
// 等它改名完成就永远搬不走了。
func TestAffectsRuntime(t *testing.T) {
	base := readySettings(`C:\src`, `D:\dst`)
	cases := []struct {
		name  string
		match func(*config.Settings)
		want  bool
	}{
		{"什么都没改", func(*config.Settings) {}, false},
		{"提示秒数", func(s *config.Settings) { s.ToastSeconds = 9 }, false},
		{"提示开关", func(s *config.Settings) { s.ToastEnabled = false }, false},
		{"窗口位置", func(s *config.Settings) { s.Anchor.BottomInset = 99 }, false},
		{"暂停", func(s *config.Settings) { s.Paused = true }, false},
		// 落点子目录是搬运时现读现用的：为它重建等于让一次勾选打断一次在传的搬运。
		{"按日期分子目录", func(s *config.Settings) { s.DateSubdirs = false }, false},
		// 语言只换文案，后端连一个词都不出：为它重建是一笔纯粹赔出去的代价。
		{"界面语言", func(s *config.Settings) { s.Lang = config.LangEn }, false},
		// 配色同语言：`--theme` 那一条也不许有重建的资格（见 UseTheme）。
		{"配色", func(s *config.Settings) { s.Theme = config.ThemeLight }, false},
		{"静置阈值", func(s *config.Settings) { s.QuietMs = 1_500 }, true},
		{"最长等待", func(s *config.Settings) { s.MaxSettleMs = 90_000 }, true},
		{"转移目录", func(s *config.Settings) { s.TargetDir = `E:\elsewhere` }, true},
		{"监控目录", func(s *config.Settings) { s.SourceDir = `C:\Users\x\Downloads` }, true},
	}
	for _, c := range cases {
		next := base
		c.match(&next)
		if got := affectsRuntime(base, next); got != c.want {
			t.Errorf("%s: affectsRuntime=%v, 想要 %v", c.name, got, c.want)
		}
	}
}

// 语言必须落盘：它是跨启动的偏好，下次开屏还得是那句英文。
// 认不出的值回落 auto 而不是报错 —— 那是一个偏好，不是一次判决（口径同 theme）。
func TestSetLangPersistsAndFallsBackToAuto(t *testing.T) {
	dir := t.TempDir()
	hub, err := newHub(dir, quietLog(), readySettings(t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatalf("建 Hub 就失败了：%v", err)
	}
	if err := hub.SetLang(config.LangEn); err != nil {
		t.Fatalf("SetLang 报错了：%v", err)
	}
	back, err := hub.store.Load()
	if err != nil {
		t.Fatalf("读不回设置：%v", err)
	}
	if back.Settings.Lang != config.LangEn {
		t.Errorf("盘上的语言 = %q, 想要 %q", back.Settings.Lang, config.LangEn)
	}
	if err := hub.SetLang("klingon"); err != nil {
		t.Fatalf("SetLang 认不出值时报错了：%v", err)
	}
	if got := hub.cur().Lang; got != config.LangAuto {
		t.Errorf("认不出的语言没回落：%q, 想要 %q", got, config.LangAuto)
	}
}

// mergeInto 是「差量」的唯一定义处：这里漏一个字段，那个字段就永远保存不了；
// 多一个字段，保存就可能把用户没打算改的东西改掉。
func TestMergeIntoOnlyTouchesPanelFields(t *testing.T) {
	base := readySettings(`C:\src`, `D:\dst`)
	base.Paused = true
	base.Anchor = config.Anchor{Expanded: true, RightInset: 7, BottomInset: 8}
	base.Lang = config.LangEn
	base.TargetDirPresets = []string{`D:\dst`, `E:\旧的档`}
	d := saveDTO{TargetDir: `E:\new`, DateSubdirs: false, ToastEnabled: false, ToastSeconds: 11, QuietMs: 1_200, MaxSettleMs: 30_000}
	got := d.mergeInto(base)
	if got.TargetDir != `E:\new` || got.DateSubdirs || got.ToastEnabled || got.ToastSeconds != 11 || got.QuietMs != 1_200 || got.MaxSettleMs != 30_000 {
		t.Errorf("该改的没改全：%+v", got)
	}
	if got.SourceDir != base.SourceDir || got.Paused != base.Paused || got.Anchor != base.Anchor || got.Version != base.Version || got.Lang != base.Lang {
		t.Errorf("不该改的被改了：%+v", got)
	}
	// 没送列表 = 列表不动，不是「把用户的常用目录清空」：多一个控件、少一个控件都不该洗掉那份清单。
	if !reflect.DeepEqual(got.TargetDirPresets, []string{`D:\dst`, `E:\旧的档`}) {
		t.Errorf("没送常用列表就被洗掉了：%q", got.TargetDirPresets)
	}
}

// 常用列表记的是**落盘那一份**目录：--dest-folder 本轮用的那个压根没打算留下，
// 跟着记进列表等于把一次临时启动变成一个「下次一点就搬进去」的档。
func TestPresetListRemembersThePersistentTargetOnly(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(src, dst))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	run := t.TempDir()
	if err := hub.UseDestFolder(run); err != nil {
		t.Fatalf("UseDestFolder: %v", err)
	}
	// 本轮的落点是参数那份，面板改一个跟目录无关的控件把当前显示原样回显回来。
	toastOnly := hub.cur()
	toastOnly.ToastSeconds = 8
	if err := hub.Save(dtoFor(toastOnly)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	list := loaded.Settings.TargetDirPresets
	if len(list) != 1 || list[0] != dst {
		t.Errorf("常用列表记下了临时目录：%q, 想要只有 %q", list, dst)
	}
	if mem := hub.cur().TargetDirPresets; len(mem) != 1 || mem[0] != dst {
		t.Errorf("内存那份和盘上那份不是一套：界面重画会看到 %q", mem)
	}
	if hub.cur().TargetDir != run {
		t.Errorf("本轮落点被改了：内存里现在是 %q", hub.cur().TargetDir)
	}
}

// ▾ 上那颗 ✕ 是「删掉一条之后的整串」：其余条要按界面给的顺序留在盘上。
func TestPanelCanRemoveOnePreset(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	set := readySettings(src, dst)
	old := t.TempDir()
	set.TargetDirPresets = []string{dst, old}
	hub, err := newHub(t.TempDir(), quietLog(), set)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	d := dtoFor(hub.cur())
	d.TargetDirPresets = []string{dst} // 用户点掉了 old 那一条
	if err := hub.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := hub.store.Load()
	if err != nil {
		t.Fatalf("回读设置: %v", err)
	}
	if got := loaded.Settings.TargetDirPresets; len(got) != 1 || got[0] != dst {
		t.Errorf("删掉的那一条又回来了：%q", got)
	}
}

// 选中列表里的一条就是换一次落点：那一档要跟着挪到最前，而不是留在原位。
func TestPickingAPresetMovesItToTheFront(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	set := readySettings(src, dst)
	older := t.TempDir()
	set.TargetDirPresets = []string{dst, older}
	hub, err := newHub(t.TempDir(), quietLog(), set)
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })

	d := dtoFor(hub.cur())
	d.TargetDir = older
	if err := hub.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := hub.cur().TargetDirPresets
	if len(got) != 2 || got[0] != older {
		t.Errorf("刚选中的没在最前：%q", got)
	}
}

// 「查看」在今天的落点还不存在时退到转移目录，而不是给用户一句报错：
// 「今天还没搬过东西」是一个答案，而那颗按钮要回答的是「东西到底在哪」。
func TestTodayDirFallsBackToTarget(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local)
	dst := t.TempDir()
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(t.TempDir(), dst))
	if err != nil {
		t.Fatalf("建 Hub 就失败了：%v", err)
	}

	got, err := hub.TodayDir(now)
	if err != nil {
		t.Fatalf("读今天的落点：%v", err)
	}
	if got.Exists {
		t.Errorf("刚建好的转移目录里不该有今天那一档：%+v", got)
	}
	if got.Opened != dst {
		t.Errorf("落点缺失时应退到转移目录，opened 给出 %q, 想要 %q", got.Opened, dst)
	}

	// 今天那一档真的有了之后就该开它自己，而不是父目录 —— 否则用户每次看到的都是全量。
	day := filepath.Join(dst, got.Dated)
	if err := os.Mkdir(day, 0o755); err != nil {
		t.Fatalf("建日期目录：%v", err)
	}
	if got, err = hub.TodayDir(now); err != nil || !got.Exists || got.Opened != day {
		t.Errorf("今天那一档在的时候给出 %+v (err=%v), 想要 opened=%q", got, err, day)
	}

	// 连转移目录本身都没了：哪儿都不该开，而不是把 explorer 指向一个不存在的路径。
	if err := os.RemoveAll(dst); err != nil {
		t.Fatalf("删转移目录：%v", err)
	}
	if got, err = hub.TodayDir(now); err != nil || got.Opened != "" {
		t.Errorf("两处都不存在时给出 opened=%q (err=%v), 想要空串", got.Opened, err)
	}
}

// 没开「按日期分目录」时今天那一档就是转移目录本身：opened 与 dir 必须同值，
// 否则前端那句「已打开 X」和「退到转移目录」会分叉，而用户看的是同一个目录。
func TestTodayDirWithoutDateSubdirs(t *testing.T) {
	dst := t.TempDir()
	set := readySettings(t.TempDir(), dst)
	set.DateSubdirs = false
	hub, err := newHub(t.TempDir(), quietLog(), set)
	if err != nil {
		t.Fatalf("建 Hub 就失败了：%v", err)
	}
	got, err := hub.TodayDir(time.Now())
	if err != nil {
		t.Fatalf("读今天的落点：%v", err)
	}
	if got.Dated != "" || got.Dir != dst || got.Opened != dst {
		t.Errorf("不分目录时给出 %+v, 想要 dir 与 opened 都等于 %q", got, dst)
	}
}

// 对账往回追的起点不能追过「监听起来」那一刻：那之前就在目录里的东西，基线已经把它
// 记成老文件了，对账再把它补成一次下载，等于把 Downloads 里碰巧被改过的老东西都搬走。
// 第一行就是那份报告的现场：20:19:08 启动，20:20:09 第一次对账。
func TestReconcileCutoffNeverPredatesWatchStart(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	start := time.Date(2026, 10, 12, 20, 19, 8, 0, cst)
	for _, tc := range []struct {
		name  string
		now   time.Time
		since time.Time
		want  time.Time
	}{
		{"启动 61 秒后的对账只追到启动那一刻", start.Add(61 * time.Second), start, start},
		{"启动 20 分钟后回到 10 分钟窗口", start.Add(20 * time.Minute), start, start.Add(10 * time.Minute)},
		{"还没监听过就退回正常窗口，不把 cutoff 推到未来", start, time.Time{}, start.Add(-10 * time.Minute)},
	} {
		if got := reconcileCutoff(tc.now, tc.since); !got.Equal(tc.want) {
			t.Errorf("%s：cutoff=%v, 想要 %v", tc.name, got, tc.want)
		}
	}
}

// watchSince 必须由「监听真的起来了」那一刻盖戳，暂停再恢复要往后挪 —— 否则暂停期间
// 落进目录的文件会在恢复后的第一次对账里被补搬，而暂停的语义是「什么都不生效」。
func TestWatchSinceStampedByRunning(t *testing.T) {
	hub, err := newHub(t.TempDir(), quietLog(), readySettings(t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatalf("newHub: %v", err)
	}
	t.Cleanup(func() { hub.Shutdown() })
	if !hub.watchSinceNow().IsZero() {
		t.Error("还没 Start 就有了监听时刻")
	}
	if err := hub.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !hub.isRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	first := hub.watchSinceNow()
	if first.IsZero() {
		t.Fatal("监听起来了却没盖对账起点")
	}
	if err := hub.SetPaused(true); err != nil {
		t.Fatalf("暂停: %v", err)
	}
	if err := hub.SetPaused(false); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if second := hub.watchSinceNow(); !second.After(first) {
		t.Errorf("恢复监听没往后挪对账起点：%v → %v", first, second)
	}
}
