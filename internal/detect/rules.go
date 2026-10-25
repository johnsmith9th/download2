package detect

import (
	"path/filepath"
	"strings"
)

// tempSuffixes 是各家下载器在「还没下完」时挂在最终名后面的后缀，全小写比较。
//
// 它们同时是两条判据的原料：带这些后缀的路径永远不当候选；
// 而一个最终名对应的这些名字如果还活着，就说明「还没下完」，那个名字绝不能搬。
var tempSuffixes = []string{
	".crdownload",        // Chrome / Edge
	".part",              // Firefox
	".download",          // Safari
	".partial",           // curl -C 之类
	".tmp",               // 通用
	"._download",         // aria2 风格
	".idmdownload",       // IDM
	".__partialdownload", // 少见浏览器
}

// deniedNames 是「永远不搬」的固定名字：系统缩略图与文件夹配置，搬走会改变目录显示行为，
// 而且它们根本不是下载产物。
var deniedNames = map[string]bool{
	"desktop.ini": true,
	"thumbs.db":   true,
}

// ownerPrefix 是 Office 打开文档时建的属主锁文件。它只有几 KB、立刻就静置，
// 是「只看静置就会误搬」的典型：搬走等于把用户的 Word 会话踢掉。
const ownerPrefix = "~$"

const recycleBin = "$recycle.bin"

// IsTempPath 判断路径本身是不是某个下载的进行时名字。
func IsTempPath(p string) bool {
	return TempSuffixOf(p) != ""
}

// TempSuffixOf 返回 p 末尾的临时后缀（小写形态），不是临时名就返回空串。
func TempSuffixOf(p string) string {
	lower := strings.ToLower(filepath.Base(p))
	for _, s := range tempSuffixes {
		if strings.HasSuffix(lower, s) && len(lower) > len(s) {
			return s
		}
	}
	return ""
}

// FinalPathFor 把临时名还原成它承诺的最终名。还原不出来（后缀正好是整个文件名）就返回空串。
func FinalPathFor(temp string) string {
	s := TempSuffixOf(temp)
	if s == "" {
		return ""
	}
	return temp[:len(temp)-len(s)]
}

// TempPathsFor 列出指向 p 的所有可能临时名。
//
// 这是「没有任何存活的临时路径指向 P」那条判据的实现方式：与其要求 watch 一定配对成功，
// 不如按各家下载器的命名规矩把候选名都推出来再逐个查存活——推错只会让判据更松，
// 而那点松动由静置 + 双采样 + 占用探测兜住。
func TempPathsFor(p string) []string {
	out := make([]string, 0, len(tempSuffixes))
	for _, s := range tempSuffixes {
		out = append(out, p+s)
	}
	return out
}

// IsDenied 判断这个名字有没有资格被搬。目录由 Sample.IsDir 单独判，不在这里。
func IsDenied(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	if deniedNames[base] || base == recycleBin {
		return true
	}
	return strings.HasPrefix(base, ownerPrefix)
}
