package mover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"download2/internal/journal"
)

func TestSplitExt(t *testing.T) {
	cases := []struct{ in, stem, ext string }{
		{"发票.pdf", "发票", ".pdf"},
		{"archive.tar.gz", "archive.tar", ".gz"},
		{"LICENSE", "LICENSE", ""},
		// 点开头、其实没有扩展名的名字：filepath.Ext 会把整个名字当扩展名，
		// 那样主名是空的，加号会变成「 (1).gitignore」。
		{".gitignore", ".gitignore", ""},
		{"无扩展名", "无扩展名", ""},
		{"报告.最终版", "报告", ".最终版"},
	}
	for _, c := range cases {
		stem, ext := splitExt(c.in)
		if stem != c.stem || ext != c.ext {
			t.Errorf("splitExt(%q) = (%q,%q), 想要 (%q,%q)", c.in, stem, ext, c.stem, c.ext)
		}
		if stem+ext != c.in {
			t.Errorf("拆完拼不回去: %q + %q", stem, ext)
		}
	}
}

func TestMarker(t *testing.T) {
	if marker(0) != "" {
		t.Errorf("第一次不该有缀号, 得到 %q", marker(0))
	}
	if marker(1) != " (1)" {
		t.Errorf("marker(1) = %q, 想要 \" (1)\"", marker(1))
	}
	if marker(12) != " (12)" {
		t.Errorf("marker(12) = %q", marker(12))
	}
}

// TestPickNameSkipsTaken 是加号那条主路：占了一个就跳下一个，一次都不许覆盖。
func TestPickNameSkipsTaken(t *testing.T) {
	dir := t.TempDir()
	taken := []string{"发票.pdf", "发票 (1).pdf"}
	for _, n := range taken {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("旧"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pickName(dir, "发票.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "发票 (2).pdf"); got != want {
		t.Errorf("挑到 %s, 想要 %s", got, want)
	}
}

// TestPickNameKeepsDirectoryOutOfTheWay 验目录也算占了名字：
// 往「归档」这种同名目录里改名会直接失败，而提前让开是更便宜的做法。
func TestPickNameKeepsDirectoryOutOfTheWay(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "笔记"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := pickName(dir, "笔记")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "笔记 (1)"); got != want {
		t.Errorf("挑到 %s, 想要 %s", got, want)
	}
}

// TestPickNameSqueezesLongNames 验长名字那条：削主名、保扩展名、保号。
//
// 这一条要防的是「文件名一长就整个搬不动」：下载目录里带长中文名的 zip 很常见，
// 而目标路径还要再挂一层 .download2.tmp 当中间形态。
func TestPickNameSqueezesLongNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("归", 40))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("中", 120) + ".zip"

	got, err := pickName(dir, long)
	if err != nil {
		t.Fatalf("长名字就该削主名，而不是报错: %v", err)
	}
	if len(got) > maxPathLen {
		t.Errorf("落地路径 %d 字符, 超出预算 %d", len(got), maxPathLen)
	}
	base := filepath.Base(got)
	if !strings.HasSuffix(base, ".zip") {
		t.Errorf("扩展名丢了: %q", base)
	}
	if !utf8.ValidString(base) {
		t.Errorf("主名被按字节劈开了，得到非法 UTF-8: %q", base)
	}

	// 削完的名字先被占了，就得再挂号 —— 号也要在预算内。
	if err := os.WriteFile(got, []byte("旧"), 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := pickName(dir, long)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got2, " (1).zip") {
		t.Errorf("第二次挑到 %q, 想要带 1 号", got2)
	}
	if len(got2) > maxPathLen {
		t.Errorf("加了号就超预算: %d 字符", len(got2))
	}
	if !utf8.ValidString(filepath.Base(got2)) {
		t.Errorf("加号那次把主名劈坏了: %q", got2)
	}
}

// TestPickNameGivesUpAfterThousand 钉住上界：一千个同名就该报错停下，而不是一路加下去。
func TestPickNameGivesUpAfterThousand(t *testing.T) {
	dir := t.TempDir()
	for n := 0; n <= maxConflictN; n++ {
		name := "x" + marker(n) + ".pdf"
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pickName(dir, "x.pdf"); err == nil {
		t.Fatalf("加到 %d 号还继续要名字，那不是搬运是刷文件名", maxConflictN)
	}
}

func TestPathFree(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "有文件.txt")
	d := filepath.Join(dir, "有目录")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		free bool
	}{
		{filepath.Join(dir, "没有.txt"), true},
		{f, false},
		{d, false},
		// 目录本身不算「有东西」在这个包里的含义是「不能落地」：它存在，所以不空。
		{dir, false},
	}
	for _, c := range cases {
		if got := pathFree(c.path); got != c.free {
			t.Errorf("pathFree(%q) = %v, 想要 %v", c.path, got, c.free)
		}
	}
	// 看不清楚的路径一律当「占了」：宁可可让一个号，也不覆盖看不清的东西。
	if pathFree(filepath.Join(dir, "缺目录", "子.txt")) != true {
		t.Error("父目录都不存在时该判成空位（ENOENT 就是没被占）")
	}
}

// TestTmpNameVariants 验两种中间名都以 TmpSuffix 结尾 ——
// 账本的 HasOwnTmp 判据对它们都得成立，否则崩溃重放时我们没资格删自己的东西。
func TestTmpNameVariants(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "movie.mp4")
	if got := tmpName(final, "abcdef1234567890"); got != final+journal.TmpSuffix {
		t.Errorf("空位时该用默认中间名, 得到 %q", got)
	}

	taken := final + journal.TmpSuffix
	if err := os.WriteFile(taken, []byte("别人的"), 0o600); err != nil {
		t.Fatal(err)
	}
	alt := tmpName(final, "abcdef1234567890")
	if alt == taken {
		t.Fatal("默认中间名被占了还返回同一个名字")
	}
	if !strings.HasSuffix(alt, journal.TmpSuffix) {
		t.Errorf("带 ID 的形态 %q 不以 TmpSuffix 结尾", alt)
	}
	if len(alt) > len(taken)+9 {
		t.Errorf("ID 层没截短: %q", alt)
	}
}
