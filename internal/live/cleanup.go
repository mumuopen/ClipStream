package live

// 磁盘清理器（M5 挂机刚需）：daemon 内定时运行。
// 策略（config.retention），五道防线由宽到严：
//   1. 超期清理：data/live/<room>_<ts>/ 会话目录超过 segments_days 整目录删除
//   2. 成品退役：data/jobs/<id>/ 已发布切片成品超 published_keep_days 删目录
//      （DB 行保留可查；源任务池条目全终态才删——发布素材直接引用源目录）
//   3. 磁盘水位：剩余 < min_free_percent 时预警 + 删最旧会话（直至高于水位或删光）
//   4. 占用上限：ClipStream 数据目录占用 > 整盘 max_usage_percent（默认一半）
//      时删最旧会话——水位守"盘快满"，占用上限守"这块盘我只肯给你一半"
//   5. 切片队列残留：data/live/queue/ 超 1 天的 .seg 视为脏数据清掉
// 注意：只动 data/live/ 与 data/jobs/ 产物，正在录制的会话目录绝不动
// （watches.status=recording 的 seg_dir 白名单）

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"chopperbot/internal/config"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

var liveSessionRe = regexp.MustCompile(`^\d+_(\d{8}_\d{6})$`)

// CleanupStats 一次清理的结果
type CleanupStats struct {
	DeletedSessions int
	FreedBytes      int64
	LowDisk         bool
	FreePercent     float64
	DeletedJobs     int // 成品目录退役数（data/jobs/<id>）
}

// RunCleanup 执行一次清理（daemon 定时调用；notify 可为 nil；st 可为 nil——
// 无 DB 时跳过依赖任务表的防线（②成品退役、录制中排除），不影响其余）
func RunCleanup(cfg *config.Config, st *store.Store, notifier *notify.Pushme) (CleanupStats, error) {
	cst := CleanupStats{}
	if cfg == nil {
		return cst, fmt.Errorf("config 为空")
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

	// 录制中目录白名单：水位/占用清理绝不动正在写入的会话（Windows 上目录被
	// 拉流写入中强删会半途而废，录制直接报废）
	recordingDirs := map[string]bool{}
	if st != nil {
		if ws, err := st.ListWatches(); err == nil {
			for _, w := range ws {
				if w.Status == "recording" && w.SegDir != "" {
					recordingDirs[w.SegDir] = true
				}
			}
		}
	}

	liveDir := filepath.Join(cfg.DataDir, "live")
	if _, err := os.Stat(liveDir); err == nil {
		// ① 超期会话目录清理
		entries, err := os.ReadDir(liveDir)
		if err != nil {
			return cst, err
		}
		var sessions []cleanupSession
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
			sessions = append(sessions, cleanupSession{path: p, ts: ts, size: sz})
			if time.Since(ts) > time.Duration(segDays)*24*time.Hour && !recordingDirs[p] {
				if err := os.RemoveAll(p); err == nil {
					cst.DeletedSessions++
					cst.FreedBytes += sz
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
					cst.FreedBytes += fi.Size()
				}
			}
		}

		// 可回收会话（未超期、非录制中），最旧在前：水位与占用上限共用
		var reclaimable []cleanupSession
		for _, s := range sessions {
			if time.Since(s.ts) <= time.Duration(segDays)*24*time.Hour && !recordingDirs[s.path] {
				reclaimable = append(reclaimable, s)
			}
		}
		sort.Slice(reclaimable, func(i, j int) bool { return reclaimable[i].ts.Before(reclaimable[j].ts) })

		// ③ 磁盘水位：低水位预警 + 删最旧
		freePercent, err := diskFreePercent(cfg.DataDir)
		cst.FreePercent = freePercent
		if err == nil && freePercent < float64(freePct) {
			cst.LowDisk = true
			deleted, freed := dropOldest(reclaimable, func() bool {
				fp, err := diskFreePercent(cfg.DataDir)
				return err == nil && fp >= float64(freePct)
			})
			cst.DeletedSessions += deleted
			cst.FreedBytes += freed
		}

		// ④ 占用上限：ClipStream 数据目录占用 > 整盘 max_usage_percent 时
		// 删最旧会话（默认 50% = "这块盘最多给 ClipStream 一半"）
		if rt.MaxUsagePercent > 0 {
			if usageOK(cfg, rt.MaxUsagePercent) == false {
				deleted, freed := dropOldest(reclaimable, func() bool {
					return usageOK(cfg, rt.MaxUsagePercent)
				})
				cst.DeletedSessions += deleted
				cst.FreedBytes += freed
				if !usageOK(cfg, rt.MaxUsagePercent) {
					// 会话删光仍超：jobs 成品与录制中目录不是本防线能动的，人工介入
					if notifier != nil {
						notifier.Notify("ClipStream 磁盘占用超限",
							fmt.Sprintf("数据目录占用仍超 %d%%（录制会话已清光）——请检查 data/jobs 成品归档或扩容",
								rt.MaxUsagePercent))
					}
				}
			}
		}
	}

	// ② 成品退役：已发布切片成品目录（data/jobs/<id>）超期清理。
	// 发布任务直接引用源 job 目录的 mp4——池条目还有 pending/dispatched 时
	// 绝不删（删了派出去的任务 prepare 就地崩溃）；需要 DB，无 store 时跳过
	if st != nil && rt.PublishedKeepDays > 0 {
		before := time.Now().Add(-time.Duration(rt.PublishedKeepDays) * 24 * time.Hour).Unix()
		if jobs, err := st.ListArtifactJobsBefore(before); err == nil {
			jobsDir := filepath.Join(cfg.DataDir, "jobs")
			for _, j := range jobs {
				dir := filepath.Join(jobsDir, j.ID)
				if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
					continue
				}
				if j.Kind == "clips" && st.JobArtifactInUse(j.ID) {
					continue // 素材仍在役（待发引用 / 审核台字幕配音 / 在途发布），不删
				}
				freed := dirSize(dir)
				if err := os.RemoveAll(dir); err == nil {
					cst.DeletedJobs++
					cst.FreedBytes += freed
				}
			}
		}
	}

	// 通知（有动作或低水位才打扰）
	if notifier != nil {
		if cst.LowDisk {
			notifier.Notify("ClipStream 磁盘水位预警",
				fmt.Sprintf("磁盘剩余仅 %.0f%%（阈值 %d%%），已自动清理 %d 个录制会话，释放 %.2f GB",
					cst.FreePercent, freePct, cst.DeletedSessions, float64(cst.FreedBytes)/1e9))
		} else if cst.DeletedSessions > 0 || cst.DeletedJobs > 0 {
			notifier.Notify("ClipStream 磁盘清理",
				fmt.Sprintf("超期清理 %d 个录制会话、%d 个任务成品目录，释放 %.2f GB（当前剩余 %.0f%%）",
					cst.DeletedSessions, cst.DeletedJobs, float64(cst.FreedBytes)/1e9, cst.FreePercent))
		}
	}
	return cst, nil
}

// cleanupSession 一个录制会话目录的清理画像
// dropOldest 从最旧开始删会话，每删一个检查一次 stop 条件（达条件即停）。
// 返回（删除数, 释放字节）。传入的 sessions 已按最旧在前排序且不含录制中
type cleanupSession struct {
	path string
	ts   time.Time
	size int64
}

func dropOldest(sessions []cleanupSession, stop func() bool) (deleted int, freed int64) {
	for _, s := range sessions {
		if stop() {
			return deleted, freed
		}
		if err := os.RemoveAll(s.path); err == nil {
			deleted++
			freed += s.size
		}
	}
	return deleted, freed
}

// usageOK 判断数据目录占用是否在上限内（≤ 总容量 pct%）。目录统计失败时
// 返回 true（宁可不删，不误判）
func usageOK(cfg *config.Config, pct int) bool {
	_, total, err := diskBytes(cfg.DataDir)
	if err != nil || total <= 0 {
		return true
	}
	used := dirSize(cfg.DataDir)
	return float64(used) <= float64(total)*float64(pct)/100
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

// diskBytes 目录所在磁盘的（剩余, 总容量）字节（Windows GetDiskFreeSpaceExW）
func diskBytes(path string) (free, total int64, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, err
	}
	root := filepath.VolumeName(abs)
	if root == "" {
		root = `\`
	}
	if !strings.HasSuffix(root, `\`) && !strings.HasSuffix(root, `/`) {
		root += string(filepath.Separator)
	}
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, err
	}
	var f, t, _free uint64
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetDiskFreeSpaceExW")
	r1, _, e1 := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&f)),
		uintptr(unsafe.Pointer(&t)),
		uintptr(unsafe.Pointer(&_free)),
	)
	if r1 == 0 {
		return 0, 0, e1
	}
	return int64(f), int64(t), nil
}

// diskFreePercent 目录所在磁盘剩余百分比
func diskFreePercent(path string) (float64, error) {
	free, total, err := diskBytes(path)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, fmt.Errorf("磁盘总容量为 0")
	}
	return float64(free) / float64(total) * 100, nil
}