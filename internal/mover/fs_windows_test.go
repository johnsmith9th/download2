//go:build windows

package mover

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 这一组跑的是真 Win32：同卷/跨卷的判据和「绝不覆盖」那条底线，
// 用替身是测不出来的 —— 替身会照着我们的假设回答，而这里要验的正是假设本身。

func TestRealVolumeIDAgreesWithinOneVolume(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := volumeID(dir)
	if err != nil {
		t.Fatalf("问目录的卷号: %v", err)
	}
	g, err := volumeID(f)
	if err != nil {
		t.Fatalf("问文件的卷号: %v", err)
	}
	if d != g {
		t.Errorf("同一块盘上的目录与文件给出不同卷号: %d vs %d", d, g)
	}
	t.Logf("本机 %s 的卷号 = %d", dir, d)
	if d == 0 {
		// 0 不是错误：exFAT / 无序列号的盘就是这样。pickMethod 会把这种盘按「不知道」处理。
		t.Logf("这一块的卷号是 0，pickMethod 在这里只会走复制路 —— 有意为之")
	}

	// 深层路径也要问得到同一个卷：转移目录常带年/月两层。
	deep := filepath.Join(dir, "2026", "10", "07")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := volumeID(deep); err != nil || v != d {
		t.Errorf("深层目录卷号 %d, 错误 %v; 想要 %d, 错误 nil", v, err, d)
	}
}

func TestVolumeIDRefusesRelativePath(t *testing.T) {
	if _, err := volumeID(`downloads\a.zip`); err == nil {
		t.Error("相对路径判不出卷，必须报错，不能返回一个看起来能用的号")
	}
}

// TestRenameNoClobberRefusesToEatExistingFile 是全包最值钱的一条：
// 目标名上已经有文件时，我们的改名必须失败并且两边都完好。
func TestRenameNoClobberRefusesToEatExistingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "新下载.zip")
	dst := filepath.Join(dir, "已经在这儿.zip")
	if err := os.WriteFile(src, []byte("新的"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("旧的"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := renameNoClobber(src, dst)
	if err == nil {
		t.Fatal("撞名的改名居然成功了")
	}
	if !isAlreadyThere(err) {
		t.Errorf("错误是 %v, 想要报「目标已存在」那两个码之一", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "旧的" {
		t.Errorf("目标文件被吃掉了: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("源文件也不见了: %v", err)
	}

	// 空位时它照常能改：这一条不通过的话上面那个「失败」就只是根本没在改名。
	if err := renameNoClobber(src, filepath.Join(dir, "空位.zip")); err != nil {
		t.Errorf("改到空位失败: %v", err)
	}
}

// TestOsRenameIsTheHazardWeAvoid 把这条政策的理由钉在实测上，而不是写在注释里靠人信：
// 标准库的改名会**成功并且吃掉**目标文件。
func TestOsRenameIsTheHazardWeAvoid(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, []byte("新"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("旧"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Skipf("这台机器上 os.Rename 撞名会报错，那条政策理由不成立了（%v）", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "新" {
		t.Fatalf("os.Rename 没有覆盖目标（得到 %q）—— 那我们的加号政策要重新评估", got)
	}
	t.Log("实测确认：os.Rename 静默吃掉了目标文件，所以本包一律走 renameNoClobber")
}

// TestErrnoClassification 钉住「哪些码算等一下就好、哪些算别重试」。
// 判错的代价是一整个坏文件把重试预算吃光，或者真的占用一次都不等就被判失败。
func TestErrnoClassification(t *testing.T) {
	cases := []struct {
		err                       error
		busy, exists, crossDevice bool
		what                      string
	}{
		{syscall.Errno(32), true, false, false, "SHARING_VIOLATION"},
		{syscall.Errno(33), true, false, false, "LOCK_VIOLATION"},
		{syscall.Errno(80), false, true, false, "FILE_EXISTS"},
		{syscall.Errno(183), false, true, false, "ALREADY_EXISTS"},
		{syscall.Errno(17), false, false, true, "NOT_SAME_DEVICE"},
		{syscall.Errno(5), false, false, false, "ACCESS_DENIED"},
		{syscall.Errno(2), false, false, false, "FILE_NOT_FOUND"},
		{os.ErrNotExist, false, false, false, "Go 的普通错误"},
	}
	for _, c := range cases {
		if got := isBusy(c.err); got != c.busy {
			t.Errorf("%s 算不算占用 = %v, 想要 %v", c.what, got, c.busy)
		}
		if got := isAlreadyThere(c.err); got != c.exists {
			t.Errorf("%s 算不算撞名 = %v, 想要 %v", c.what, got, c.exists)
		}
		if got := isCrossDevice(c.err); got != c.crossDevice {
			t.Errorf("%s 算不算跨卷 = %v, 想要 %v", c.what, got, c.crossDevice)
		}
	}
	// 包在 PathError 里的码也要认得：Go 的 OS 错误几乎都这么包一层。
	if !isBusy(&os.PathError{Op: "rename", Err: syscall.Errno(32)}) {
		t.Error("PathError 包起来的占用没被认出来")
	}
}

func TestLongPathShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`F:\a\b.txt`, `\\?\F:\a\b.txt`},
		{`\\srv\share\a.txt`, `\\?\UNC\srv\share\a.txt`},
		{`\\?\F:\a.txt`, `\\?\F:\a.txt`}, // 已经加过就不再加
		{`相对\路径.txt`, `相对\路径.txt`},       // 猜基准目录等于往别的地方写文件
		{``, ``},
	}
	for _, c := range cases {
		if got := longPath(c.in); got != c.want {
			t.Errorf("longPath(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}
}

// TestVolumeRootEndsInSeparator 是 volumeID 正确性的前提：
// GetVolumeInformationByHandle 要的是卷根，"F:" 和 "F:\" 不是同一个开法。
func TestVolumeRootEndsInSeparator(t *testing.T) {
	for _, p := range []string{`F:\a\b.txt`, `F:\`, `C:\Users\x\Downloads`} {
		root, err := volumeRoot(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(root, `\`) {
			t.Errorf("%s 的卷根是 %q, 想要以分隔符结尾", p, root)
		}
		if root != filepath.VolumeName(p)+`\` {
			t.Errorf("%s 的卷根是 %q", p, root)
		}
	}
	root, err := volumeRoot(`\\srv\share\a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if root != `\\srv\share\` {
		t.Errorf("UNC 的卷根是 %q, 想要分享根 \\\\srv\\share\\", root)
	}
}
