package live

// 虎牙弹幕采集器（M4.2 弹幕）：TACG 协议 WS 直连
// 血统：对齐 simple_live_reborn（2026-05）的 HuyaDanmaku——新协议：
//   - 连接 wss://wsapi.huya.com?baseinfo=<base64>（baseinfo 为 TARS 编码的 uid/guid/ua 等，
//     旧 cdnws 入口已被服务器静默拒绝——2026-05 协议升级）
//   - 连接后发 join：外层 {cmd:1, body:内层}，内层 {ayyuid,true,"","",topSid,sid,0,0}
//   - 心跳 60s：固定字节 base64 "ABQdAAwsNgBM"
//   - 消息：外层 tag0 int，type==7 推送（tag1 bytes=HYPushMessage；uri 1400=弹幕/8006=在线）
//   - 弹幕按 5s 桶聚合密度（与 B站采集器同契约回调）

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 2026 新协议常量（对齐 simple_live_reborn）
const (
	huyaDanmuHost = "wsapi.huya.com"
	huyaDanmuUA   = "webh5&2604281646&websocket" // webDanmakuUA（服务端校验）
)

var (
	huyaPushType  = int64(7) // 外层 tag0 值：推送消息
	huyaDanmuURI  = int64(1400)
	huyaOnlineURI = int64(8006)
	huyaHeartbeatB = func() []byte {
		b, _ := base64.StdEncoding.DecodeString("ABQdAAwsNgBM")
		return b
	}()
)

// HuyaDanmakuCollector 虎牙弹幕采集器（进程内，断线自动重连）
type HuyaDanmakuCollector struct {
	RoomID string
	Yyid   int64 // ayyuid（房间 lYyid）
	TopSid int64 // 频道号（lChannelId）
	Sid    int64 // 子频道（lSubChannelId）

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
				c.log("虎牙弹幕断开，3s 后重连（第 %d 次）：%v", failStreak, err)
			}
			if failStreak >= 3 {
				failStreak = 1
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
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
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.wsURL(), http.Header{})
	if err != nil {
		return fmt.Errorf("虎牙弹幕 WS 连接失败: %w", err)
	}
	defer conn.Close()
	c.lastBeat.Store(time.Now().Unix())

	// 入房包
	if err := conn.WriteMessage(websocket.BinaryMessage, c.joinPacket()); err != nil {
		return err
	}
	c.log("虎牙弹幕入房成功（room %s ayyuid=%d tid=%d）", c.RoomID, c.Yyid, c.TopSid)

	// 心跳 60s
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hbDone:
				return
			case <-t.C:
				_ = conn.WriteMessage(websocket.BinaryMessage, huyaHeartbeatB)
			}
		}
	}()

	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		c.lastBeat.Store(time.Now().Unix())
		c.handleFrame(data)
	}
}

// wsURL 构建带 baseinfo 认证的 WS 地址（2026 新协议）
func (c *HuyaDanmakuCollector) wsURL() string {
	var os tarsWriter
	os.writeInt(0, 0) // uid（匿名）
	os.writeString(1, huyaGUID())
	os.writeString(2, huyaDanmuUA)
	os.writeString(3, "HUYA&ZH&2052")
	os.writeString(4, "")
	os.writeString(5, "")
	os.writeInt(6, 0)
	os.writeString(7, "")
	os.writeString(8, "")
	os.writeString(9, "")
	os.writeStringMap(10, map[string]string{"HUYA_NET": "0", "HUYA_VSDKUA": huyaDanmuUA})

	baseInfo := base64.StdEncoding.EncodeToString(os.bytes())
	return "wss://" + huyaDanmuHost + "?baseinfo=" + url.QueryEscape(baseInfo)
}

// huyaGUID 随机 16 字节 hex（浏览器 cookie 同款格式）
func huyaGUID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return hex.EncodeToString(b)
}

// WsURLForTest 导出于 ws 地址（调试用）
func (c *HuyaDanmakuCollector) WsURLForTest() string { return c.wsURL() }

// JoinPacketForTest 导出入房包（调试/测试用）
func (c *HuyaDanmakuCollector) JoinPacketForTest() []byte { return c.joinPacket() }

// joinPacket 入房包（对齐 SimpleLive getJoinData）
func (c *HuyaDanmakuCollector) joinPacket() []byte {
	var inner tarsWriter
	inner.writeInt(0, c.Yyid)
	inner.writeBool(1, true)
	inner.writeString(2, "")
	inner.writeString(3, "")
	inner.writeInt(4, c.TopSid)
	inner.writeInt(5, c.Sid)
	inner.writeInt(6, 0)
	inner.writeInt(7, 0)
	var outer tarsWriter
	outer.writeInt(0, 1)
	outer.writeBytes(1, inner.bytes())
	return outer.bytes()
}

// handleFrame 处理一帧：外层 {tag0 int=7, tag1 bytes=HYPushMessage}
func (c *HuyaDanmakuCollector) handleFrame(data []byte) {
	r := newTarsReader(data)
	// tag0：推送类型
	tag, typ, err := r.readHead()
	if err != nil {
		return
	}
	if tag != 0 {
		return
	}
	pushType := r.readInt(typ)
	if pushType != huyaPushType {
		return
	}
	// tag1：HYPushMessage 字节
	tag, typ, err = r.readHead()
	if err != nil || tag != 1 {
		return
	}
	push := r.readBytes(typ)
	if push == nil {
		return
	}
	c.handlePush(push)
}

// handlePush 解析 HYPushMessage：uri(tag1) / msg(tag2)
func (c *HuyaDanmakuCollector) handlePush(b []byte) {
	r := newTarsReader(b)
	uri := int64(0)
	var msg []byte
	for {
		tag, typ, err := r.readHead()
		if err != nil {
			break
		}
		switch tag {
		case 1:
			uri = r.readInt(typ)
		case 2:
			msg = r.readBytes(typ)
		default:
			r.skip(typ)
		}
	}
	switch uri {
	case huyaDanmuURI:
		c.handleDanmu(msg)
	case huyaOnlineURI:
		// 在线人数（可选更新）
	}
}

// handleDanmu 解析 HYMessage：content(tag3)；弹幕即计数入密度桶
func (c *HuyaDanmakuCollector) handleDanmu(b []byte) {
	r := newTarsReader(b)
	hasContent := false
	for {
		tag, typ, err := r.readHead()
		if err != nil {
			break
		}
		switch tag {
		case 3: // content
			content := string(r.readBytes(typ))
			if content != "" {
				hasContent = true
			}
		default:
			r.skip(typ)
		}
	}
	if hasContent {
		c.accumulate(time.Now().Unix())
	}
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