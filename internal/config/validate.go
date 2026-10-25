package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"download2/internal/apperr"
)

// cleanAbs 把用户给的路径收成绝对、Clean 过、不带尾分隔符的形式。
// 空串原样返回空——「没配」和「配错了」是两件事，得让 Ready() 与 Validate() 分别说。
//
// 这里**不做环境变量展开**：只有注册表里的 REG_EXPAND_SZ 才是 `%VAR%` 形态，
// 那由 expandRegistryPath 负责。对任意路径展开会把 `D:\50%off%促销` 这种真实文件夹名吃掉一段，
// 而「用户手输的路径莫名其妙变短」是比「没展开」难查得多的故障。
func cleanAbs(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// 只写了盘符或 UNC 分享根时（"D:"、"\\srv\share"），后面的 Abs 会把它解析成
	// 「该盘的当前目录」，于是「不能是盘根」这条规则对最常见的手输形态漏判。
	// 补一个分隔符逼出根，末尾的分隔符下面照样会剥掉。
	if vol := filepath.VolumeName(p); vol != "" && len(p) == len(vol) {
		p += string(filepath.Separator)
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	p = filepath.Clean(p)
	return strings.TrimRight(p, string(filepath.Separator))
}

// CleanAbs 是 cleanAbs 留给包外的那一个入口：启动参数带来的路径要先收成同一个形状，
// 才谈得上和设置里那一份比（口径与读设置文件时完全一样，包括「不做环境变量展开」）。
func CleanAbs(p string) string { return cleanAbs(p) }

// samePath：Windows 路径不区分大小写也不区分分隔符方向，直接用 == 会比出假阴性，
// 而假阴性的后果是「target 其实就在 Downloads 里却判不出来」→ 自搬环。
//
// SamePath 是同一条口径留给包外的入口（比较启动参数那份与面板送回来的那份）。
func SamePath(a, b string) bool { return samePath(a, b) }

func samePath(a, b string) bool {
	a, b = cleanAbs(a), cleanAbs(b)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// within 报告 child 是否位于 parent 之内（不含等于自身）。
func within(parent, child string) bool {
	parent, child = cleanAbs(parent), cleanAbs(child)
	if parent == "" || child == "" || parent == child {
		return false
	}
	prefix := parent + string(filepath.Separator)
	return strings.HasPrefix(strings.ToLower(child), strings.ToLower(prefix))
}

// isDriveRoot 认盘符根（D: → 清完是 "D:"）和 UNC 分享根（\\srv\share）。
// 这两种的共同点是 VolumeName 吃掉了整个路径，往里搬东西等于往卷的顶层倒垃圾。
func isDriveRoot(p string) bool {
	p = cleanAbs(p)
	vol := filepath.VolumeName(p)
	return vol != "" && p == vol
}

// forbiddenTargets 是几条「技术上能写但搬进去就是灾难」的落点。
// 盘符根会把用户文件混进回收站/系统目录那一层；Windows 与 Program Files 需要管理员权限，
// 搬进去等于让每次搬运都在权限边界上失败。
var forbiddenTargets = []string{
	`\Windows`,
	`\Program Files`,
	`\Program Files (x86)`,
	`\ProgramData`,
}

// inSystemDir 认「就是系统目录，或在它里面」。
//
// 只看盘符卷，不拿 Contains 去比：`F:\windows备份` 这种用户自己的文件夹里确实可能有
// "\windows" 这几个字，用子串会把它误杀；而网络分享根下的 \Windows 只是恰好同名。
// 代价是 UNC 上的系统路径不再特殊处理——我们本来就允许网络路径，且那里没有 Windows 目录可言。
func inSystemDir(p string) bool {
	p = cleanAbs(p)
	vol := filepath.VolumeName(p)
	if len(vol) != 2 || vol[1] != ':' {
		return false
	}
	rest := strings.ToLower(strings.TrimPrefix(p, vol))
	for _, bad := range forbiddenTargets {
		b := strings.ToLower(bad)
		if rest == b || strings.HasPrefix(rest, b+`\`) {
			return true
		}
	}
	return false
}

// Validate 是唯一的路径口径来源：界面保存前调它，mover 干活前也调它。
// 两处共用一份，避免「界面放行了、搬运时才发现不对」那种半截配置。
func (s Settings) Validate() error {
	if s.SourceDir == "" {
		return apperr.New(apperr.EInvalidArg, "监控目录没配上")
	}
	if s.TargetDir == "" {
		return apperr.New(apperr.EInvalidArg, "先选一个转移目录")
	}

	if samePath(s.SourceDir, s.TargetDir) {
		return apperr.New(apperr.EPathForbidden, "转移目录不能就是监控目录本身").With("target", s.TargetDir)
	}
	// 这一条是本工具唯一能自我毁灭的方式：目标在源里面，刚搬过去的文件马上会被再看见一遍
	if within(s.SourceDir, s.TargetDir) {
		return apperr.New(apperr.EPathForbidden, "转移目录不能在监控目录里面，否则会把搬过去的文件再搬一次").
			With("target", s.TargetDir).With("source", s.SourceDir)
	}

	if isDriveRoot(s.TargetDir) {
		return apperr.New(apperr.EPathForbidden, "转移目录不能是一个盘或网络共享的根目录").With("target", s.TargetDir)
	}
	if inSystemDir(s.TargetDir) {
		return apperr.New(apperr.EPathForbidden, "转移目录落在系统目录下，不适合放下载的文件").With("target", s.TargetDir)
	}

	// 下限 200ms 不是拍脑袋：静置判据要连续两次 500ms 采样一致，阈值再低就形同「只凭改名就搬」，
	// 而允许 0 会和 normalize() 的「0 回落默认」打对——同一个数字两处口径不同，是最难读的 bug。
	if s.QuietMs < 200 || s.QuietMs > 5000 {
		return apperr.New(apperr.EInvalidArg, "静置阈值只能在 200 到 5000 毫秒之间").With("quietMs", itoa(s.QuietMs))
	}
	if s.MaxSettleMs < 5_000 {
		return apperr.New(apperr.EInvalidArg, "最长等待不能小于 5 秒").With("maxSettleMs", itoa(s.MaxSettleMs))
	}
	if s.ToastSeconds < 1 || s.ToastSeconds > 15 {
		return apperr.New(apperr.EInvalidArg, "提示停留秒数得在 1 到 15 之间").With("toastSeconds", itoa(s.ToastSeconds))
	}
	return nil
}

// EnsureTarget 建出转移目录。校验通过但建不出来（网络盘断了、只读、没权限）是另一类失败，
// 得分开报，因为用户在这两种情况下要做的事完全不同。
func EnsureTarget(target string) error {
	target = cleanAbs(target)
	if target == "" {
		return apperr.New(apperr.EInvalidArg, "先选一个转移目录")
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "建不出转移目录", err).With("target", target)
	}
	return nil
}

func itoa(v int) string { return strconv.Itoa(v) }
