package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"download2/internal/apperr"
)

// newTestSink 在临时目录里立一份运行日志：日志相关的测试绝不碰 exe 旁边那一份真的。
// 退出时把句柄关掉 —— 不然 Windows 上 TempDir 清不掉这个文件，整个测试就红在这里。
func newTestSink(t *testing.T) (*logSink, *slog.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), logName)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	sink := &logSink{path: path, f: f}
	t.Cleanup(func() { _ = sink.Close() })
	return sink, slog.New(slog.NewTextHandler(sink, nil)), path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newSinkAt 在指定路径上立一份运行日志，走的是真启动那条路（openLogFile + logSink）。
func newSinkAt(t *testing.T, path string, fresh bool) *logSink {
	t.Helper()
	f, err := openLogFile(path, fresh)
	if err != nil {
		t.Fatal(err)
	}
	sink := &logSink{path: path, f: f}
	t.Cleanup(func() { _ = sink.Close() })
	return sink
}

// 后起的那一份不许踩坏正在跑的那一份的日志：两份进程共用 exe 旁边那一个文件，
// 而单实例判定比开日志晚。当年 os.Create 带的 O_TRUNC 会把前一份写过的整段抹掉；
// 而「不清空」还不够 —— 本机实测 Windows 上的 os.O_APPEND 只在打开时挪一次指针，
// 前一份下一句仍从自己的老偏移往回写，正好把后一份那句整段盖掉（tools/appendprobe）。
func TestLogSinkDoesNotClobberTheRunningInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), logName)
	running := newSinkAt(t, path, true)
	logged(t, running, "前一份的第一句")

	second := newSinkAt(t, path, false)
	logged(t, second, "后一份的一句")

	// 关键在这一步：这个句柄完全不知道中间被人加过一段。
	logged(t, running, "前一份的第二句")

	s := string(readFile(t, path))
	if strings.ContainsRune(s, 0) {
		t.Error("日志里混进了 NUL，说明写没落在文件末尾")
	}
	if want := "前一份的第一句\n后一份的一句\n前一份的第二句\n"; s != want {
		t.Errorf("三份句子没按先后落在末尾：%q", s)
	}
}

// 没有别人在跑时仍然是一次干净的开始：日志不该跨启动无限长下去。
func TestLogSinkStartsCleanForTheOnlyInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), logName)
	logged(t, newSinkAt(t, path, false), "上一轮的一句")

	fresh := newSinkAt(t, path, true)
	if raw := readFile(t, path); len(raw) != 0 {
		t.Errorf("本轮是唯一的一份却没清空日志：%d 字节", len(raw))
	}
	logged(t, fresh, "这一轮的一句")
	if s := string(readFile(t, path)); s != "这一轮的一句\n" {
		t.Errorf("清空之后没从开头写：%q", s)
	}
}

// logged 往 sink 里落一行，走的就是 slog 每条记录一次 Write 的那个入口。
// 不经 slog：时间戳与级别会让「按先后落在末尾」那条断言变成逐字节比对一个猜出来的串。
func logged(t *testing.T, sink *logSink, line string) {
	t.Helper()
	if _, err := sink.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

// logSink 是「清空日志」那颗按钮对运行日志做的那件事。这里钉的是那条不该出现的 NUL 空洞：
// 就地 truncate 一个本进程开着的句柄不会把写位置挪回 0，下一条日志会从老偏移写回去，
// 于是「清空」之后打开它看到的是一段看不见的空白 + 半句话。换底（关掉、重开）才干净。
func TestLogSinkResetStartsClean(t *testing.T) {
	sink, log, path := newTestSink(t)
	first := strings.Repeat("旧的一句", 40)
	log.Info(first)

	if err := sink.Reset(); err != nil {
		t.Fatal(err)
	}
	if raw := readFile(t, path); len(raw) != 0 {
		t.Errorf("Reset 之后日志不是空的：%d 字节", len(raw))
	}

	log.Info("新的一句")
	s := string(readFile(t, path))
	if strings.ContainsRune(s, 0) {
		t.Error("换底之后的日志里混进了 NUL")
	}
	if !strings.Contains(s, "新的一句") || strings.Contains(s, first) {
		t.Errorf("日志没从文件开头重写：%s", s[:min(80, len(s))])
	}
	// 关的是换底之后那一个新句柄，第二次 Close 该没什么可做的。
	if err := sink.Close(); err != nil {
		t.Errorf("关闭换底后的句柄报错: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("第二次 Close 不该做事：%v", err)
	}
}

func TestLogSinkResetWithoutAPath(t *testing.T) {
	sink := &logSink{}
	err := sink.Reset()
	if err == nil {
		t.Fatal("解析不出 exe 位置时 Reset 该报错，而不是假装清了")
	}
	e := apperr.Of(err)
	if e.Code != apperr.EFileOpFailed || e.Msg == "" {
		t.Errorf("给出的是 %+v", e)
	}
	// 写不出的日志不该拦住任何事：没有句柄时 Write 把这一条吃掉并报告成功。
	if n, werr := sink.Write([]byte("任何一句话")); werr != nil || n != 15 {
		t.Errorf("空句柄的 Write 给出 %d, %v", n, werr)
	}
}

func TestLogSinkResetWhenTheFileCannotBeMade(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "没有这个子目录", logName)
	sink := &logSink{path: missing}
	if err := sink.Reset(); err == nil {
		t.Fatal("重开失败却报了成功：界面上的「已清空」就成了假话")
	}
	if sink.Path() != missing {
		t.Errorf("失败之后 Path() 把路径也弄丢了：%q", sink.Path())
	}
}

// 启动参数一共三条，三种写法都得认（flag 包本身就把 `-x` 与 `--x` 视为同一条路）。
// 空命令行是双击启动的常态而不是错误；认不出的参数只回报一个错，绝不 exit ——
// -H windowsgui 之后 exit 表现成「双击了没反应」，比参数打错了难查得多。
func TestParseLaunchShapes(t *testing.T) {
	for _, c := range []struct {
		args []string
		want launch
	}{
		{[]string{"-dest-folder", `D:\x`}, launch{destFolder: `D:\x`}},
		{[]string{"--dest-folder", `D:\x`}, launch{destFolder: `D:\x`}},
		{[]string{"--dest-folder=D:\\x"}, launch{destFolder: `D:\x`}},
		{[]string{"-lang", "en"}, launch{lang: "en"}},
		{[]string{"--lang=en"}, launch{lang: "en"}},
		{[]string{"--theme", "light"}, launch{theme: "light"}},
		{[]string{"-theme=light"}, launch{theme: "light"}},
		// 三条一起提是这套用法最自然的样子：一条临时落点配一句英文、一个浅色。
		{[]string{"--dest-folder=D:\\x", "--lang=en", "-theme", "light"},
			launch{destFolder: `D:\x`, lang: "en", theme: "light"}},
		// 落点目录里带空格也不能散：那是「下载」目录的常态。
		{[]string{"--dest-folder", `D:\My Downloads`, "--lang", "zh"},
			launch{destFolder: `D:\My Downloads`, lang: "zh"}},
	} {
		got, err := parseLaunch(c.args)
		if err != nil || got != c.want {
			t.Errorf("%q 给出 %+v, %v", c.args, got, err)
		}
	}
	if got, err := parseLaunch(nil); err != nil || got != (launch{}) {
		t.Errorf("不带参数时给出 %+v, %v", got, err)
	}
	for _, args := range [][]string{
		{"--dest-foldr", `D:\x`},
		{"--lan", "en"},
		{"--them", "light"},
	} {
		if _, err := parseLaunch(args); err == nil {
			t.Errorf("%q 被静默吞掉了，日志里连一条都不会有", args)
		}
	}
}

// 打错一个参数名的人没有控制台可看，所以那一句得自己把「认得哪三条」说出来；
// 而它必须塞得进 toast 那两行 —— 超了就只剩一个省略号，等于没说。
func TestBadLaunchNoticeFitsTheToast(t *testing.T) {
	for _, want := range []string{"--dest-folder", "--lang", "--theme"} {
		if !strings.Contains(badLaunchNotice, want) {
			t.Errorf("提示里少了 %q：%s", want, badLaunchNotice)
		}
	}
	if c := toastCells(badLaunchNotice); c > toastCellsMax {
		t.Errorf("那一句要 %d 个半角位，两行只放得下 %d 个：%s", c, toastCellsMax, badLaunchNotice)
	}
}

// 值写错那两句同理：它们就是 toast 上要读完的那句话，认哪几个词必须写在里面。
func TestRejectedValueNoticesFitTheToast(t *testing.T) {
	h := &Hub{log: slog.New(slog.DiscardHandler)}
	for _, c := range []struct {
		apply func(string) error
		value string
		want  []string
	}{
		{h.UseLang, "zn", []string{"auto", "zh", "en"}},
		{h.UseTheme, "pink", []string{"dark", "light"}},
	} {
		err := c.apply(c.value)
		if err == nil {
			t.Fatalf("%q 居然被收下了", c.value)
		}
		msg := "--x 没用上：" + apperr.Of(err).Msg
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Errorf("提示里没列出 %q：%s", w, msg)
			}
		}
		if got := toastCells(msg); got > toastCellsMax {
			t.Errorf("%q 那句要 %d 个半角位，两行只放得下 %d 个：%s", c.value, got, toastCellsMax, msg)
		}
	}
}

// toastCells 按「半角位」数一句 toast 有多长：CJK 与全角标点算两位，其余一位。
// 两行的容量按真窗口拍到那句（「--lang 没用上：界面语言只认 auto、zh、en」正好两行）估的。
func toastCells(s string) int {
	n := 0
	for _, r := range s {
		w := 1
		if r >= 0x2E80 {
			w = 2
		}
		n += w
	}
	return n
}

const toastCellsMax = 60

func TestJoinNoticeDoesNotLeaveAStraySeparator(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"", "", ""},
		{"甲", "", "甲"},
		{"", "乙", "乙"},
		{"甲", "乙", "甲；乙"},
	} {
		if got := joinNotice(c.a, c.b); got != c.want {
			t.Errorf("joinNotice(%q, %q) = %q，想要 %q", c.a, c.b, got, c.want)
		}
	}
}

// OpenLog 交出去的名字是 "notepad"（不带 .exe、不经 shell），所以它得能在 PATH 里找着。
// 这里只查找不找得到：真开一次会往桌面扔一个记事本窗口，而「文件能不能被画出来」
// 已经用外部探针在这台机器上实测过了（连带空格的路径）。
func TestNotepadIsResolvable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("notepad 是 Windows 的东西")
	}
	if _, err := exec.LookPath("notepad"); err != nil {
		t.Errorf("PATH 里找不到 notepad：%v", err)
	}
}
