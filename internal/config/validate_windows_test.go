//go:build windows

package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"download2/internal/apperr"
)

// 这几条测的是盘符与系统目录形态，只在 Windows 的路径语法下成立，
// 所以整档打了 windows 标签——共享那份 config_test.go 因此能保持跨平台。

func TestValidateRejectsSystemLandingSpots(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Downloads")

	cases := []struct {
		name    string
		target  string
		wantMsg string
	}{
		{"盘符根", `D:\`, "根目录"},
		{"盘符根不带斜杠", `D:`, "根目录"},
		{"Windows 里面", `C:\Windows\Temp`, "系统目录"},
		{"就是 Windows", `C:\Windows`, "系统目录"},
		{"Program Files", `C:\Program Files\下载`, "系统目录"},
		{"Program Files (x86)", `C:\Program Files (x86)`, "系统目录"},
		{"ProgramData", `C:\ProgramData\下载`, "系统目录"},
		{"UNC 盘符根", `\\nas\files`, "根目录"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Default()
			s.SourceDir, s.TargetDir = src, c.target
			err := s.Validate()
			if err == nil {
				t.Fatalf("%q 竟然合法", c.target)
			}
			e := apperr.Of(err)
			if e.Code != apperr.EPathForbidden {
				t.Errorf("码 = %s, 想要 E_PATH_FORBIDDEN", e.Code)
			}
			if !strings.Contains(e.Msg, c.wantMsg) {
				t.Errorf("文案 %q 里没有 %q", e.Msg, c.wantMsg)
			}
		})
	}
}

// 用户自己的文件夹里带「windows」字样不能误杀——误杀的代价是他找不到一个能保存的路径。
func TestSystemDirRuleDoesNotEatLookalikeNames(t *testing.T) {
	base := strings.ToUpper(filepath.VolumeName(t.TempDir())[:1]) + ":"
	for _, p := range []string{
		base + `\windows备份`,
		base + `\Program Files 存档`,
		base + `\ProgramDatax`,
		filepath.Join(t.TempDir(), "Windows"),
	} {
		if inSystemDir(p) {
			t.Errorf("%q 被误判成系统目录", p)
		}
	}
	for _, p := range []string{`C:\Windows`, `c:\windows\temp`, `C:\Program Files (x86)\x`, `C:\ProgramData`} {
		if !inSystemDir(p) {
			t.Errorf("%q 没被认出是系统目录", p)
		}
	}
	// UNC 分享根下恰好同名的文件夹不该被当成系统目录——我们本来就允许网络路径。
	if inSystemDir(`\\nas\files\Windows`) {
		t.Error("网络分享里的 Windows 目录被误杀")
	}
}

// 常用列表里不留天生不能当落点的那几档：手改过 settings.json、或旧版本存下的
// `D:\`、`C:\Windows` 都要在读取那一刻就被剔掉，否则那颗 ▾ 上会出现一行「点下去必然报错」的目录。
// UNC 的分享根同一条（isDriveRoot 认它，见 validate.go 的注释），所以它也进不了列表。
func TestCleanPresetsDropsUnusableShapes(t *testing.T) {
	ok := filepath.Join(t.TempDir(), "归档")
	got := cleanPresets([]string{`D:\`, `D:`, `C:\Windows`, `C:\Program Files`, `\\srv\share`, ok})
	want := []string{ok}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cleanPresets = %q, 想要 %q", got, want)
	}
}

func TestIsDriveRoot(t *testing.T) {
	for _, p := range []string{`D:`, `D:\`, `C:\`, `\\nas\files`} {
		if !isDriveRoot(p) {
			t.Errorf("%q 是卷根却没认出来", p)
		}
	}
	for _, p := range []string{`D:\a`, ``, `C:\Windows`} {
		if isDriveRoot(p) {
			t.Errorf("%q 不是卷根", p)
		}
	}
}

// 盘符根与系统目录都在同一个源目录之下时，先拒哪个都要说得清——
// 这条钉住「自搬环」优先于「系统目录」，因为它的后果更严重。
func TestSelfLoopBeatsSystemDirInReporting(t *testing.T) {
	s := Default()
	s.SourceDir = `C:\Windows`
	s.TargetDir = `C:\Windows\Downloads`
	err := apperr.Of(s.Validate())
	if err.Code != apperr.EPathForbidden {
		t.Fatalf("码 = %s", err.Code)
	}
	if !strings.Contains(err.Msg, "再搬一次") {
		t.Errorf("应该先说自搬环, 得到 %q", err.Msg)
	}
}

// Windows 路径既不区分大小写、也不区分分隔符方向。用 == 会比出假阴性，
// 而假阴性的后果是「目标其实就在 Downloads 里却判不出来」→ 自搬环。
func TestWithinAndSamePathIgnoreSeparators(t *testing.T) {
	if !samePath(`C:\Users\A\Downloads\`, `c:\users\a\downloads`) {
		t.Error("同一个目录判成不同")
	}
	if samePath(`C:\Users\A\Downloads`, `C:\Users\A\Downloads2`) {
		t.Error("前缀相同的两个目录判成同一个")
	}
	if !within(`C:\a`, `C:\a\b`) {
		t.Error("子目录判成不在里面")
	}
	if !within(`C:\a\`, `c:\A\b\c`) {
		t.Error("混合分隔符加大小写就判不出层级")
	}
	if within(`C:\a`, `C:\ab`) {
		t.Error(`C:\ab 并不在 C:\a 里面`)
	}
	if within(`C:\a`, `C:\a`) {
		t.Error("等于自身不算在里面")
	}
	if samePath("", `C:\a`) || within("", `C:\a`) {
		t.Error("空路径不参与比较，否则会误判成「相同」")
	}
}

// 手输 "D:" 若被 Abs 解析成 D 盘的当前目录，「不能搬到盘根」这条规则就形同虚设：
// 用户会以为自己在保护根目录，实际我们把文件倒进了某个想不到的深层文件夹。
func TestCleanAbsPinsVolumeRoots(t *testing.T) {
	for _, p := range []string{`D:`, `d:`, `D:\`, `d:\`} {
		if !isDriveRoot(cleanAbs(p)) {
			t.Errorf("cleanAbs(%q) = %q, 判不出卷根", p, cleanAbs(p))
		}
	}
	if got := cleanAbs(`\\nas\share`); !isDriveRoot(got) {
		t.Errorf("UNC 分享根没认出来: %q", got)
	}
	if got := cleanAbs(`\\nas\share\`); !isDriveRoot(got) {
		t.Errorf("带尾斜杠的 UNC 分享根没认出来: %q", got)
	}
	// 深一层就不该再被当根。
	if isDriveRoot(cleanAbs(`E:\下载`)) {
		t.Error(`E:\下载 不是卷根`)
	}
}
