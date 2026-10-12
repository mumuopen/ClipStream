package live

// 虎牙弹幕采集器（M4.2）：直连 va.huya.com 边缘节点 + 页面同款订阅协议。
//
// 2026-10-12 Playwright 页面抓包 + Go 复刻实测定案（替换此前失败的 wsapi 方案）：
//   1. 连接 wss://<hash>-ws.va.huya.com/?baseinfo=<TARS>——baseinfo 必须是页面 JS 同款
//      （虎牙前端 tars 变体：type6=字符串；结构含 struct 包裹/guid/UA/HUYA&ZH&2052/
//      时间戳数字段/HUYA_NET map），自拼标准 tars 会被服务器静默忽略（零响应根因）。
//   2. 连上后发 "live:0" 订阅帧（页面照抄字节）。
//   3. 再发频道订阅帧：09 00 02 06 <len>"live:<uid>" 06 <len>"chat:<uid>" 16 00 …（uid=主播号）。
//   4. 15s 心跳（9B 固定字节）。
//   5. 推送帧里宽松提取 UTF-8 中文即弹幕文本（密度桶只需计数，无需完整解析 wup 结构）。
// 实测 room 441195（胖炸）：30s 96 帧弹幕。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// va 边缘节点候选（页面实测可用，逐个尝试）
var huyaVaHosts = []string{
	"ab0f2428-ws.va.huya.com",
	"ab0f242a-ws.va.huya.com",
	"3ad7b133-ws.va.huya.com",
	"b461f4a4-ws.va.huya.com",
}

// 页面同款 baseinfo（2026-10-12 抓包；含时间戳字段，静态可用，失效时需重抓）
const huyaPageBaseinfo = "DBYgMGE3ZGE3MDVjNTYwY2M2YTNjMDEyMGJkMjFiZGFkNDAmGndlYmg1JjI2MTAxMDE0MzImd2Vic29ja2V0NgxIVVlBJlpIJjIwNTJGAFY+NDQyOTkuNzY5NDksNDkwMjMuODgwMTAsNTc0MTYuMTA1MTM4LDU4NDc0LjEwNjkwMiw1ODY2NC4xMDcyNjRsdgCGAJYAqAACBghIVVlBX05FVBYBMAYLSFVZQV9WU0RLVUEWGndlYmg1JjI2MTAxMDE0MzImd2Vic29ja2V0"

// "live:0" 订阅帧（页面照抄）
var huyaFrameLive0 = []byte{0x00, 0x10, 0x1D, 0x00, 0x00, 0x0D, 0x09, 0x00, 0x01, 0x06, 0x06, 'l', 'i', 'v', 'e', ':', '0', 0x16, 0x00, 0x2C, 0x36, 0x00, 0x4C, 0x5C, 0x66, 0x00}

// 心跳（9B 固定）
var huyaHeartbeat = []byte{0x00, 0x14, 0x1D, 0x00, 0x0C, 0x2C, 0x36, 0x00, 0x4C}

// huyaSubscribeFrame 频道订阅帧：订阅 "live:<uid>" + "chat:<uid>"（页面 #12 帧动态化）
func huyaSubscribeFrame(uid string) []byte {
	live := "live:" + uid
	chat := "chat:" + uid
	payload := []byte{0x09, 0x00, 0x02, 0x06, byte(len(live))}
	payload = append(payload, live...)
	payload = append(payload, 0x06, byte(len(chat)))
	payload = append(payload, chat...)
	payload = append(payload, 0x16, 0x00, 0x20, 0x07, 0x36, 0x00, 0x4C, 0x5C, 0x66, 0x00)
	frame := []byte{0x00, 0x10, 0x1D, 0x00, byte(len(payload) >> 8), byte(len(payload))}
	return append(frame, payload...)
}

// HuyaDanmakuCollector 虎牙弹幕采集器（进程内直连 va 边缘节点，断线自动重连）
type HuyaDanmakuCollector struct {
	RoomID string
	Yyid   int64 // 房间 yyid（保留；订阅用 TopSid）
	TopSid int64 // 主播 uid（订阅频道 live:<uid>/chat:<uid>）
	Sid    int64

	// OnBucket 密度桶（5s）回调（落库/曲线展示）
	OnBucket func(roomID string, bucketStart int64, count int)
	// OnLog 进度日志（可空）
	OnLog func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lastBucket int64
	curCount   int
	lastBeat   atomic.Int64
}

func NewHuyaDanmakuCollector(roomID string, yyyid, topSid, sid int64) *HuyaDanmakuCollector {
	return &HuyaDanmakuCollector{RoomID: roomID, Yyid: yyyid, TopSid: topSid, Sid: sid}
}

func (c *HuyaDanmakuCollector) log(format string, args ...any) {
	if c.OnLog != nil {
		c.OnLog(format, args...)
	}
}

// Start 启动采集（非阻塞）
func (c *HuyaDanmakuCollector) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.ctx = ctx
	c.cancel = cancel
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		defer c.flushBucket()
		failStreak := 0
		for {
			if ctx.Err() != nil {
				return
			}
			err := c.connectAndRead(ctx)
			if ctx.Err() != nil {
				return
			}
			failStreak++
			if failStreak == 1 || failStreak%10 == 0 {
				c.log("虎牙弹幕断开，5s 后重连（第 %d 次）：%v", failStreak, err)
			}
			if failStreak >= 3 {
				failStreak = 1
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

func (c *HuyaDanmakuCollector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	<-c.done
}

// Alive 近 120s 内有数据
func (c *HuyaDanmakuCollector) Alive() bool {
	return time.Now().Unix()-c.lastBeat.Load() < 120
}

func (c *HuyaDanmakuCollector) connectAndRead(ctx context.Context) error {
	// 逐 va 节点尝试（首个成功的即用）
	var wsConn net.Conn
	var host string
	var err error
	for _, h := range huyaVaHosts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wsConn, err = dialHuyaVa(ctx, h)
		if err == nil {
			host = h
			break
		}
		c.log("va 节点 %s 失败：%v", h, err)
	}
	if wsConn == nil {
		return fmt.Errorf("全部 va 节点失败: %w", err)
	}
	defer wsConn.Close()
	c.lastBeat.Store(time.Now().Unix())

	// Go 标准 TLS + gobwas 握手已在 dialHuyaVa 完成；发订阅序列
	if err := wsutil.WriteClientBinary(wsConn, huyaFrameLive0); err != nil {
		return err
	}
	if err := wsutil.WriteClientBinary(wsConn, huyaSubscribeFrame(strconv.FormatInt(c.TopSid, 10))); err != nil {
		return err
	}
	c.log("虎牙弹幕已订阅（节点 %s，频道 live:%d/chat:%d）", host, c.TopSid, c.TopSid)

	// 心跳（15s）
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hbDone:
				return
			case <-t.C:
				_ = wsutil.WriteClientBinary(wsConn, huyaHeartbeat)
			}
		}
	}()

	// 读循环：宽松识别弹幕帧（含 UTF-8 中文且 <600B）
	wsConn.SetReadDeadline(time.Now().Add(120 * time.Second))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, op, err := wsutil.ReadServerData(wsConn)
		if err != nil {
			return err
		}
		_ = wsConn.SetReadDeadline(time.Now().Add(120 * time.Second))
		c.lastBeat.Store(time.Now().Unix())
		if op == ws.OpBinary && isHuyaDanmuFrame(data) {
			c.accumulate(time.Now().Unix())
		}
	}
}

// isHuyaDanmuFrame 宽松弹幕识别：小帧且含 UTF-8 中文（推送里混系统消息，中文即观众发言）
func isHuyaDanmuFrame(b []byte) bool {
	if len(b) == 0 || len(b) >= 600 {
		return false
	}
	for i := 0; i+2 < len(b); i++ {
		if b[i] >= 0xE4 && b[i] <= 0xE9 && b[i+1] >= 0x80 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF {
			return true
		}
	}
	return false
}

func (c *HuyaDanmakuCollector) accumulate(now int64) {
	b := now / barrageBucketSec * barrageBucketSec
	if c.lastBucket == 0 {
		c.lastBucket = b
		c.curCount = 0
	}
	if b != c.lastBucket {
		c.flushBucket()
		c.lastBucket = b
		c.curCount = 0
	}
	c.curCount++
}

func (c *HuyaDanmakuCollector) flushBucket() {
	if c.lastBucket != 0 && c.curCount > 0 && c.OnBucket != nil {
		c.OnBucket(c.RoomID, c.lastBucket, c.curCount)
	}
	c.lastBucket = 0
	c.curCount = 0
}

// WsURLForTest 导出连接 URL（调试用；新协议下无独立 URL 拼接，保留占位）
func (c *HuyaDanmakuCollector) WsURLForTest() string {
	return "wss://" + huyaVaHosts[0] + "/?baseinfo=" + huyaPageBaseinfo
}

// JoinPacketForTest 导出订阅帧（调试/测试用）
func (c *HuyaDanmakuCollector) JoinPacketForTest() []byte {
	return huyaSubscribeFrame(strconv.FormatInt(c.TopSid, 10))
}

// dialHuyaVa 建立 TLS + gobwas WS 连接（返回已握手连接）
func dialHuyaVa(ctx context.Context, host string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	tcp, err := d.DialContext(ctx, "tcp", host+":443")
	if err != nil {
		return nil, err
	}
	tc := tls.Client(tcp, &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}})
	if err := tc.HandshakeContext(ctx); err != nil {
		tcp.Close()
		return nil, err
	}
	wsConn, _, _, err := ws.Dialer{
		Timeout: 10 * time.Second,
		Host:   host,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tc, nil
		},
	}.Dial(ctx, "ws://"+host+":443/?baseinfo="+huyaPageBaseinfo)
	if err != nil {
		tc.Close()
		return nil, err
	}
	return wsConn, nil
}