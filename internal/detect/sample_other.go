//go:build !windows

package detect

import "os"

// StatSample 在非 Windows 上没有 fileIndex 可用，身份只能靠 size + mtime。
//
// 这个文件存在的唯一理由是让测试跨平台跑得起；本产品只发布 Windows 版，
// 这里的放宽不算被验证过的行为。
func StatSample(path string) (Sample, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Sample{}, err
	}
	return Sample{
		Size:   fi.Size(),
		Mtime:  fi.ModTime(),
		IsDir:  fi.IsDir(),
		Hidden: len(fi.Name()) > 0 && fi.Name()[0] == '.',
	}, nil
}
