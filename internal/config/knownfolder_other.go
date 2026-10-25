//go:build !windows

package config

import (
	"os"
	"path/filepath"

	"download2/internal/apperr"
)

// DownloadsDir 的非 Windows 退路。本工具只发 Windows，这份存在的意义是让
// internal/config 的单测能在任何平台上跑，不代表支持了那些系统。
func DownloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", apperr.Wrap(apperr.EFileOpFailed, "找不到用户目录", err)
	}
	return filepath.Join(home, "Downloads"), nil
}
