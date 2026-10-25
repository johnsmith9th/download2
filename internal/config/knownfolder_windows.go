//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"

	"download2/internal/apperr"
)

// FOLDERID_Downloads 的 GUID。用户壳目录键里既可能有这个 GUID 值，也可能有老式的 "Downloads" 值。
const downloadsFolderGUID = "{377DE29E-F132-414D-8CDB-13B245F1B833}"

const userShellFoldersKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`

// DownloadsDir 解析当前用户的下载目录。
//
// 为什么不直接拼 home\Downloads：OneDrive 接管下载目录时只有注册表这个键会跟着变。
// 硬拼家目录会让我们监一个根本没人写文件的空文件夹——那是最难查的失败形态：
// 进程活着、事件一个不来、用户只觉得「这工具没反应」。
func DownloadsDir() (string, error) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, userShellFoldersKey, registry.QUERY_VALUE); err == nil {
		defer k.Close()
		for _, name := range []string{downloadsFolderGUID, "Downloads"} {
			if raw, _, err := k.GetStringValue(name); err == nil {
				if p := expandRegistryPath(raw); p != "" {
					return p, nil
				}
			}
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", apperr.Wrap(apperr.EFileOpFailed, "找不到用户目录", err)
	}
	return filepath.Join(home, "Downloads"), nil
}

// expandRegistryPath 处理 REG_EXPAND_SZ 里 %USERPROFILE%\Downloads 这种未展开值。
// 展开完还是相对路径，就说明这个值不可用，返回空串让调用方继续往下找。
func expandRegistryPath(raw string) string {
	p := filepath.Clean(expandPercentVars(raw))
	if !filepath.IsAbs(p) {
		return ""
	}
	// 尾分隔符留着会让 samePath/within 的比较多出一层空段
	for len(p) > len(filepath.VolumeName(p)) && (p[len(p)-1] == '\\' || p[len(p)-1] == '/') {
		p = p[:len(p)-1]
	}
	return p
}

// expandPercentVars 按 Win32 的规矩展开 %NAME%：os.ExpandEnv 只认 $NAME，
// 拿它处理注册表值会原样返回 "%USERPROFILE%\Downloads"，然后 filepath.IsAbs 判假，
// 下载目录就解析失败了——症状是「进程活着但一个文件都不搬」。
//
// %% 是转义的字面百分号；落单的 % 后面没有配对，就原样留着。
// os.Getenv 在 Windows 上不区分大小写，正好对上注册表里的 %USERNAME% 这类写法。
func expandPercentVars(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(p, '%')
		if i < 0 {
			b.WriteString(p)
			break
		}
		b.WriteString(p[:i])
		rest := p[i+1:]
		j := strings.IndexByte(rest, '%')
		if j < 0 {
			b.WriteString("%")
			p = rest
			continue
		}
		if name := rest[:j]; name == "" {
			b.WriteString("%")
		} else {
			b.WriteString(os.Getenv(name))
		}
		p = rest[j+1:]
	}
	return b.String()
}
