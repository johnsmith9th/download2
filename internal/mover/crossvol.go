package mover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"download2/internal/apperr"
	"download2/internal/detect"
	"download2/internal/journal"
)

// byCopy 是跨卷 / 网络路径那条路：
// 复制到中间名 → 落盘 → 核对字节 → 补时间 → 改名上岗 → 才删源。
//
// 顺序里每一环都是同一条规矩的化身：**源文件不得早于「目标已经是完整一份」而消失**。
// 所以核对放在改名上岗之前 —— 中间名上的坏副本是我们自己的东西，删它没有任何风险；
// 而一旦它换上最终名，它就是用户的文件了，我们再没有资格动它。
//
// 返回 true 表示文件已经安全落在目标，只是源还被占用、欠一次补删 —— 那不是失败。
func (m *Mover) byCopy(j *job) (bool, error) {
	src, dst, tmp := j.rec.Src, j.rec.Dst, j.rec.Tmp

	in, err := os.Open(src)
	if err != nil {
		return false, fileOp("读不了源文件。", err)
	}

	// 中间名先记在账上再创建：这样即使建到一半被杀，重放也知道那个名字是我们自己的东西。
	out, actual, err := m.openTmp(dst, tmp, j.rec.ID)
	if err != nil {
		in.Close()
		return false, err
	}
	if actual != tmp {
		tmp = actual
		j.rec.Tmp = tmp
		if err := m.book.Append(m.stamped(j, journal.StateClaimed, "默认中间名被占了，换一个")); err != nil {
			out.Close()
			in.Close()
			m.dropTmp(tmp)
			return false, apperr.Wrap(apperr.EFileOpFailed, "换中间名前记不上账。", err)
		}
	}
	if err := m.book.Append(m.stamped(j, journal.StateMoving, "")); err != nil {
		out.Close()
		in.Close()
		m.dropTmp(tmp)
		return false, apperr.Wrap(apperr.EFileOpFailed, "开始复制前记不上账。", err)
	}

	// 从这一行起 tmp 存在。下面每一条失败路径都只清它，绝不碰源。
	_, copyErr := m.copy(out, in)
	// 源句柄必须在删源之前放下：自己还开着一个句柄去 DeleteFile，
	// Windows 只会把那个名字留在「删除待定」里 —— 看着像没删成，其实是没人能删。
	closeErr := in.Close()
	if copyErr != nil {
		out.Close()
		m.dropTmp(tmp)
		return false, apperr.Wrap(apperr.EFileOpFailed, "复制没完成，源文件一个字节都没动。", copyErr)
	}
	if closeErr != nil {
		out.Close()
		m.dropTmp(tmp)
		return false, apperr.Wrap(apperr.EFileOpFailed, "读完源文件却关不掉它，源一个字节都没动。", closeErr)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		m.dropTmp(tmp)
		return false, apperr.Wrap(apperr.EFileOpFailed, "副本没落盘，源文件一个字节都没动。", err)
	}
	if err := out.Close(); err != nil {
		m.dropTmp(tmp)
		return false, apperr.Wrap(apperr.EFileOpFailed, "副本收尾没做完，源文件一个字节都没动。", err)
	}

	// 两道核对：副本对不对得上源，源还是不是判定时的样子。
	// 后一道是防「下载器又回来续传了」——那种文件本来就不该被搬，更不该被删。
	if err := m.verify(j, src, tmp); err != nil {
		m.dropTmp(tmp)
		return false, err
	}
	m.keepTimes(src, tmp, j.sample)

	if err := m.publish(j, tmp); err != nil {
		m.dropTmp(tmp)
		return false, err
	}

	// 删源。目标已经带着核对过的字节上岗了，这一步才允许碰源。
	if err := m.remove(src); err != nil {
		if isBusy(err) {
			if aerr := m.book.Append(m.stamped(j, journal.StatePendingDelete, "目标已就位，源还被占用")); aerr != nil {
				// 欠条没记上，重放就不知道要补删。结果只是同一份文件在两处都留着，
				// 不是数据丢失，所以照实记一笔日志，仍然把成功返回给调用方。
				m.log.Error("欠补删但记不上账", "src", src, "err", aerr)
			}
			return true, nil
		}
		return false, fileOp("已经复制到目标了，但删不掉源文件。", err)
	}
	return false, nil
}

// verify 核对副本字节数。
//
// 拿三个数比：账本里的 sample.Size（判定时看到的）、源的当前大小、副本的当前大小。
// 三者不齐就不许继续 —— 少了这一个闸，跨卷复制被打断时我们就可能删掉用户的半个文件。
func (m *Mover) verify(j *job, src, tmp string) error {
	srcSt, err := os.Stat(src)
	if err != nil {
		return fileOp("复制完了却看不了源文件，源留在原地。", err)
	}
	tmpSt, err := os.Stat(tmp)
	if err != nil {
		return apperr.Wrap(apperr.EFileOpFailed, "副本建出来了却看不了，源文件没动。", err)
	}
	switch {
	case tmpSt.Size() != srcSt.Size():
		return apperr.New(apperr.EFileOpFailed, fmt.Sprintf(
			"副本 %d 字节、源 %d 字节，对不上就不删源。", tmpSt.Size(), srcSt.Size()))
	case srcSt.Size() != j.sample.Size:
		// 判定之后源又变了：它当时就没下完，或者被人续传了。这一轮整个作废，回到观察。
		return apperr.New(apperr.EInvalidArg, fmt.Sprintf(
			"文件在判定之后又变了（%d → %d 字节），这次不搬。", j.sample.Size, srcSt.Size()))
	}
	j.dstSize = tmpSt.Size()
	return nil
}

// publish 把副本从中间名换上最终名。占用照旧重试，撞名时重挑号，绝不覆盖。
func (m *Mover) publish(j *job, tmp string) error {
	for c := 0; c < conflictTries; c++ {
		err := m.renameBusy(j, tmp, j.rec.Dst)
		if err == nil {
			// 名字上岗了：从这里起它已经是用户的文件，任何失败都不许再删它。
			return m.book.Append(m.stamped(j, journal.StateCopied, ""))
		}
		if isAlreadyThere(err) {
			j.conflicts++
			alt, perr := pickName(filepath.Dir(j.rec.Dst), filepath.Base(j.rec.Src))
			if perr != nil {
				return apperr.Wrap(apperr.EFileOpFailed, "在转移目录里挪不出一个空着的文件名。", perr)
			}
			j.rec.Dst = alt
			if aerr := m.book.Append(m.stamped(j, journal.StateMoving, "目标名被抢，换个名字重试")); aerr != nil {
				return apperr.Wrap(apperr.EFileOpFailed, "换名字前记不上账。", aerr)
			}
			continue
		}
		return fileOp("副本没能换上最终名。", err)
	}
	return apperr.New(apperr.EFileOpFailed, "转移目录里撞名太多次，先停下。")
}

// openTmp 用 O_EXCL 建中间名：那个名字已经有东西就不再试第二次的可能性。
//
// 撞名时不删 existing 的文件、只换一个带记录 ID 的名字 —— 它虽然以 .download2.tmp 结尾，
// 也不能证明是我们的东西：用户完全可能有这么一个文件，而删掉别人的文件没有可恢复性可言。
func (m *Mover) openTmp(dst, tmp, id string) (*os.File, string, error) {
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return out, tmp, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, "", apperr.Wrap(apperr.EFileOpFailed, "建不出副本的中间文件。", err)
	}
	alt := tmpName(dst, id)
	if alt == tmp {
		return nil, "", apperr.Wrap(apperr.EFileOpFailed, "副本要用的中间名已经被别的文件占了。", err)
	}
	out, err = os.OpenFile(alt, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.EFileOpFailed, "建不出副本的中间文件。", err)
	}
	return out, alt, nil
}

// keepTimes 把源的时间戳搬到副本上。
//
// 失败只记日志：字节已经核对过了，为了一个访问时间把一个完整副本扔掉、
// 让用户的文件多留在 Downloads 一天，是判错了轻重。
func (m *Mover) keepTimes(src, tmp string, s detect.Sample) {
	fi, err := os.Stat(src)
	if err != nil || s.Mtime.IsZero() {
		return
	}
	if err := os.Chtimes(tmp, accessTime(fi), s.Mtime); err != nil {
		m.log.Debug("副本的时间戳没跟上", "tmp", tmp, "err", err)
	}
}

// dropTmp 只清我们自己造的中间文件。
func (m *Mover) dropTmp(tmp string) {
	if tmp == "" {
		return
	}
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		// 删不掉就不再纠缠：这个路径以 .download2.tmp 结尾，判定侧永远不当它是候选，
		// 留着只是碍眼，而误删才是不可恢复的。
		m.log.Warn("中间文件删不掉", "tmp", tmp, "err", err)
	}
}
