package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

// Guard 直播守护器（对齐原版 ChopperBot 的 HotGuard/LiverFollower）：
// 轮询关注列表 → 开播自动录制（FLV 分段）→ 下播自动将分段送入切片管线
type Guard struct {
	cfg    *config.Config
	store  *store.Store
	notify *notify.Pushme

	// OnSegQueued 分段送入切片管线回调（由 main 装配，避免反向依赖 pipeline）
	OnSegQueued func(segPath, roomTitle string)

	mu        sync.Mutex
	recorders map[string]*Recorder // roomID → active
	ctx       context.Context
	stopFn    context.CancelFunc
	wg        sync.WaitGroup
}

func NewGuard(cfg *config.Config, st *store.Store, n *notify.Pushme) *Guard {
	return &Guard{
		cfg:       cfg,
		store:     st,
		notify:    n,
		recorders: map[string]*Recorder{},
	}
}

// Start 启动守护循环（serve 时调用）
func (g *Guard) Start() {
	g.ctx, g.stopFn = context.WithCancel(context.Background())
	interval := time.Duration(g.cfg.Live.CheckIntervalSec) * time.Second
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.tick() // 启动立刻查一轮（添加关注后无需等 interval）
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-t.C:
				g.tick()
			}
		}
	}()
}

// Stop 停止全部录制与轮询（serve 退出时调用）
func (g *Guard) Stop() {
	if g.stopFn != nil {
		g.stopFn()
	}
	g.wg.Wait()
	g.mu.Lock()
	for _, rec := range g.recorders {
		go rec.Stop() // 并行收尾，不阻塞退出
	}
	g.mu.Unlock()
}

// Kick 触发立即检测一轮（添加关注后调用，不用等下个周期）
func (g *Guard) Kick() {
	if g.ctx == nil {
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if g.ctx.Err() != nil {
			return
		}
		g.tick()
	}()
}

// StopRoomByID 删除关注时停止该房间录制
func (g *Guard) StopRoomByID(id int64) {
	watches, err := g.store.ListWatches()
	if err != nil {
		return
	}
	for _, w := range watches {
		if w.ID != id {
			continue
		}
		g.mu.Lock()
		rec := g.recorders[w.RoomID]
		delete(g.recorders, w.RoomID)
		g.mu.Unlock()
		if rec != nil {
			go rec.Stop()
		}
		return
	}
}

func (g *Guard) tick() {
	watches, err := g.store.ListWatches()
	if err != nil {
		return
	}
	for _, w := range watches {
		g.check(w)
	}
}

func (g *Guard) check(w *store.Watch) {
	// 录制中：确认 recorder 存活（recorder 自带断流重连，异常结束时清理）
	if w.Status == "recording" {
		g.mu.Lock()
		rec := g.recorders[w.RoomID]
		g.mu.Unlock()
		if rec == nil || rec.Done() {
			// recorder 已结束（onEnd 回调应已把状态置回 idle；此处兜底）
			g.mu.Lock()
			delete(g.recorders, w.RoomID)
			g.mu.Unlock()
			if w.Status == "recording" {
				w.Status = "idle"
				_ = g.store.UpdateWatch(w)
			}
		} else {
			w.SegCount = int(rec.SegCount())
			_ = g.store.UpdateWatch(w)
			return
		}
		// 残留状态已清理：落入下方的开播检测（不等下轮，同轮即测）
	}

	// 空闲/异常：开播检测（API 失败时标 error 留状态给下轮重试，不判死）
	ctx, cancel := context.WithTimeout(g.ctx, 20*time.Second)
	defer cancel()
	info, err := GetRoomInfo(ctx, w.RoomID)
	if err != nil {
		if w.Status != "error" {
			w.Status = "error"
			w.LastError = err.Error()
			_ = g.store.UpdateWatch(w)
		}
		return
	}
	if w.Title != info.Title { // 标题变化也顺便更新
		w.Title = info.Title
	}
	if info.LiveStatus != 1 {
		if w.Status != "idle" {
			w.Status = "idle"
			w.LastError = ""
			_ = g.store.UpdateWatch(w)
		} else if w.Title != "" {
			_ = g.store.UpdateWatch(w)
		}
		return
	}

	// 开播！启动录制
	g.startRecording(w, info)
}

func (g *Guard) startRecording(w *store.Watch, info *RoomInfo) {
	segDir := filepath.Join(g.cfg.DataDir, "live", fmt.Sprintf("%s_%s", w.RoomID, time.Now().Format("20060102_150405")))
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return
	}

	w.Status = "recording"
	w.LiveStart = time.Now().Unix()
	w.SegDir = segDir
	w.SegCount = 0
	w.Title = info.Title
	w.LastError = ""
	_ = g.store.UpdateWatch(w)

	// 【实验验证的组合模式】Windows 网络栈需要"第一次失败的子进程连接"来 prime，
	// 之后的子进程才能做 FLV 流式 IO。保留 Recorder（作为 primer，会挂起在 CDN），
	// 5 秒后用直接 exec.Command 做实际录制（diag2 对照实验反复验证此组合有效）。
	rec := &Recorder{
		RoomID:     w.RoomID,
		SegDir:     segDir,
		FFmpegBin:  g.cfg.Engines.FFmpeg,
		SegmentSec: g.cfg.Live.SegmentSec,
		OnLog:      func(format string, args ...any) {},
		OnEnd:      func(rec *Recorder, segs []string, reason string) {},
	}
	rec.Start(g.ctx) // Primer goroutine（网络 prime 用，不产出有效段文件）

	// 实际录制 goroutine（在 primer 启动 5 秒后执行，此组合经 diag2 实验反复验证有效）
	go func() {
		time.Sleep(5 * time.Second)
		fmt.Printf("[live %s] 正式录制启动\n", w.RoomID)
		playURL, err := GetPlayURL(context.Background(), w.RoomID)
		if err != nil {
			fmt.Printf("[live %s] 获取地址失败: %v\n", w.RoomID, err)
			g.endRecording(w, segDir, "获取地址失败", info.Title)
			return
		}
		exe, _ := os.Executable()
		recordBin := filepath.Join(filepath.Dir(exe), "guanclip-record.exe")
		fmt.Printf("[live %s] 开始拉流: %.60s\n", w.RoomID, playURL)
		cmd := exec.Command(recordBin, playURL, segDir, fmt.Sprintf("%d", g.cfg.Live.SegmentSec))
		out, _ := cmd.CombinedOutput()
		if len(out) > 0 {
			fmt.Printf("[live %s] 录制器: %s\n", w.RoomID, string(out))
		}

		// 录制结束：检查下播或重连
		ri, e := GetRoomInfo(context.Background(), w.RoomID)
		if e != nil || ri.LiveStatus == 0 {
			g.endRecording(w, segDir, "下播", info.Title)
			return
		}
		fmt.Printf("[live %s] 仍在播，重新启动录制\n", w.RoomID)
		g.startRecording(w, info)
	}()

	// pushme 开播通知
	g.notify.Send("GuanClip 开播提醒",
		fmt.Sprintf("《%s》开播了，已自动开始录制（房间 %s）", info.Title, w.RoomID))
}

func (g *Guard) endRecording(w *store.Watch, segDir, reason, title string) {
	segs := listSegFiles(segDir)
	w.Status = "idle"
	w.SegCount = len(segs)
	_ = g.store.UpdateWatch(w)

	g.notify.Send("GuanClip 直播录制结束",
		fmt.Sprintf("《%s》%s；分段 %d 个", title, reason, len(segs)))

	if g.cfg.Live.AutoClip && g.OnSegQueued != nil {
		for _, seg := range segs {
			g.OnSegQueued(seg, title)
		}
	}
}

func listSegFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "seg_*.flv"))
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Size() > 1<<20 {
			out = append(out, m)
		}
	}
	return out
}

func countSegs(dir string) int {
	return len(listSegFiles(dir))
}
