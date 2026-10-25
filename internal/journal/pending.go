package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pendingFile 是 pending.jsonl 的读写。它是 append-only 的：一条搬运的每一步都追加一行，
// 从不改旧行。这样崩溃时最多丢最后一行，而不是让整个文件不可读。
type pendingFile struct{ path string }

func (f pendingFile) append(r Record) error {
	// 三个必填项：没 ID 无法归并，没时间戳无法判过期，没源路径则重放时无从查起。
	if r.ID == "" {
		return errors.New("journal: 记录没有 ID")
	}
	if r.TS.IsZero() {
		return errors.New("journal: 记录没有时间戳")
	}
	if r.Src == "" {
		return errors.New("journal: 记录没有源路径")
	}

	buf, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("journal: 记录编码失败: %w", err)
	}
	buf = append(buf, '\n')

	file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("journal: 打不开账本: %w", err)
	}
	if _, err := file.Write(buf); err != nil {
		file.Close()
		return fmt.Errorf("journal: 写账本失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("journal: 账本没落盘: %w", err)
	}
	return file.Close()
}

// load 读出每个 ID 的最新状态，按第一次出现的顺序返回。
//
// 读之前先修尾部：撕裂的最后一行必须截掉，否则下一次 append 会把新行接在半截 JSON 后面，
// 一条能读的账就变成两条读不了的账。截掉的只能是没写完的那条——它既然不完整，
// 当初的动作就没被记账，重放时最多是「同一个文件被重新观察一遍」，不会误删东西。
//
// 完整行里解析不了的（老版本字段、手工编辑）跳过并计数，绝不整体报错：
// 账本读不动就意味着重启后对半完成状态失忆，那比跳过一行严重得多。
func (f pendingFile) load() (recs []Record, skipped int, err error) {
	// 账本最多几百行，整个读进内存比维护一个游标好读，也不会因为一次 SEEK 出错而失忆。
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("journal: 读不了账本: %w", err)
	}

	// 最后一个换行之后就是撕裂的尾巴，截掉它。
	if cut := bytes.LastIndexByte(raw, '\n'); cut >= 0 && cut+1 < len(raw) {
		if err := os.Truncate(f.path, int64(cut+1)); err != nil {
			return nil, 0, fmt.Errorf("journal: 截掉撕裂尾巴失败: %w", err)
		}
		raw = raw[:cut+1]
	} else if cut < 0 && len(raw) > 0 {
		// 整段都是半行（第一次写就被打断），同样只能丢掉。
		if err := os.Truncate(f.path, 0); err != nil {
			return nil, 0, fmt.Errorf("journal: 清空撕裂的账本失败: %w", err)
		}
		return nil, 0, nil
	}

	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			skipped++
			continue
		}
		if r.ID == "" {
			skipped++
			continue
		}
		if i, ok := seen[r.ID]; ok {
			recs[i] = r
			continue
		}
		seen[r.ID] = len(recs)
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return recs, skipped, fmt.Errorf("journal: 扫账本出错: %w", err)
	}
	return recs, skipped, nil
}

// rewrite 原子地用 keep 替换整个账本内容，只给压缩用。
//
// 顺序是「同目录临时文件写全 → Sync → 旧的挪去 .bak → 临时文件改名上岗」。
// 中途最坏停在 .bak，绝不会留下半个 JSONL——半本账本比没账本更坏，因为它读都读不出来。
func (f pendingFile) rewrite(keep []Record) error {
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, "pending-*.jsonl.tmp")
	if err != nil {
		return fmt.Errorf("journal: 建不出压缩用的临时文件: %w", err)
	}
	tmpName := tmp.Name()

	w := bufio.NewWriter(tmp)
	for _, r := range keep {
		buf, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return fmt.Errorf("journal: 压缩时编码失败: %w", err)
		}
		w.Write(buf)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("journal: 压缩时写失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("journal: 压缩时没落盘: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("journal: 关闭压缩临时文件失败: %w", err)
	}

	if _, err := os.Stat(f.path); err == nil {
		if err := os.Rename(f.path, f.path+".bak"); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("journal: 备份旧账本失败: %w", err)
		}
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("journal: 换上压缩后的账本失败: %w", err)
	}
	return nil
}

// keepFor 是「解决完的记录再留多久」。留一周是为了事后追问「这文件哪天搬走的、搬到哪儿」
// 时还查得到；再久就没意义了，而且账本越长启动重放越慢。
const keepFor = 7 * 24 * time.Hour
