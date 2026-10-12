package live

// 磁盘清理器（M5 挂机刚需）：daemon 内定时运行。
// 策略（config.retention）：
//   1. 超期清理：data/live/<room>_<ts>/ 会话目录超过 segments_days 整目录删除
//   2. 磁盘水位：剩余 < min_free_percent 时 pushme 预警 + 删最旧会话（直至高于水位或删光）
//   3. 切片队列残留：data/live/queue/ 超 1 天的 .seg 视为脏数据清掉
// 注意：只动 data/live/（录制产物），不碰 data/jobs/（切片成品/审核产物归 serve 管）

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
	"unsafe"

	"chopperbot/internal/config"
	"chopperbot/internal/notify"
)

var liveSessionRe = regexp.MustCompile(`^\d+_(\d{8}_\d{6})$`)

// CleanupStats 一次清理的结果
type CleanupStats struct {
	DeletedSessions int
	FreedBytes      int64
	LowDisk         bool
	FreePercent     float64
}

// RunCleanup 执行一次清理（daemon 定时调用；notify 可为 nil）
func RunCleanup(cfg *config.Config, notifier *notify.Pushme) (CleanupStats, error) {
	st := CleanupStats{}
	if cfg == nil {
		return st, fmt.Errorf("config 为空")
	}
	rt := cfg.Retention
	segDays := rt.SegmentsDays
	if segDays <= 0 {
		segDays = 7
	}
	freePct := rt.MinFreePercent
	if freePct <= 0 {
		freePct = 10
	}

	liveDir := filepath.Join(cfg.DataDir, "live")
	if _, err := os.Stat(liveDir); err != nil {
		return st, nil // 目录不存在（从未录制过），无需清理
	}

	// ① 超期会话目录清理
	entries, err := os.ReadDir(liveDir)
	if err != nil {
		return st, err
	}
	type session struct {
		path string
		ts   time.Time
		size int64
	}
	var sessions []session
	for _, e := range entries {
		if !e.IsDir() || !liveSessionRe.MatchString(e.Name()) {
			continue
		}
		m := liveSessionRe.FindStringSubmatch(e.Name())
		ts, err := time.ParseInLocation("20060102_150405", m[1], time.Local)
		if err != nil {
			continue
		}
		p := filepath.Join(liveDir, e.Name())
		sz := dirSize(p)
		sessions = append(sessions, session{path: p, ts: ts, size: sz})
		if time.Since(ts) > time.Duration(segDays)*24*time.Hour {
			if err := os.RemoveAll(p); err == nil {
				st.DeletedSessions++
				st.FreedBytes += sz
			}
		}
	}

	// ② 队列残留清理（>1 天的 .seg）
	queueDir := filepath.Join(liveDir, "queue")
	if qes, err := os.ReadDir(queueDir); err == nil {
		for _, qe := range qes {
			if qe.IsDir() {
				continue
			}
			fi, err := qe.Info()
			if err != nil || time.Since(fi.ModTime()) < 24*time.Hour {
				continue
			}
			p := filepath.Join(queueDir, qe.Name())
			if err := os.Remove(p); err == nil {
				st.FreedBytes += fi.Size()
			}
		}
	}

	// ③ 磁盘水位：低水位预警 + 删最旧
	freePercent, err := diskFreePercent(cfg.DataDir)
	st.FreePercent = freePercent
	if err == nil && freePercent < float64(freePct) {
		st.LowDisk = true
		var alive []session
		for _, s := range sessions {
			if time.Since(s.ts) <= time.Duration(segDays)*24*time.Hour {
				alive = append(alive, s)
			}
		}
		sort.Slice(alive, func(i, j int) bool { return alive[i].ts.Before(alive[j].ts) })
		for _, s := range alive {
			if err := os.RemoveAll(s.path); err == nil {
				st.DeletedSessions++
				st.FreedBytes += s.size
			}
			if fp, err := diskFreePercent(cfg.DataDir); err == nil && fp >= float64(freePct) {
				break
			}
		}
	}

	// 通知（有动作或低水位才打扰）
	if notifier != nil {
		if st.LowDisk {
			notifier.Notify("ClipStream 磁盘水位预警",
				fmt.Sprintf("磁盘剩余仅 %.0f%%（阈值 %d%%），已自动清理 %d 个录制会话，释放 %.2f GB",
					freePercent, freePct, st.DeletedSessions, float64(st.FreedBytes)/1e9))
		} else if st.DeletedSessions > 0 {
			notifier.Notify("ClipStream 磁盘清理",
				fmt.Sprintf("超期清理 %d 个录制会话，释放 %.2f GB（当前剩余 %.0f%%）",
					st.DeletedSessions, float64(st.FreedBytes)/1e9, freePercent))
		}
	}
	return st, nil
}

func dirSize(p string) int64 {
	var total int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// diskFreePercent 目录所在磁盘剩余百分比（Windows GetDiskFreeSpaceExW）
func diskFreePercent(path string) (float64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	root := filepath.VolumeName(abs)
	if root == "" {
		root = `\`
	}
	if root[len(root)-1] != '\\' && root[len(root)-1] != '/' {
		root += string(filepath.Separator)
	}
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var free, total, _free uint64
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetDiskFreeSpaceExW")
	r1, _, e1 := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&_free)),
	)
	if r1 == 0 {
		return 0, e1
	}
	if total == 0 {
		return 0, fmt.Errorf("磁盘总容量为 0")
	}
	return float64(free) / float64(total) * 100, nil
}