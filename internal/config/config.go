package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"download2/internal/apperr"
)

const Version = 1

// Anchor 存的是药丸**到所在工作区右/下边的物理像素距离**，而不是绝对 x/y。
// 理由：换显示器或改分辨率时绝对坐标会落到屏幕外，而右下角的相对位置几乎总是有意义的那个位置。
type Anchor struct {
	Expanded    bool `json:"expanded"`
	RightInset  int  `json:"rightInset"`
	BottomInset int  `json:"bottomInset"`
}

type Settings struct {
	Version      int    `json:"version"`
	SourceDir    string `json:"sourceDir"`
	TargetDir    string `json:"targetDir"`
	DateSubdirs  bool   `json:"dateSubdirs"`
	ToastEnabled bool   `json:"toastEnabled"`
	ToastSeconds int    `json:"toastSeconds"`
	Paused       bool   `json:"paused"`
	QuietMs      int    `json:"quietMs"`
	MaxSettleMs  int    `json:"maxSettleMs"`
	Anchor       Anchor `json:"anchor"`
	Theme        string `json:"theme"`
	Lang         string `json:"lang"`
	// TargetDirPresets 是面板上那颗 ▾ 里的常用转移目录，最近用的在最前。
	// 它是一份「大概还要回去」的清单，不是一次判决：里面的目录可以不在线（网络盘断了、
	// U 盘拔了），所以列表本身不参与 Validate —— 选中那一刻才走完整的那套。
	TargetDirPresets []string `json:"targetDirPresets,omitempty"`
}

// MaxTargetDirPresets 是那颗下拉最多列几条。再多就超出面板那 340 DIP 一眼看完的量，
// 而「常用」本来就该是最近用上的那几档。
const MaxTargetDirPresets = 8

// 两套配色。值就是 html[data-theme] 的那个词，前端不再翻译一遍。
const (
	ThemeDark  = "dark"
	ThemeLight = "light"
)

// 三档界面语言。值进 settings.json，也由前端拿去查 i18n 的字典 ——
// 后端一处都不读它（和 Theme 同类）：翻哪句、怎么翻是前端的事，Go 只负责把这个偏好存住。
const (
	LangAuto = "auto" // 由前端的 navigator.language 猜
	LangZh   = "zh"
	LangEn   = "en"
)

func Default() Settings {
	return Settings{
		Version:      Version,
		DateSubdirs:  true,
		ToastEnabled: true,
		ToastSeconds: 4,
		QuietMs:      900,
		MaxSettleMs:  600_000,
		Anchor:       Anchor{RightInset: 24, BottomInset: 24},
		Theme:        ThemeDark,
		Lang:         LangAuto,
	}
}

// Ready 是「能开始搬东西」的唯一判据。转移目录没配上之前，程序活着但不动任何文件，
// 界面据此显示未配置态——不能拿一个猜出来的默认目录去搬用户的文件。
func (s Settings) Ready() bool {
	return s.SourceDir != "" && s.TargetDir != ""
}

type Store struct {
	Dir  string
	path string
}

// NewStore 的 dir 为空时用 AppDataDir()。测试一律传 %TEMP% 下的目录，不碰真实配置。
func NewStore(dir string) (Store, error) {
	if dir == "" {
		var err error
		dir, err = AppDataDir()
		if err != nil {
			return Store{}, err
		}
	}
	return Store{Dir: dir, path: filepath.Join(dir, "settings.json")}, nil
}

func AppDataDir() (string, error) {
	if a := os.Getenv("APPDATA"); a != "" {
		return filepath.Join(a, "download2"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", apperr.Wrap(apperr.EFileOpFailed, "找不到用户目录", err)
	}
	return filepath.Join(h, ".config", "download2"), nil
}

func (s Store) Path() string { return s.path }

// Loaded 把「文件在不在、坏没坏」这两件事交给调用方，而不是在 Load 里替用户做决定。
type Loaded struct {
	Settings Settings
	FromDisk bool
	// CorruptPath 非空表示原文件解析失败，已被另存（路径在那儿），内存里用的是默认值。
	CorruptPath string
}

// Load 读设置。文件不存在不是错误——那是首次运行的正常态。
//
// 解析失败时**绝不静默回到默认值**：那等于把用户配好的转移目录悄悄清掉，
// 下一次下载就会搬到别处去，而他以为还在原处。做法是另存坏档 + 把这件事报给界面。
func (s Store) Load() (Loaded, error) {
	out := Loaded{Settings: Default()}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, apperr.Wrap(apperr.EFileOpFailed, "读不了设置文件", err)
	}

	var loaded Settings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		out.CorruptPath = s.quarantine(raw)
		return out, apperr.Wrap(apperr.EConfigCorrupt, "设置文件读坏了，已另存并暂用默认值", err)
	}
	out.FromDisk = true
	out.Settings = normalize(loaded)
	return out, nil
}

func (s Store) quarantine(raw []byte) string {
	dst := filepath.Join(s.Dir, fmt.Sprintf("settings.corrupt-%d.json", time.Now().Unix()))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return ""
	}
	return dst
}

// normalize 补齐旧版本文件里缺的字段：0 值不等于「用户就要 0」，
// 静置阈值给 0 会让判定立刻通过、把半成品搬走，所以 0 一律回落到默认。
func normalize(got Settings) Settings {
	def := Default()
	if got.QuietMs <= 0 {
		got.QuietMs = def.QuietMs
	}
	if got.MaxSettleMs <= 0 {
		got.MaxSettleMs = def.MaxSettleMs
	}
	if got.ToastSeconds <= 0 {
		got.ToastSeconds = def.ToastSeconds
	}
	if got.Version == 0 {
		got.Version = def.Version
	}
	// 主题走这里而不是 Validate：磁盘上写着别的词（手改过、旧版本、别的工具写过）
	// 不该让保存按钮点不动 —— 用户没有义务知道那个词是什么，回落默认就行。
	if got.Theme != ThemeDark && got.Theme != ThemeLight {
		got.Theme = def.Theme
	}
	// 语言同一条口径：它是一个偏好，不是一次判决。
	switch got.Lang {
	case LangAuto, LangZh, LangEn:
	default:
		got.Lang = def.Lang
	}
	got.SourceDir = cleanAbs(got.SourceDir)
	got.TargetDir = cleanAbs(got.TargetDir)
	got.RememberTarget()
	return got
}

// cleanPresets 把一串常用目录收成形：逐条收成绝对路径、丢掉空的和天生不能当落点的
// （整盘根、系统目录 —— 手改过设置文件或旧版本留下的那些），按 Windows 的路径口径去重
// （分隔符方向与大小写不算两个目录），保序截到 MaxTargetDirPresets。
//
// 空列表返回 nil 而不是 []string{}：前者在设置文件里根本不出现（omitempty），
// 后者会留下一行没有任何内容的 "targetDirPresets": []，而那是人要打开来看的文件。
func cleanPresets(list []string) []string {
	var out []string
	for _, p := range list {
		p = cleanAbs(p)
		if p == "" || isDriveRoot(p) || inSystemDir(p) {
			continue
		}
		var dup bool
		for _, q := range out {
			if samePath(p, q) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, p)
		if len(out) == MaxTargetDirPresets {
			break
		}
	}
	return out
}

// RememberTarget 把当前的转移目录挪到常用列表最前（顺带整串清洗、去重、限长）。
// 读盘和写盘两侧都走它，所以「正在用的那一个一定在列表里」不取决于界面那一刻送回了什么。
//
// 留给包外的那一个入口是 Hub.Save：那里才知道「落盘的那一份目录」是哪一个
// （本轮的 --dest-folder 不该被记成常用，见 diskTargetFor），所以由它先定目录再调这一句。
func (s *Settings) RememberTarget() {
	s.TargetDirPresets = cleanPresets(append([]string{s.TargetDir}, s.TargetDirPresets...))
}

// Save 原子写：先写 .new 并 Sync，再把旧的挪成 .bak，最后 .new 改名上岗。
// 中途断电最坏只丢 .bak，绝不会留下半个 settings.json 让用户下次开屏就撞上解析失败。
func (s Store) Save(got Settings) error {
	got.Version = Version
	got.SourceDir = cleanAbs(got.SourceDir)
	got.TargetDir = cleanAbs(got.TargetDir)
	got.RememberTarget()

	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "建不出设置目录", err)
	}

	buf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.EInternal, "设置序列化失败", err)
	}
	buf = append(buf, '\n')

	tmp := s.path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "写不了临时设置文件", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		os.Remove(tmp)
		return apperr.Wrap(apperr.EFileOpFailed, "写临时设置文件失败", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return apperr.Wrap(apperr.EFileOpFailed, "设置没落盘", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return apperr.Wrap(apperr.EFileOpFailed, "关闭临时设置文件失败", err)
	}

	if _, err := os.Stat(s.path); err == nil {
		if err := os.Rename(s.path, s.path+".bak"); err != nil {
			os.Remove(tmp)
			return apperr.Wrap(apperr.EFileOpFailed, "备份旧设置失败", err)
		}
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "换上新设置失败", err)
	}
	return nil
}
