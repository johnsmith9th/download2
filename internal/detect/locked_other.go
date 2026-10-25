//go:build !windows

package detect

// Locked 在非 Windows 上恒为 false。
//
// POSIX 没有 share mode 这回事：别人开着写并不妨碍我们也打开，而协作式记录锁下载器
// 根本不加，所以这里探不出「还在被占用」，只剩「静置 + 双采样」两条判据兜着。
// 本产品只发布 Windows 版，这个文件的存在意义是让测试跨平台跑得起来。
func Locked(string) bool { return false }
