package live

import (
	"context"
	"fmt"
	"os"
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
	// 调试：确认 startRecording 被调用
	dbg := filepath.Join(segDir, "debug.txt")
	_ = os.WriteFile(dbg, []byte(fmt.Sprintf("startRecording called at %s, room=%s, url=%s\n", time.Now(), w.RoomID, w.URL)), 0o644)

	rec := &Recorder{
		RoomID:     w.RoomID,
		SegDir:     segDir,
		FFmpegBin:  g.cfg.Engines.FFmpeg,
		SegmentSec: g.cfg.Live.SegmentSec,
		OnLog: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			fmt.Printf("[live %s] %s\n", w.RoomID, msg)
			// 同时写调试文件（防 stdout 被吞）
			if f, e := os.OpenFile(filepath.Join(segDir, "debug.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); e == nil {
				_, _ = f.WriteString(fmt.Sprintf("%s [live %s] %s\n", time.Now().Format("15:04:05"), w.RoomID, msg))
				_ = f.Close()
			}
		},
		OnEnd: func(rec *Recorder, segs []string, reason string) {
			g.mu.Lock()
			delete(g.recorders, w.RoomID)
			g.mu.Unlock()
			w.Status = "idle"
			w.SegDir = segDir
			w.SegCount = len(segs)
			_ = g.store.UpdateWatch(w)

			title := w.Title
			if info != nil && info.Title != "" {
				title = info.Title
			}
			g.notify.Send("GuanClip 直播录制结束",
				fmt.Sprintf("《%s》%s；分段 %d 个已%s", title, reason, len(segs), map[bool]string{true: " 送入切片管线", false: " 保存"}[g.cfg.Live.AutoClip && len(segs) > 0]))

			// 下播自动接切片管线：每段一个 clips 任务（本地文件直入）
			if g.cfg.Live.AutoClip && g.OnSegQueued != nil {
				for _, seg := range segs {
					g.OnSegQueued(seg, title)
				}
			}
		},
	}
	g.mu.Lock()
	g.recorders[w.RoomID] = rec
	g.mu.Unlock()

	w.Status = "recording"
	w.LiveStart = time.Now().Unix()
	w.SegDir = segDir
	w.SegCount = 0
	w.Title = info.Title
	w.LastError = ""
	_ = g.store.UpdateWatch(w)
	rec.Start(g.ctx)

	g.notify.Send("GuanClip 开播提醒",
		fmt.Sprintf("《%s》开播了，已自动开始录制（房间 %s）", info.Title, w.RoomID))
}
