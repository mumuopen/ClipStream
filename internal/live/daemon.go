package live

// 直播录制守护进程核心（M4.1 重写版）。
// 背景：Windows 上 serve 进程（rod/stealth 重依赖）拉起的子进程无法做 FLV 流式 IO
//（实测 10+ 启动方式全部 0KB），而独立进程内拉流 100% 正常。
// 因此录制职责从 serve 内 Guard 拆给独立 daemon 进程：
//   - Daemon 自行轮询 watches（SQLite WAL 多进程安全）
//   - 开播 → 进程内 StreamFLV 拉流（不 exec 任何子进程）
//   - 状态写回 watches 表，serve 只读展示；心跳文件落盘供 serve 探测存活
//   - 下播 → 分段写入 data/live/queue/ 队列文件，serve 侧 watcher 消费送入切片管线

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

// Daemon 直播录制守护进程
type Daemon struct {
	cfg    *config.Config
	store  *store.Store
	notify *notify.Pushme

	mu       sync.Mutex
	sessions map[string]*liveSession // roomID → 录制会话

	ctx    context.Context
	stopFn context.CancelFunc
	wg     sync.WaitGroup
}

// liveSession 单场直播录制会话（进程内拉流 + 弹幕采集）
type liveSession struct {
	roomID   string
	platform string
	segDir   string
	title    string
segCount atomic.Int32
	danmaku  danmakuCollector

	cancel context.CancelFunc
	done   chan struct{}
}

// danmakuCollector 弹幕采集器公共接口（B站 WS / 虎牙 TACG 共用）
type danmakuCollector interface {
	Start(parent context.Context)
	Stop()
}

func (s *liveSession) SegCount() int32 { return s.segCount.Load() }

func (s *liveSession) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// NewDaemon 创建录制守护进程
func NewDaemon(cfg *config.Config, st *store.Store, n *notify.Pushme) *Daemon {
	return &Daemon{
		cfg:      cfg,
		store:    st,
		notify:   n,
		sessions: map[string]*liveSession{},
	}
}

// Start 启动守护循环（非阻塞）
func (d *Daemon) Start() {
	d.ctx, d.stopFn = context.WithCancel(context.Background())
	interval := time.Duration(d.cfg.Live.CheckIntervalSec) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.tick() // 启动立即查一轮
		d.heartbeat()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				d.tick()
				d.heartbeat()
			}
		}
	}()
}

// Stop 停止轮询与全部录制会话（daemon 退出时调用）
func (d *Daemon) Stop() {
	if d.stopFn != nil {
		d.stopFn()
	}
	// 先取消录制会话，再等循环与会话 goroutine 退出
	d.mu.Lock()
	for _, s := range d.sessions {
		s.cancel()
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// HeartbeatFile 心跳文件路径（serve 据此判断 daemon 存活）
func (d *Daemon) HeartbeatFile() string {
	return filepath.Join(d.cfg.DataDir, "live", "daemon.heartbeat")
}

// QueueDir 下播分段产物队列目录（serve 消费送切片管线）
func (d *Daemon) QueueDir() string {
	return filepath.Join(d.cfg.DataDir, "live", "queue")
}

func (d *Daemon) heartbeat() {
	dir := filepath.Join(d.cfg.DataDir, "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(d.HeartbeatFile(), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

// tick 一轮轮询
func (d *Daemon) tick() {
	watches, err := d.store.ListWatches()
	if err != nil {
		return
	}
	for _, w := range watches {
		d.check(w)
	}
}

// check 单直播间状态机：recording→确认会话存活；idle/error→开播检测
func (d *Daemon) check(w *store.Watch) {
	if w.Status == "recording" {
		d.mu.Lock()
		s := d.sessions[w.RoomID]
		d.mu.Unlock()
		if s == nil || s.isDone() {
			// 会话已结束（endSession 应已置 idle；此处兜底清理）
			d.mu.Lock()
			delete(d.sessions, w.RoomID)
			d.mu.Unlock()
			if w.Status == "recording" {
				w.Status = "idle"
				_ = d.store.UpdateWatch(w)
			}
			// 兜底后落进下方开播检测（同轮重新判定）
		} else {
			w.SegCount = int(s.SegCount())
			w.Title = s.title
			_ = d.store.UpdateWatch(w)
			return
		}
	}

	// 空闲/error：开播检测（API 失败标 error 留状态，下轮重试不判死）
	ctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
	defer cancel()
	info, err := d.roomInfo(ctx, w.Platform, w.RoomID)
	if err != nil {
		if w.Status != "error" {
			w.Status = "error"
			w.LastError = err.Error()
			_ = d.store.UpdateWatch(w)
		}
		return
	}
	if w.Title != info.Title {
		w.Title = info.Title
	}
	if info.LiveStatus != 1 {
		if w.Status != "idle" {
			w.Status = "idle"
			w.LastError = ""
			_ = d.store.UpdateWatch(w)
		} else if w.Title != "" {
			_ = d.store.UpdateWatch(w)
		}
		return
	}

	d.startSession(w, info)
}

// startSession 创建录制会话并立即拉流
func (d *Daemon) startSession(w *store.Watch, info *RoomInfo) {
	segDir := filepath.Join(d.cfg.DataDir, "live", fmt.Sprintf("%s_%s", w.RoomID, time.Now().Format("20060102_150405")))
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(d.ctx)
	s := &liveSession{
		roomID:   w.RoomID,
		platform: w.Platform,
		segDir:   segDir,
		title:    info.Title,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	d.mu.Lock()
	if old := d.sessions[w.RoomID]; old != nil {
		old.cancel() // 防重复启动：顶掉旧会话
	}
	d.sessions[w.RoomID] = s
	d.mu.Unlock()

		w.Status = "recording"
	w.LiveStart = time.Now().Unix()
	w.SegDir = segDir
	w.SegCount = 0
	w.Title = info.Title
	w.LastError = ""
	_ = d.store.UpdateWatch(w)

	// 弹幕采集（M4.2）：5s 密度桶落库，供曲线展示与双信号高光（B站 WS / 虎牙 TACG）
	if w.Platform == "" || w.Platform == "bilibili" {
		dc := NewDanmakuCollector(info.RoomID)
		dc.OnLog = func(format string, args ...any) {
			fmt.Printf("[danmaku %s] %s\n", w.RoomID, fmt.Sprintf(format, args...))
		}
		dc.OnBucket = func(roomID string, bucketStart int64, count int) {
			if err := d.store.AddBarrage(roomID, bucketStart, count); err != nil {
				dc.log("弹幕桶落库失败：%v", err)
			}
		}
		s.danmaku = dc
		dc.Start(ctx)
	} else if w.Platform == "huya" {
		hc := NewHuyaDanmakuCollector(info.RoomID, info.Yyid, info.TopSid, info.Sid)
		hc.OnLog = func(format string, args ...any) {
			fmt.Printf("[danmaku-hy %s] %s\n", w.RoomID, fmt.Sprintf(format, args...))
		}
		hc.OnBucket = func(roomID string, bucketStart int64, count int) {
			if err := d.store.AddBarrage(roomID, bucketStart, count); err != nil {
				hc.log("弹幕桶落库失败：%v", err)
			}
		}
		s.danmaku = hc
		hc.Start(ctx)
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(s.done)
		d.runLoop(ctx, s)
	}()

	d.notify.Send("GuanClip 开播提醒",
		fmt.Sprintf("《%s》开播了，已开始自动录制（房间 %s）", info.Title, w.RoomID))
}

// runLoop 会话录制循环：拉流 → 断流判定 → 重连/结束
func (d *Daemon) runLoop(ctx context.Context, s *liveSession) {
	defer d.endSession(s)
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		// API 调用用独立 ctx，避免会话取消误伤检测
		playURL, err := d.playURL(context.Background(), s.platform, s.roomID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if attempt >= 3 {
				info, err2 := d.roomInfo(context.Background(), s.platform, s.roomID)
				if err2 != nil || info.LiveStatus == 0 {
					return // 下播
				}
				attempt = 0
			}
			if !sleepCtx(ctx, 15*time.Second) {
				return
			}
			continue
		}

		// 进程内拉流（分段写 segDir，onSeg 回调更新段计数；首段即算 1；重连续写段号）
		_, _, err = StreamFLV(ctx, playURL, s.segDir, d.cfg.Live.SegmentSec, countSegFiles(s.segDir),
			func(segIdx int, _ int64) { s.segCount.Store(int32(segIdx + 1)) })
		if ctx.Err() != nil {
			return
		}
		// 流断后探测直播间状态：API 失败 ≠ 下播，连续 3 次才收工
		down := false
		for probe := 0; probe < 3; probe++ {
			info, err2 := d.roomInfo(context.Background(), s.platform, s.roomID)
			if err2 == nil {
				down = info.LiveStatus == 0
				break
			}
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
		}
		if down {
			return // 下播收工
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

// endSession 会话收尾：状态回写 + 通知 + 分段入切片队列
func (d *Daemon) endSession(s *liveSession) {
	// 停弹幕采集（flush 残余桶）
	if s.danmaku != nil {
		s.danmaku.Stop()
	}
	d.mu.Lock()
	if d.sessions[s.roomID] == s {
		delete(d.sessions, s.roomID)
	}
	d.mu.Unlock()

	segs := listSegFiles(s.segDir)
	if w, err := d.store.WatchByRoomID(s.roomID); err == nil {
		w.Status = "idle"
		w.SegCount = len(segs)
		_ = d.store.UpdateWatch(w)
	}
	d.notify.Send("GuanClip 直播录制结束",
		fmt.Sprintf("《%s》结束（房间 %s）；分段 %d 个", s.title, s.roomID, len(segs)))

	if d.cfg.Live.AutoClip {
		for _, seg := range segs {
			if err := d.queueClip(seg, s.title); err != nil {
				d.notify.Send("GuanClip 切片排队失败",
					fmt.Sprintf("分段 %s 送入切片队列失败：%v", seg, err))
			}
		}
	}
}

// queueClip 写切片队列文件（serve 侧 watcher 消费）；内容 = 分段路径 \t 标题
func (d *Daemon) queueClip(segPath, title string) error {
	queueDir := d.QueueDir()
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(queueDir, fmt.Sprintf("clip-%d.seg", time.Now().UnixNano())))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\n", segPath, title)
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// listSegFiles 列出目录下 >1MB 的有效分段（忽略残段）
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

// countSegFiles 统计目录下已有段文件数（含残段，重连续写用）
func countSegFiles(dir string) int {
	matches, _ := filepath.Glob(filepath.Join(dir, "seg_*.flv"))
	return len(matches)
}

// ── 平台分发（M4 多平台：bilibili / huya）──

// roomInfo 按平台获取直播间状态（统一转成 RoomInfo 结构）
func (d *Daemon) roomInfo(ctx context.Context, platform, roomID string) (*RoomInfo, error) {
	switch platform {
	case "huya":
		h, err := GetHuyaRoomInfo(ctx, roomID)
		if err != nil {
			return nil, err
		}
		st := 0
		if h.LiveStatus == 2 {
			st = 1
		}
		return &RoomInfo{RoomID: h.RoomID, Title: h.Title, LiveStatus: st,
			Yyid: h.Yyid, TopSid: h.TopSid, Sid: h.SubSid}, nil
	default: // bilibili
		return GetRoomInfo(ctx, roomID)
	}
}

// playURL 获取平台 FLV 播放地址（虎牙优先选第一条线路）
func (d *Daemon) playURL(ctx context.Context, platform, roomID string) (string, error) {
	switch platform {
	case "huya":
		h, err := GetHuyaRoomInfo(ctx, roomID)
		if err != nil {
			return "", err
		}
		urls, err := HuyaPlayURLs(h)
		if err != nil {
			return "", err
		}
		return urls[0], nil
	default: // bilibili
		return GetPlayURL(ctx, roomID)
	}
}