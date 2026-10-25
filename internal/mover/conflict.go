package mover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// maxConflictN 是同名后缀加到几号就停手。
//
// 一个目录里挤了一千个「发票 (999).pdf」说明那里已经有人在当仓库用了，
// 那不是下载器之间的偶发撞名；继续加号只会把文件名变成谁也认不出来的东西。
const maxConflictN = 999

// maxPathLen 是落地路径的软上限。
//
// Win32 的常规上限是 260，而我们的目标名还要再挂一层 ".download2.tmp" 当中间形态（14 个字符），
// 所以名字必须压在 240 以内，否则复制路上会造出一个开不了的名字。
// 超限时削的是主名而不是后缀：扩展名一丢，文件就再也打不开了。
//
// 长度一律按字节算。中文目录名按 UTF-16 计数其实更短，所以这个算法偏保守 ——
// 保守的方向是对的：宁可少留一个字，也不要造出一个开不了的路径。
const maxPathLen = 240

// pickName 返回 dir 下面第一个没被占用的名字，形态是「发票 (1).pdf」。
//
// 为什么无条件加号、连「同名覆盖」的开关都不做：Windows 上改名撞上已存在的目标名
// 是**会成功并且吃掉目标文件**的。把用户重新下载的「发票.pdf」静默盖掉归档里的旧版
// 是不可恢复的数据丢失，而且流水上看不出来。加后缀和浏览器自己的行为一致。
//
// 这里只负责「尽量挑一个空位」。真正的保证在 renameNoClobber 那一层：
// 挑完之后有人抢先把名字用掉时，改名会失败而不会覆盖，调用方据此重挑一次。
func pickName(dir, name string) (string, error) {
	stem, ext := splitExt(name)
	for n := 0; n <= maxConflictN; n++ {
		mk := marker(n)
		body := stem
		if len(dir)+1+len(body)+len(mk)+len(ext) > maxPathLen {
			body = squeeze(stem, dir, mk, ext)
		}
		p := filepath.Join(dir, body+mk+ext)
		if pathFree(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s 的同名文件太多（超过 %d 个）", name, maxConflictN)
}

// splitExt 拆主名与扩展名。
//
// filepath.Ext 把「最后一个点之后的整段」当扩展名，所以 ".gitignore" 这类点开头、
// 其实没有扩展名的名字会被整个当成扩展名 —— 那样主名成了空串，加号会变成「 (1).gitignore」。
// 这种名字按「没有扩展名」处理才对。
func splitExt(name string) (stem, ext string) {
	e := filepath.Ext(name)
	if e == "" || e == name {
		return name, ""
	}
	return name[:len(name)-len(e)], e
}

// marker 是第 n 号挂缀；n=0 表示不撞名，原样。
func marker(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d)", n)
}

// squeeze 把主名截短到刚好放得下。
//
// 一次去掉一个 rune，绝不按字节切：中文文件名在这里最常见，
// 按字节截会把一个汉字劈成半个，得到一个非法 UTF-8 的名字，界面和资源管理器都显示成乱码方块。
func squeeze(stem, dir, mk, ext string) string {
	room := maxPathLen - len(dir) - 1 - len(mk) - len(ext)
	if room <= 0 {
		// 连目录本身都吃满了预算：只能把主名让光，号与扩展名一个都不动。
		return ""
	}
	out := stem
	for len(out) > room && out != "" {
		_, sz := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-sz]
	}
	return out
}

// pathFree 判断这个路径上有没有东西。
//
// 用 Lstat 而不是 Stat：一个指向不存在目标的链接也占着那个名字，
// 而 Stat 对它报 ErrNotExist，于是我们对着一个用户看得见的名牌又造一个文件。
// 看不清楚的失败（权限等）一律当「占了」：宁可往后挪一号，也不覆盖看不清的东西。
func pathFree(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, os.ErrNotExist)
}
