package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Move 是 moves.jsonl 里的一行：给人看的搬运流水。
//
// 它和 pending.jsonl 的分工要明确：pending 是给崩溃后重放用的机器账本，字段齐全但难读；
// moves 是用户追问「我那个文件去哪了」时唯一会去翻的东西，所以每条都必须自带结果。
type Move struct {
	TS   time.Time `json:"ts"`
	Src  string    `json:"src"`
	Dst  string    `json:"dst,omitempty"`
	Size int64     `json:"size"`

	// Method 是搬运用的手段，用户看到 copy 就该预期慢。
	Method string `json:"method,omitempty"` // rename | copy
	Ms     int64  `json:"ms,omitempty"`

	// Outcome 用英文稳定值，方便 grep；界面要显示中文自己映射。
	Outcome string `json:"outcome"` // moved | abandoned | stuck | aborted | merged
	Reason  string `json:"reason,omitempty"`
}

// 结果的稳定词汇表。写在磁盘上，改了就和老日志对不上。
const (
	OutcomeMoved     = "moved"
	OutcomeAbandoned = "abandoned"
	OutcomeStuck     = "stuck"
	OutcomeAborted   = "aborted"
	// OutcomeMerged 是压缩时从 pending 搬进 moves 的老记录，不是新发生的一次搬运。
	OutcomeMerged = "merged"
)

type movesLog struct{ path string }

func (m movesLog) append(ev Move) error {
	if ev.Outcome == "" {
		return errors.New("journal: 流水记录没有 outcome")
	}
	if ev.TS.IsZero() {
		return errors.New("journal: 流水记录没有时间戳")
	}
	buf, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("journal: 流水编码失败: %w", err)
	}
	buf = append(buf, '\n')

	file, err := os.OpenFile(m.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("journal: 打不开流水: %w", err)
	}
	if _, err := file.Write(buf); err != nil {
		file.Close()
		return fmt.Errorf("journal: 写流水失败: %w", err)
	}
	// 流水也要逐行 Sync：它要是丢了，用户追问时我们连「搬过」这个事实都拿不出来。
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("journal: 流水没落盘: %w", err)
	}
	return file.Close()
}
