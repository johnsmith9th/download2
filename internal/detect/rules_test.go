package detect

import "testing"

func TestTempSuffixRecognition(t *testing.T) {
	cases := []struct {
		in     string
		suffix string // 想要的临时后缀，空串表示不是临时名
		final  string // 还原出来的最终名
	}{
		{`D:\Downloads\a.zip.crdownload`, ".crdownload", `D:\Downloads\a.zip`},
		{`D:\Downloads\a.zip.part`, ".part", `D:\Downloads\a.zip`},
		{`D:\Downloads\a.zip.PART`, ".part", `D:\Downloads\a.zip`}, // 大小写不许影响判定
		{`D:\Downloads\a.zip.download`, ".download", `D:\Downloads\a.zip`},
		{`D:\Downloads\a.zip.__partialdownload`, ".__partialdownload", `D:\Downloads\a.zip`},
		{`D:\Downloads\发票.pdf.IDMDownload`, ".idmdownload", `D:\Downloads\发票.pdf`},
		{`D:\Downloads\setup.exe.tmp`, ".tmp", `D:\Downloads\setup.exe`},
		{`D:\Downloads\x.7z._download`, "._download", `D:\Downloads\x.7z`},
		{`D:\Downloads\x.7z.partial`, ".partial", `D:\Downloads\x.7z`},
		{`D:\Downloads\正常文件.zip`, "", ""},
		{`D:\Downloads\part`, "", ""},
		// 整个文件名就是一个后缀：没有「最终名」可还原，别把它当成半个下载。
		{`D:\Downloads\.part`, "", ""},
		{`D:\Downloads\.tmp`, "", ""},
		// 后缀只在末尾才算数。
		{`D:\Downloads\a.part.zip`, "", ""},
		{`D:\Downloads\报告.docx`, "", ""},
	}
	for _, c := range cases {
		if got := TempSuffixOf(c.in); got != c.suffix {
			t.Errorf("TempSuffixOf(%q) = %q, 想要 %q", c.in, got, c.suffix)
		}
		if got := IsTempPath(c.in); got != (c.suffix != "") {
			t.Errorf("IsTempPath(%q) = %v, 想要 %v", c.in, got, c.suffix != "")
		}
		if got := FinalPathFor(c.in); got != c.final {
			t.Errorf("FinalPathFor(%q) = %q, 想要 %q", c.in, got, c.final)
		}
	}
}

func TestIsDenied(t *testing.T) {
	yes := []string{
		`D:\Downloads\~$报表.docx`,   // Office 属主锁文件
		`D:\Downloads\~$alone`,     // 没有扩展名也得拒
		`D:\Downloads\desktop.ini`, // 大小写都要认
		`D:\Downloads\DESKTOP.INI`,
		`D:\Downloads\Thumbs.db`,
		`D:\Downloads\$Recycle.Bin`,
		`D:\Downloads\sub\desktop.ini`, // 拒绝只看文件名，不看它在哪一层
	}
	for _, p := range yes {
		if !IsDenied(p) {
			t.Errorf("%q 该被拒", p)
		}
	}

	no := []string{
		`D:\Downloads\发票.pdf`,
		`D:\Downloads\~report.docx`,      // 只有 ~$ 前缀才是锁文件
		`D:\Downloads\我的desktop.ini.txt`, // 不是整个名字
		`D:\Downloads\ini`,
	}
	for _, p := range no {
		if IsDenied(p) {
			t.Errorf("%q 不该被拒", p)
		}
	}
}

// TempPathsFor 是「临时名还挂着吗」这条判据的搜索集合，必须一条后缀都不漏。
func TestTempPathsForCoversEverySuffix(t *testing.T) {
	got := TempPathsFor(`D:\Downloads\a.zip`)
	if len(got) != len(tempSuffixes) {
		t.Fatalf("推出 %d 个候选临时名, 后缀清单有 %d 条", len(got), len(tempSuffixes))
	}
	seen := map[string]bool{}
	for i, p := range got {
		if seen[p] {
			t.Errorf("第 %d 条重复: %q", i, p)
		}
		seen[p] = true
		if p[:len(p)-len(tempSuffixes[i])] != `D:\Downloads\a.zip` {
			t.Errorf("%q 不是在最终名后面加后缀得到的", p)
		}
	}

	// 推出来的名字必须都能被认回临时名，否则判据搜到的东西自己都不认。
	for _, p := range got {
		if !IsTempPath(p) {
			t.Errorf("TempPathsFor 造出了认不出来的名字 %q", p)
		}
	}
}
