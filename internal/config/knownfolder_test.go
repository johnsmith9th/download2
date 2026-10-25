package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DownloadsDir 是本工具唯一「替用户决定监哪里」的地方，所以它必须给出一个真的存在的目录。
// 只读，不写：这条测试不碰下载目录里的任何东西。
func TestDownloadsDirResolvesToRealFolder(t *testing.T) {
	p, err := DownloadsDir()
	if err != nil {
		t.Fatalf("DownloadsDir: %v", err)
	}
	if p == "" || !filepath.IsAbs(p) {
		t.Fatalf("%q 不是绝对路径", p)
	}
	// 指到卷根是灾难：非递归监听会把整个盘的顶层事件都当成「新下载」。
	if isDriveRoot(p) {
		t.Fatalf("%q 是盘符根", p)
	}
	if !strings.EqualFold(filepath.Base(p), "Downloads") && !strings.EqualFold(filepath.Base(p), "下载") {
		t.Logf("目录名是 %q——OneDrive 或本地化改过名，能存在就照它监", filepath.Base(p))
	}
	if strings.HasSuffix(p, `\`) || strings.HasSuffix(p, "/") {
		t.Errorf("%q 带了尾分隔符，会让 within() 多比出一层空段", p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("解析出来的目录不存在，监听它等于什么都不搬: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%q 不是目录", p)
	}
	// 这条不是断言，只是让「下载目录挂在哪盘」在测试输出里看得见。
	t.Logf("下载目录的父路径：%s", filepath.Dir(p))
}
