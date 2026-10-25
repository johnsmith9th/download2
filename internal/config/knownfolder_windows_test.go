//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandRegistryPathRejectsRelatives(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("拿不到家目录")
	}
	want := filepath.Clean(home + string(filepath.Separator) + "Downloads")
	if got := expandRegistryPath(`%USERPROFILE%\Downloads`); got != want {
		t.Errorf("展开 = %q, 想要 %q", got, want)
	}
	// 注册表里带尾斜杠的值很常见，不剥掉会让同一路径在两个地方长得不一样。
	if got := expandRegistryPath(`%USERPROFILE%\`); got != filepath.Clean(home) {
		t.Errorf("尾斜杠没剥: %q", got)
	}
	if got := expandRegistryPath(`相对\Downloads`); got != "" {
		t.Errorf("相对路径该返回空串好让调用方兜底, 得到 %q", got)
	}
	if got := expandRegistryPath(``); got != "" {
		t.Errorf("空值给出 %q", got)
	}
}

// os.ExpandEnv 只认 $NAME，注册表里的 REG_EXPAND_SZ 用的是 %NAME%。
// 这一条测的就是这个区别：没展开成功时 IsAbs 判假，下载目录解析失败，
// 外在表现是「程序活着、一个文件都不搬」。
func TestExpandPercentVars(t *testing.T) {
	t.Setenv("DOWNLOAD2_TEST_VAR", `C:\展开结果`)

	for _, c := range []struct{ in, want string }{
		{`%DOWNLOAD2_TEST_VAR%\Downloads`, `C:\展开结果\Downloads`},
		{`${DOWNLOAD2_TEST_VAR}`, `${DOWNLOAD2_TEST_VAR}`}, // $ 语法不归它管
		{`没有百分号`, `没有百分号`},
		{`100%`, `100%`},              // 落单的 % 原样留着
		{`D:\50%off`, `D:\50%off`},    // 同上：不能把名字后面的路径吃掉
		{`100%%真实`, `100%真实`},         // %% 是转义
		{`%DOWNLOAD2_没这个变量%\x`, `\x`}, // 未定义的变量展开成空，剩下的是相对路径 → 调用方兜底
	} {
		if got := expandPercentVars(c.in); got != c.want {
			t.Errorf("expandPercentVars(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}

	// 环境变量名不区分大小写，注册表里 %username% 这种小写写法是真的存在。
	if got := expandPercentVars(`%download2_test_var%`); got != `C:\展开结果` {
		t.Errorf("小写变量名没展开: %q", got)
	}
}

func TestExpandRegistryPathWithPercent(t *testing.T) {
	t.Setenv("DOWNLOAD2_TEST_HOME", t.TempDir())
	got := expandRegistryPath(`%DOWNLOAD2_TEST_HOME%\Downloads\`)
	if got != filepath.Join(os.Getenv("DOWNLOAD2_TEST_HOME"), "Downloads") {
		t.Errorf("展开 = %q, 尾分隔符该剥掉", got)
	}
	if expandRegistryPath(`%DOWNLOAD2_TEST_HOME%`) != filepath.Clean(os.Getenv("DOWNLOAD2_TEST_HOME")) {
		t.Error("卷根之后没有尾分隔符可剥，别把路径本身剥短了")
	}
}
