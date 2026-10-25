package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"download2/internal/apperr"
)

// 所有用例的路径都在 t.TempDir() 下面，绝不碰真实的 %APPDATA%\download2 与 Downloads。
// 盘符形态的规则在 validate_windows_test.go（带 build tag），所以这一档能跨平台跑。

func TestDefaultIsSafeStartingPoint(t *testing.T) {
	d := Default()

	if d.Version != Version {
		t.Errorf("Version = %d, 想要 %d", d.Version, Version)
	}
	// 这两个开关默认开：按日期归档和 toast 是用户明确要过的行为。
	if !d.DateSubdirs || !d.ToastEnabled {
		t.Errorf("DateSubdirs/ToastEnabled = %v/%v, 都要 true", d.DateSubdirs, d.ToastEnabled)
	}
	if d.QuietMs != 900 {
		t.Errorf("QuietMs = %d, 想要 900", d.QuietMs)
	}
	// 最长等待必须远大于静置阈值，否则一次慢盘采样就烧完预算、什么都搬不动。
	if d.MaxSettleMs <= d.QuietMs {
		t.Errorf("MaxSettleMs=%d 没超过 QuietMs=%d", d.MaxSettleMs, d.QuietMs)
	}
	// 目录一个都不填：没配上之前不该搬任何文件，这点必须由默认值自己保证。
	if d.SourceDir != "" || d.TargetDir != "" {
		t.Errorf("默认目录不该非空: %q / %q", d.SourceDir, d.TargetDir)
	}
	if d.Ready() {
		t.Error("默认值就 Ready()，等于允许拿猜出来的目录搬东西")
	}
	if d.Anchor.RightInset != 24 || d.Anchor.BottomInset != 24 {
		t.Errorf("默认内缩量 = %d/%d, 想要 24/24", d.Anchor.RightInset, d.Anchor.BottomInset)
	}
	// 深色是默认：药丸本来就画在半透明实底上，浅色要靠用户自己点。
	if d.Theme != ThemeDark {
		t.Errorf("默认主题 = %q, 想要 %q", d.Theme, ThemeDark)
	}
	// 语言默认不表态：先按系统语言猜，猜错了用户看得见、也改得动。
	if d.Lang != LangAuto {
		t.Errorf("默认语言 = %q, 想要 %q", d.Lang, LangAuto)
	}
	if err := d.Validate(); err == nil {
		t.Fatal("默认值竟然过校验——两个目录还没配，过了等于允许搬东西")
	}
	if e := apperr.Of(d.Validate()); e.Code != apperr.EInvalidArg {
		t.Errorf("默认值的拒绝码 = %s, 想要 E_INVALID_ARG（是没配上，不是没权限）", e.Code)
	}
}

func TestReadyNeedsBothDirs(t *testing.T) {
	s := Default()
	s.SourceDir = filepath.Join(t.TempDir(), "a")
	if s.Ready() {
		t.Error("只有源目录就该 Ready，不对")
	}
	s.TargetDir = filepath.Join(t.TempDir(), "b")
	if !s.Ready() {
		t.Error("两个目录都在却不 Ready")
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := Default()
	want.SourceDir = filepath.Join(dir, "downloads")
	want.TargetDir = filepath.Join(dir, "moved")
	want.ToastSeconds = 9
	want.QuietMs = 1500
	want.Paused = true
	want.Anchor = Anchor{Expanded: true, RightInset: 77, BottomInset: 88}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("设置文件没落盘: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.FromDisk {
		t.Error("文件明明在，FromDisk 却是 false")
	}
	if got.Settings.ToastSeconds != 9 || got.Settings.QuietMs != 1500 {
		t.Errorf("数字没读回来: %+v", got.Settings)
	}
	if got.Settings.Anchor != want.Anchor {
		t.Errorf("Anchor = %+v, 想要 %+v", got.Settings.Anchor, want.Anchor)
	}
	// 存档里的目录必须是绝对路径——面板要把这串字原样显示给用户。
	if got.Settings.SourceDir != want.SourceDir {
		t.Errorf("SourceDir = %q, 想要 %q", got.Settings.SourceDir, want.SourceDir)
	}
	// 暂停是用户按下去就该活过一次重启的状态。
	if !got.Settings.Paused {
		t.Error("Paused 没读回来")
	}
}

func TestSaveLeavesNoScratchFileAndBacksUpPrevious(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := Default()
	s.TargetDir = filepath.Join(dir, "one")
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	s.TargetDir = filepath.Join(dir, "two")
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(store.Path() + ".new"); !os.IsNotExist(err) {
		t.Errorf("原子写用完还留着 .new: %v", err)
	}
	// .bak 是「上一次的值」，存在意味着换档失败时有东西可捡。
	bak, err := os.ReadFile(store.Path() + ".bak")
	if err != nil {
		t.Fatalf("没有 .bak: %v", err)
	}
	if !strings.Contains(string(bak), "one") {
		t.Error(".bak 里存的不是上一次的设置")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "还不存在"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("首次运行不该报错: %v", err)
	}
	if got.FromDisk || got.CorruptPath != "" {
		t.Errorf("缺文件却报成别的: %+v", got)
	}
	if got.Settings.QuietMs != Default().QuietMs {
		t.Errorf("缺文件时该给默认值, 得到 %d", got.Settings.QuietMs)
	}
}

// 坏文件必须被保起来，不能静默重置：用户配好的转移目录一旦被悄悄清掉，
// 下一份下载就会搬去别处，而他以为还在原处——这种数据位移事后极难追回。
func TestCorruptFileIsQuarantinedNotSilentlyReset(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"targetDir": "F:\\重要归档", "toastSeconds": 4,`)
	if err := os.WriteFile(store.Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, loadErr := store.Load()
	if loadErr == nil {
		t.Fatal("坏文件居然没报错")
	}
	if apperr.Of(loadErr).Code != apperr.EConfigCorrupt {
		t.Errorf("码 = %s, 想要 E_CONFIG_CORRUPT", apperr.Of(loadErr).Code)
	}
	if got.CorruptPath == "" {
		t.Fatal("没告诉用户坏档存哪儿了")
	}
	saved, err := os.ReadFile(got.CorruptPath)
	if err != nil {
		t.Fatalf("坏档读不回来: %v", err)
	}
	if string(saved) != string(raw) {
		t.Error("坏档内容与原文件不一致，等于二次破坏")
	}
	// 内存里给默认值：程序要继续跑得起来，界面再报错。
	if got.Settings.TargetDir != "" {
		t.Errorf("坏档不该被当真: %+v", got.Settings)
	}
	// 报错里必须留着原始解析错误，否则日志里看不出是哪一个字节把 JSON 读崩了。
	if apperr.Of(loadErr).Err == nil {
		t.Error("原始错误丢了")
	}
}

// 老文件/半截文件里的 0 值不能当「用户就要 0」：静置阈值为 0 会让判定当场通过，
// 于是把还在写的半成品搬走。
func TestNormalizeBacksZeroesUp(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "Downloads")
	dst := filepath.Join(dir, "归档")
	body := `{"sourceDir":` + jsonQuote(src) + `,"targetDir":` + jsonQuote(dst) +
		`,"dateSubdirs":false,"quietMs":0,"maxSettleMs":0,"toastSeconds":0,"version":0}`
	if err := os.WriteFile(store.Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if got.Settings.QuietMs != d.QuietMs || got.Settings.MaxSettleMs != d.MaxSettleMs || got.Settings.ToastSeconds != d.ToastSeconds {
		t.Errorf("0 值没回落默认: %+v", got.Settings)
	}
	if got.Settings.Version != Version {
		t.Errorf("缺 version 没补: %d", got.Settings.Version)
	}
	if e := got.Settings.Validate(); e != nil {
		t.Errorf("补齐后的设置应该合法, 得到 %v", e)
	}
	// 布尔开关不做「0 回落默认」：false 是用户真的关掉过它，补成 true 等于替他改主意。
	if got.Settings.DateSubdirs {
		t.Error("DateSubdirs 被补齐了，用户关掉的归档方式该保留")
	}
	// 旧文件里没有 theme 这个键：那是一台没表过态的机器，按默认深色办，而不是报「主题非法」。
	if got.Settings.Theme != ThemeDark {
		t.Errorf("缺 theme 的旧文件读出来 = %q, 想要 %q", got.Settings.Theme, ThemeDark)
	}
	// 语言同理：没表过态就交给前端按系统语言猜。
	if got.Settings.Lang != LangAuto {
		t.Errorf("缺 lang 的旧文件读出来 = %q, 想要 %q", got.Settings.Lang, LangAuto)
	}
}

// 语言和主题同一条口径：磁盘上写着不认识的值就回落，绝不因此让任何按钮点不动。
//
// 回落的是 auto 而不是 zh —— 那个词的意思是「我不知道，按系统语言猜」。
// 猜错的代价是一屏文字不对胃口，替用户改主意的代价不是。
func TestLangSurvivesRoundTripAndUnknownFallsBack(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := Default()
	s.SourceDir = filepath.Join(dir, "Downloads")
	s.TargetDir = filepath.Join(dir, "归档")
	s.Lang = LangEn
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings.Lang != LangEn {
		t.Errorf("英文没读回来: %q", got.Settings.Lang)
	}

	for _, bad := range []string{"", "EN", "zh-CN", "fr", "自动"} {
		if n := normalize(Settings{Lang: bad}); n.Lang != LangAuto {
			t.Errorf("lang=%q 回落成 %q, 想要 %q", bad, n.Lang, LangAuto)
		}
	}
}

// 主题是一个外观词，不是一个判决：磁盘上写着别的字（被人手改过、别的工具写过、
// 以后我们加了第三套配色而这份程序不认识）都不该让保存按钮点不动，
// 也不该让界面画成没有样式。所以它走 normalize 回落默认，不走 Validate 报错。
func TestThemeSurvivesRoundTripAndUnknownFallsBack(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := Default()
	s.SourceDir = filepath.Join(dir, "Downloads")
	s.TargetDir = filepath.Join(dir, "归档")
	s.Theme = ThemeLight
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings.Theme != ThemeLight {
		t.Errorf("浅色没读回来: %q", got.Settings.Theme)
	}

	// 认不出的词回落到默认，而且回落之后依然过校验。
	for _, bad := range []string{"", "Light", "WHITE", "system", "亮"} {
		if n := normalize(Settings{Theme: bad}); n.Theme != ThemeDark {
			t.Errorf("theme=%q 回落成 %q, 想要 %q", bad, n.Theme, ThemeDark)
		}
	}
}

// 路径里可能有真正的百分号（`D:\50%off%促销` 这类文件夹名是存在的），
// 保存设置绝不能把它当环境变量吃掉——被改短的路径会静默搬到别处去。
func TestSaveKeepsLiteralPercentInPaths(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := Default()
	s.SourceDir = filepath.Join(dir, "100%真实%目录")
	s.TargetDir = filepath.Join(dir, "归档")
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings.SourceDir != s.SourceDir {
		t.Errorf("存进去 %q 读出来 %q", s.SourceDir, got.Settings.SourceDir)
	}
}

func TestCleanAbsNormalizesShape(t *testing.T) {
	base := t.TempDir()
	sep := string(filepath.Separator)
	// 分隔符一律手写：filepath.Join 会提前 Clean，那样就测不到 cleanAbs 自己。
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{base + sep, base},
		{base + sep + "." + sep + "x", filepath.Join(base, "x")},
		{base + sep + "a" + sep + ".." + sep + "b", filepath.Join(base, "b")},
		{"  " + base + "  ", base},
	}
	for _, c := range cases {
		if got := cleanAbs(c.in); got != c.want {
			t.Errorf("cleanAbs(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}
	// 相对路径要变成基于当前工作目录的绝对路径，绝不能带着相对形态进校验。
	if got := cleanAbs("相对目录"); !filepath.IsAbs(got) {
		t.Errorf("相对路径没被展开: %q", got)
	}
}

// —— 校验规则：一条规则一个 E_* 码，界面按码挑文案 ——

func TestValidateRules(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "Downloads")
	dst := filepath.Join(base, "归档")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	good := func(mutate func(*Settings)) Settings {
		s := Default()
		s.SourceDir, s.TargetDir = src, dst
		mutate(&s)
		return s
	}

	cases := []struct {
		name    string
		set     Settings
		want    apperr.Code
		wantMsg string // 只做包含判断：中文文案不该被测试逐字锁死
	}{
		{"没配监控目录", good(func(s *Settings) { s.SourceDir = "" }), apperr.EInvalidArg, "监控目录"},
		{"没配转移目录", good(func(s *Settings) { s.TargetDir = "" }), apperr.EInvalidArg, "转移目录"},
		{"目标就是源", good(func(s *Settings) { s.TargetDir = src }), apperr.EPathForbidden, "不能就是监控目录"},
		{"目标就是源（多个尾斜杠）", good(func(s *Settings) { s.TargetDir = src + string(filepath.Separator) }), apperr.EPathForbidden, "不能就是监控目录"},
		{"目标在源里面——自搬环", good(func(s *Settings) { s.TargetDir = filepath.Join(src, "搬到这里") }), apperr.EPathForbidden, "再搬一次"},
		{"源在目标里面反而合法", good(func(s *Settings) { s.SourceDir = filepath.Join(dst, "Downloads") }), "", ""},
		{"静置阈值为 0", good(func(s *Settings) { s.QuietMs = 0 }), apperr.EInvalidArg, "静置阈值"},
		{"静置阈值太短", good(func(s *Settings) { s.QuietMs = 50 }), apperr.EInvalidArg, "静置阈值"},
		{"静置阈值太长", good(func(s *Settings) { s.QuietMs = 9000 }), apperr.EInvalidArg, "静置阈值"},
		{"最长等待小于 5 秒", good(func(s *Settings) { s.MaxSettleMs = 4000 }), apperr.EInvalidArg, "最长等待"},
		{"提示秒数为 0", good(func(s *Settings) { s.ToastSeconds = 0 }), apperr.EInvalidArg, "提示停留秒数"},
		{"提示秒数过长", good(func(s *Settings) { s.ToastSeconds = 16 }), apperr.EInvalidArg, "提示停留秒数"},
		{"阈值上界都合法", good(func(s *Settings) { s.QuietMs = 5000; s.ToastSeconds = 15; s.MaxSettleMs = 5000 }), "", ""},
		{"正常配置", good(func(*Settings) {}), "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.set.Validate()
			if c.want == "" {
				if err != nil {
					t.Fatalf("合法配置被拒: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("该拒的没拒: %+v", c.set)
			}
			e := apperr.Of(err)
			if e.Code != c.want {
				t.Errorf("码 = %s, 想要 %s (%v)", e.Code, c.want, err)
			}
			if !strings.Contains(e.Msg, c.wantMsg) {
				t.Errorf("文案 %q 里没有 %q", e.Msg, c.wantMsg)
			}
			// 交给界面的每一句都得是可读的中文，绝不能是 Go/OS 的英文原文。
			if strings.Contains(e.Msg, ": ") && e.Err == nil {
				t.Errorf("疑似贴了原始错误: %q", e.Msg)
			}
		})
	}
}

// 面板要在拒绝的那一行里点名是哪个目录出了问题，所以除了码还得带参数。
func TestValidateParamsCarryThePaths(t *testing.T) {
	base := t.TempDir()
	s := Default()
	s.SourceDir = filepath.Join(base, "Downloads")
	s.TargetDir = filepath.Join(s.SourceDir, "里面")
	e := apperr.Of(s.Validate())
	if e.Params["target"] == "" || e.Params["source"] == "" {
		t.Errorf("参数没带上: %v", e.Params)
	}
}

func TestEnsureTargetMakesNestedDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "a", "b", "归档")
	if err := EnsureTarget(target); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Errorf("目录没建出来: %v", err)
	}
	// 幂等：目录已存在时不该报错，用户每次保存都会重走这条。
	if err := EnsureTarget(target); err != nil {
		t.Errorf("重复调用报错: %v", err)
	}
	if err := EnsureTarget(""); err == nil {
		t.Error("空路径该拒")
	}
	// 占位文件挡住了同名目录，这是真实的失败形态（用户在目标位置放了个同名文件）。
	blocker := filepath.Join(t.TempDir(), "挡住")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnsureTarget(blocker)
	if err == nil {
		t.Fatal("路径被文件占着却成功")
	}
	if apperr.Of(err).Code != apperr.EFileOpFailed {
		t.Errorf("码 = %s, 想要 E_FILE_OP_FAILED（要把 OS 原文带进日志）", apperr.Of(err).Code)
	}
	if apperr.Of(err).Err == nil {
		t.Error("原始错误丢了，日志里查不到为什么")
	}
}

// 设置文件是人会用记事本打开看的，格式坏了也要能人工修——所以存的是缩进 JSON。
func TestSettingsFileIsHumanReadable(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := Default()
	s.TargetDir = filepath.Join(dir, "归档")
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  \"") {
		t.Error("设置文件不是缩进 JSON")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// 字段名是前后端共同的契约，这里钉住几个关键的，改名要过这条测试。
	for _, k := range []string{"version", "sourceDir", "targetDir", "targetDirPresets", "dateSubdirs", "toastEnabled", "toastSeconds", "paused", "quietMs", "maxSettleMs", "anchor", "theme"} {
		if _, ok := m[k]; !ok {
			t.Errorf("存档里少了 %q", k)
		}
	}
}

// 位置记的是「到工作区右/下边的距离」而不是绝对坐标：
// 换显示器后绝对坐标会掉到屏幕外，相对右下角却仍然是用户放药丸的那个位置。
func TestAnchorSurvivesAsInsets(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := Default()
	s.Anchor = Anchor{Expanded: true, RightInset: 1200, BottomInset: 700}
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings.Anchor != s.Anchor {
		t.Errorf("Anchor = %+v, 想要 %+v", got.Settings.Anchor, s.Anchor)
	}
}

// 常用转移目录那串：当前落点永远在最前，去重认 Windows 的路径口径，超了截断。
// 这条是那颗 ▾ 唯一的正确性来源 —— 记错了顺序，用户下次点的就是半年前那个档。
func TestPresetsRememberCurrentTargetFirst(t *testing.T) {
	dir := t.TempDir()
	sep := string(filepath.Separator)
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	c := filepath.Join(dir, "c")

	// 尾分隔符与大小写都不算第二条：留下第一次出现的那个写法，不猜也不改大小写
	// （口径同 cleanAbs —— 它也只清形；「哪几个字母该大写」只有资源管理器知道，
	// 而列表是给人点的，猜错一次就永远错在那一行上）。
	list := cleanPresets([]string{b + sep, a, "  ", c, strings.ToUpper(b)})
	want := []string{b, a, c}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("cleanPresets = %q, 想要 %q", list, want)
	}

	if got := cleanPresets(nil); got != nil {
		t.Errorf("空列表要返回 nil（设置文件里那行整个不出现），拿到 %#v", got)
	}

	// 限长：超出上限的那些按原序截掉，而不是把最新的挤到看不见的那一头。
	long := make([]string, 0, MaxTargetDirPresets+3)
	for i := 0; i < MaxTargetDirPresets+3; i++ {
		long = append(long, filepath.Join(dir, "d"+strconv.Itoa(i)))
	}
	if got := cleanPresets(long); len(got) != MaxTargetDirPresets {
		t.Errorf("常用目录没截断：%d 条, 想要 %d", len(got), MaxTargetDirPresets)
	}

	// RememberTarget 把当前落点挪到最前，哪怕它本来在列表中间。
	s := Default()
	s.TargetDir = c
	s.TargetDirPresets = []string{a, b, c}
	s.RememberTarget()
	if got := s.TargetDirPresets; len(got) != 3 || got[0] != c {
		t.Errorf("当前落点没挪到最前：%q", got)
	}
}

// 常用列表要经得起断电：存进去读出来必须是同一串，且当前落点那一条在。
func TestPresetsSurviveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := Default()
	s.TargetDir = filepath.Join(dir, "归档")
	older := filepath.Join(dir, "旧的档")
	s.TargetDirPresets = []string{older}
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{s.TargetDir, older}; !reflect.DeepEqual(got.Settings.TargetDirPresets, want) {
		t.Errorf("常用列表存完读回来是 %q, 想要 %q", got.Settings.TargetDirPresets, want)
	}

	// 只有当前落点、列表还没被用过：写盘时补上那一条，而不是留下一行空数组。
	fresh := Default()
	fresh.TargetDir = filepath.Join(dir, "第一次")
	if err := store.Save(fresh); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{fresh.TargetDir}; !reflect.DeepEqual(got.Settings.TargetDirPresets, want) {
		t.Errorf("第一次用的目录没被记住：%q", got.Settings.TargetDirPresets)
	}
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
