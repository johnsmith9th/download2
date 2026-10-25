package watch

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fsnotify/fsnotify"

	"download2/internal/detect"
)

// 这一组用例里的路径用正斜杠写：Classify 只做字符串归一，不碰 OS，
// 所以它得在任何平台上都跑得起来，而不是靠运行它的那台机器恰好是 Windows。
// 唯一的例外是分隔符 —— Windows 上 filepath.Clean 会把 / 换成 \，
// 所以期望值一律过一遍 toOS()，让它跟着平台走。
func toOS(p string) string { return filepath.FromSlash(p) }

func TestClassifyMapsOpsToMachineInput(t *testing.T) {
	cases := []struct {
		name string
		ev   fsnotify.Event
		want []detect.Event
	}{
		{"新名字", fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Create},
			[]detect.Event{{Kind: detect.EvCreate, Path: toOS("/D/Downloads/a.zip")}}},
		{"内容变了", fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Write},
			[]detect.Event{{Kind: detect.EvWrite, Path: toOS("/D/Downloads/a.zip")}}},
		{"名字没了", fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Remove},
			[]detect.Event{{Kind: detect.EvRemove, Path: toOS("/D/Downloads/a.zip")}}},
		{"改名的左半边", fsnotify.Event{Name: "/D/Downloads/a.zip.crdownload", Op: fsnotify.Rename},
			[]detect.Event{{Kind: detect.EvRenameFrom, Path: toOS("/D/Downloads/a.zip.crdownload")}}},
		{"属性变化不参与判据", fsnotify.Event{Name: "/D/Downloads/a.zip", Op: fsnotify.Chmod}, nil},
		{"空操作", fsnotify.Event{Name: "/D/Downloads/a.zip", Op: 0}, nil},
		{"没有名字的事件", fsnotify.Event{Op: fsnotify.Create}, nil},
		// 路径键进状态机之前必须归一：同一个文件的两次通知落成两个条目，
		// 就等于同一次下载可能被搬两次。
		{"路径先归一", fsnotify.Event{Name: "/D/Downloads/./x/../a.zip", Op: fsnotify.Create},
			[]detect.Event{{Kind: detect.EvCreate, Path: toOS("/D/Downloads/a.zip")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.ev)
			if len(got) != len(c.want) {
				t.Fatalf("得到 %+v, 想要 %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("第 %d 条 = %+v, 想要 %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// 改名对只能靠到达顺序对上：fsnotify v1.10.1 把改名来源字段设成了包私有的
// renamedFrom，而且没有留任何读取入口（backend_windows.go 的 sendEvent 收了
// renamedFrom 参数，但事件到调用方手上时那个信息已经被丢掉了）。
//
// 这条不是随口一提 —— 能拿到「这个 Create 来自哪个老名字」的话，判定就不必依赖
// 两条事件的先后，Machine.aborts 那一轮延后也许可省。哪天升级依赖把它又暴露出来了，
// 这个测试会先红，好让人回来读 classify 的注释。
func TestRenameSourceIsNotExposedByFsnotify(t *testing.T) {
	typ := reflect.TypeOf(fsnotify.Event{})
	for _, name := range []string{"RenamedFrom", "OldName", "From"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Fatalf("fsnotify.Event 现在导出了字段 %s，classify 的改名对可以改成直接读来源", name)
		}
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.IsExported() && f.Name != "Name" && f.Name != "Op" {
			t.Errorf("多出一个导出的字段 %s，需要确认它是不是改名来源", f.Name)
		}
	}
}

// Windows 后端一个 action 只给一个 Op 位；万一哪天合并成 Create|Rename，
// 按 Create 解释是安全的：改名左半边缺席时，detect 那边最多晚一轮结算，不会搬错。
func TestClassifyPrefersCreateWhenOpsCombine(t *testing.T) {
	got := Classify(fsnotify.Event{
		Name: "/D/Downloads/a.zip",
		Op:   fsnotify.Create | fsnotify.Rename | fsnotify.Chmod,
	})
	if len(got) != 1 || got[0].Kind != detect.EvCreate {
		t.Fatalf("得到 %+v", got)
	}
}

// Clean 只会往「更干净」的方向变，这里钉一下归一化确实发生了。
func TestClassifyCleansTheSameWayAsFilepath(t *testing.T) {
	for _, p := range []string{"/D/Downloads/a.zip", "/D/Downloads//a.zip", "/D/Downloads/./a.zip"} {
		got := Classify(fsnotify.Event{Name: p, Op: fsnotify.Create})
		if len(got) != 1 || got[0].Path != filepath.Clean(p) {
			t.Errorf("%q 归一成 %v, 想要 %q", p, got, filepath.Clean(p))
		}
	}
}
