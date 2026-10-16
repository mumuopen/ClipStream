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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
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
	// 磁盘清理定时（挂机刚需）：超期会话/队列残留/低水位兜底
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		mins := d.cfg.Retention.CheckMinutes
		if mins <= 0 {
			mins = 60
		}
		// 启动即清一次（顺带得到水位基线）
		if d.cfg.Retention.Enabled {
			if st, err := RunCleanup(d.cfg, d.notify); err == nil {
				fmt.Printf("[cleanup] 启动清理：%d 个超期会话，剩余 %.0f%%\n", st.DeletedSessions, st.FreePercent)
			}
		}
		t := time.NewTicker(time.Duration(mins) * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				if !d.cfg.Retention.Enabled {
					continue
				}
				if st, err := RunCleanup(d.cfg, d.notify); err == nil && (st.DeletedSessions > 0 || st.LowDisk) {
					fmt.Printf("[cleanup] 清理完成：删除 %d 会话，释放 %.2f GB，磁盘剩余 %.0f%%\n",
						st.DeletedSessions, float64(st.FreedBytes)/1e9, st.FreePercent)
				}
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

	d.notify.Send("ClipStream 开播提醒",
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
	d.notify.Send("ClipStream 直播录制结束",
		fmt.Sprintf("《%s》结束（房间 %s）；分段 %d 个", s.title, s.roomID, len(segs)))

	if d.cfg.Live.AutoClip {
		// 碎段流判定（按段数，不按时长）：虎牙 CDN 是 5-15 秒短连接，一场直播留下数百段；
		// 正常 600s 分段一场最多 ~10 段（含断流重连残段），≥20 段即碎段流，合并后送切。
		// 注：不能用 ffprobe 判段时长——直播流切出的 FLV 段 metadata duration 是"流内累计
		// 时间"假值（实测 B站段标 4562s、虎牙段标 5718s，均非本段真实长度）。
		if len(segs) >= 20 {
			merged, err := d.mergeAndQueue(segs, s.title, s.segDir)
			if err != nil {
				fmt.Printf("[queue] 房间 %s 碎段合并失败：%v（%d 段未送切）\n", s.roomID, err, len(segs))
				d.notify.Send("ClipStream 碎段合并失败",
					fmt.Sprintf("《%s》（房间 %s）%d 段未能送切：%v", s.title, s.roomID, len(segs), err))
			} else {
				fmt.Printf("[queue] 房间 %s 碎段合并：%d 段 → %d 个合并段送切\n", s.roomID, len(segs), merged)
			}
		} else {
			// 正常分段流：逐段直接送切（短尾巴残段交管线时长下限兜底）
			for _, seg := range segs {
				if err := d.queueClip(seg, s.title); err != nil {
					d.notify.Send("ClipStream 切片排队失败",
						fmt.Sprintf("分段 %s 送入切片队列失败：%v", seg, err))
				}
			}
		}
	}
}

// mergeAndQueue 把碎段按墙钟跨度分组，FLV tag 级拼接合并为大段后送切。
// 为什么不用 ffmpeg concat demuxer：①虎牙流内嵌非标 script/text 轨（ffmpeg 的 FLV
// demuxer 解析即错位，报 Packet corrupt 并错杀后续 tag）；②CDN 短连接的段尾截断是
// 常态。tag 级拼接剥掉 script 轨、断链止损（丢截断尾），字节透传零转码、零进程开销；
// 各段 tag 时间戳本就续接（内容轴无缝），产物直供管线（Cut 的 -ss 会自动处理非零起点）。
// 合并产物写 segDir/merged_*.flv，避开 seg_*.flv 的 glob（避免 retention/送切重复处理）。
func (d *Daemon) mergeAndQueue(segs []string, title, segDir string) (int, error) {
	target := time.Duration(d.cfg.Live.SegmentSec) * time.Second
	if target <= 0 {
		target = 600 * time.Second
	}
	// 按段文件 mtime（墙钟跨度）分组：碎段流的内容轴有重连空洞（实测虎牙 60fps 流每段
	// 墙钟 ~5.3s 但内容仅 ~2.2s），探不出真实时长，mtime 差是唯一稳定的分组依据
	var groups [][]string
	var cur []string
	var groupStart time.Time
	for _, seg := range segs {
		fi, err := os.Stat(seg)
		if err != nil {
			continue
		}
		if len(cur) > 0 && fi.ModTime().Sub(groupStart) >= target {
			groups = append(groups, cur)
			cur = nil
		}
		if len(cur) == 0 {
			groupStart = fi.ModTime()
		}
		cur = append(cur, seg)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	if len(groups) == 0 {
		return 0, fmt.Errorf("无有效段")
	}

	// seq header 跨段提取（注入用）：h264/AAC 的 sequence header（解码配置）只在流首
	// 出现一次——B站长连接流只在首段有，裸段组缺它会 "No start code found" 视频不可解。
	// 虎牙短连接流每段都自带 seq header（每条连接都是流首），注入重复无害（demuxer
	// 遇 seq header 只是刷新解码配置）。取第一个能扫到的段的 seq header。
	var inject [][]byte
	for _, seg := range segs {
		if tags := extractSeqHeaders(seg); len(tags) > 0 {
			inject = tags
			break
		}
	}

	queued := 0
	for i, grp := range groups {
		out := filepath.Join(segDir, fmt.Sprintf("merged_%03d.flv", i))
		n, meta, err := concatFLV(grp, out, inject)
		if err != nil {
			_ = os.Remove(out)
			return queued, fmt.Errorf("第 %d 组拼接失败: %w", i, err)
		}
		if n == 0 {
			_ = os.Remove(out) // 全空组（每段都解析失败）不送切
			continue
		}
		// 侧车 meta：弹幕热点映射（墙钟时间 → 合并产物内容轴偏移），
		// 读方 pipeline/barrageHotspots；写失败不阻塞送切（仅退化无弹幕信号）
		if mb, jerr := json.Marshal(meta); jerr == nil {
			_ = os.WriteFile(strings.TrimSuffix(out, ".flv")+".meta.json", mb, 0o644)
		}
		if err := d.queueClip(out, title); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

// concatFLV 把一组 FLV 段的音视频 tag 原样顺序写入 out（剥 script 轨 + 断链止损），
// 返回写入 tag 数与拼接 meta（各段墙钟时刻/内容偏移/内容时长，供弹幕热点映射）。
// FLV 结构：header(9B) + prevTagSize0(4B) + [tagType(1B) + size(3B) + ts(3B) +
// tsExt(1B) + streamID(3B) + data(sizeB) + prevTagSize(4B)]*；prevTagSize 应等于本
// tag 总长 11+size，不匹配即流损坏，止损丢弃其后所有字节（CDN 断流时段尾 tag 常
// 写一半，其后本就无数据）。
// 兼容历史裸段（旧版 StreamFLV 无头分段产物）：文件头非 "FLV" 时从 offset 0 按
// tag 流扫描，产物头用合成标准头补齐。inject 为可选的 seq header tag（ts 已清零），
// 写在产物 tag 序列最前，保证每组产物自解码配置（缺 seq header 的裸段组可正常 Cut）。
func concatFLV(files []string, out string, inject [][]byte) (int, core.MergedSegMeta, error) {
	f, err := os.Create(out)
	if err != nil {
		return 0, core.MergedSegMeta{}, err
	}
	defer f.Close()
	// 合成标准头：FLV v1 + audio/video flags + headerSize=9（裸段产物的兜底头）
	synthHeader := []byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9}
	var zeros [4]byte
	total := 0
	var meta core.MergedSegMeta
	var contentOffset float64
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return total, meta, err
		}
		offset := 0
		if len(b) >= 13 && string(b[:3]) == "FLV" {
			offset = 13 // 标准 FLV：跳过 header(9) + prevTagSize0(4)
		} else if len(b) < 15 {
			continue // 太小不成 tag，跳过（宁少勿坏）
		}
		if total == 0 {
			if offset == 13 {
				if _, err := f.Write(b[:9]); err != nil { // 透传原头
					return total, meta, err
				}
				if _, err := f.Write(zeros[:]); err != nil { // prevTagSize0
					return total, meta, err
				}
			} else {
				if _, err := f.Write(synthHeader); err != nil {
					return total, meta, err
				}
				if _, err := f.Write(zeros[:]); err != nil {
					return total, meta, err
				}
			}
			// 注入 seq header（裸段组缺解码配置时视频不可解；重复注入无害）
			for _, tag := range inject {
				if _, err := f.Write(tag); err != nil {
					return total, meta, err
				}
				total++
			}
		}
		// 段级 meta：记录墙钟时刻与内容轴偏移（弹幕热点映射用）
		ms := core.MergedSeg{Name: filepath.Base(file), Offset: contentOffset}
		if fi, err := os.Stat(file); err == nil {
			ms.Wall = fi.ModTime().Unix()
		}
		firstTS, lastTS := uint32(0), uint32(0)
		pos := offset
		for pos+11 <= len(b) {
			t := b[pos]
			size := int(uint32(b[pos+1])<<16 | uint32(b[pos+2])<<8 | uint32(b[pos+3]))
			dataEnd := pos + 11 + size
			if dataEnd+4 > len(b) {
				break // 段尾截断，弃
			}
			if binary.BigEndian.Uint32(b[dataEnd:dataEnd+4]) != uint32(11+size) {
				break // tag 链断裂，其后不可信
			}
			if t == 8 || t == 9 { // 只保留音频/视频，剥 script(18) 等杂轨
				if _, err := f.Write(b[pos : dataEnd+4]); err != nil {
					return total, meta, err
				}
				total++
				// FLV tag ts：Timestamp(3B) + TimestampExtended(1B)，大端毫秒
				ts := uint32(b[pos+4])<<16 | uint32(b[pos+5])<<8 | uint32(b[pos+6]) |
					uint32(b[pos+7])<<24
				if total == len(inject)+1 || firstTS == 0 && lastTS == 0 {
					firstTS = ts
				}
				lastTS = ts
			}
			pos = dataEnd + 4
		}
		if lastTS >= firstTS {
			ms.Dur = float64(lastTS-firstTS) / 1000
		}
		contentOffset += ms.Dur
		meta.Segs = append(meta.Segs, ms)
	}
	if n := len(meta.Segs); n > 0 {
		// 组窗口：首段内容开始 ≈ 首段完成时刻 - 内容时长；末段完成时刻即终点
		meta.Start = meta.Segs[0].Wall - int64(meta.Segs[0].Dur)
		meta.End = meta.Segs[n-1].Wall
	}
	return total, meta, nil
}

// extractSeqHeaders 从段文件头部提取 FLV sequence header tag（视频 AVC 解码配置/
// 音频 AudioSpecificConfig）的原始字节，ts 清零供拼接产物注入。seq header 总在流首的
// 前几个 tag 里，只读前 64KB 即可。识别：video tag 数据首字节 0x17（keyframe+AVC）
// 且次字节 0x00（AVCSeqHeader）；audio tag 数据首字节高 4bit 0xA（AAC）且次字节 0x00。
func extractSeqHeaders(path string) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	b := head[:n]
	if n < 15 || string(b[:3]) != "FLV" {
		return nil
	}
	var out [][]byte
	pos := 13
	for pos+11 <= len(b) && len(out) < 2 {
		size := int(uint32(b[pos+1])<<16 | uint32(b[pos+2])<<8 | uint32(b[pos+3]))
		dataEnd := pos + 11 + size
		if dataEnd+4 > len(b) {
			break
		}
		t := b[pos]
		var d0, d1 byte
		if size >= 1 {
			d0 = b[pos+11]
		}
		if size >= 2 {
			d1 = b[pos+12]
		}
		isVideoSeq := t == 9 && d0 == 0x17 && d1 == 0x00
		isAudioSeq := t == 8 && (d0&0xF0) == 0xA0 && d1 == 0x00
		if isVideoSeq || isAudioSeq {
			tag := append([]byte{}, b[pos:dataEnd+4]...)
			tag[4], tag[5], tag[6], tag[7] = 0, 0, 0, 0 // ts 清零（注入产物开头）
			out = append(out, tag)
		}
		pos = dataEnd + 4
	}
	return out
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

// listSegFiles 列出目录下有效分段（>64KB：虎牙 60fps 短连接碎段最小实测 ~76KB/2s，
// 旧阈值 1MB 会把碎段流九成以上的段全部漏掉；空文件/写一半的段仍被忽略）
func listSegFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "seg_*.flv"))
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Size() > 64<<10 {
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