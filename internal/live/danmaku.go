package live

// B站直播弹幕采集器（M4.2）：WebSocket 直连 broadcastlv.chat.bilibili.com，
// 解析 DANMU_MSG，按 5s 桶聚合弹幕密度，回调落库/展示。
// 血统：对齐原版 ChopperBot barrage/BarrageScoreCurve（弹幕密度→高光信号）。
//
// 协议要点（2026-10 实测确认）：
//   - 帧头 16B：packetLen(4) | headerLen(2)=16 | protover(2) | op(4) | seq(4)，大端
//   - 认证 op=7：{"uid":0,"roomid":<realRoomID>,"protover":2,"platform":"web","type":2}
//   - 心跳 op=2 每 30s；服务端回 op=3（4 字节人气值）
//   - 消息 op=5：body 为 JSON（protover=2 时 zlib 压缩，解压后可能是多条 JSON 拼接）
//   - 弹幕 cmd=DANMU_MSG：info[1]=文本，info[0][4]=发送时间戳（秒）
//   - 一帧可能含多个 16B 头包；解压后可能多条 JSON——都需要拆解

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	barrageBucketSec = 5 // 密度桶大小：5s

	opHeartbeat    = 2
	opHeartbeatRpl = 3
	opMessage      = 5
	opAuth         = 7
	opAuthReply    = 8

	protoJSON = 0
	protoZlib = 2

	// 认证/心跳包头协议版本固定为 1（B站服务端校验，传 0 会被断开）
	authProtoVer = 1
)

var barrageWSUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// DanmakuCollector 弹幕采集器：进程内 WS 直连，断线自动重连，ctx 取消退出
type DanmakuCollector struct {
	RoomID string

	// OnBucket 每个密度桶（5s）结束时回调（含最后残余桶）
	OnBucket func(roomID string, bucketStart int64, count int)
	// OnLog 进度日志（可为 nil）
	OnLog func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// 聚合状态（只在 run goroutine 内访问）
	lastBucket int64
	curCount   int
	lastBeat   atomic.Int64 // 最近心跳/弹幕时间（存活探测）
}

func NewDanmakuCollector(roomID string) *DanmakuCollector {
	return &DanmakuCollector{RoomID: roomID}
}

func (c *DanmakuCollector) log(format string, args ...any) {
	if c.OnLog != nil {
		c.OnLog(format, args...)
	}
}

// Start 启动采集（非阻塞）；退出时 flush 残余桶
func (c *DanmakuCollector) Start(parent context.Context) {
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
			// 失败重试日志限流：连续失败只打 1/10，避免刷屏
			failStreak++
			if failStreak == 1 || failStreak%10 == 0 {
				c.log("弹幕连接断开，3s 后重连（第 %d 次失败）：%v", failStreak, err)
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

// Stop 停止采集并返回
func (c *DanmakuCollector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	<-c.done
}

// Alive 近 90s 内有心跳/弹幕（供状态展示）
func (c *DanmakuCollector) Alive() bool {
	return time.Now().Unix()-c.lastBeat.Load() < 90
}

// connectAndRead 单次 WS 会话：getDanmuInfo 拿节点列表 → 逐节点认证 → 心跳 → 读循环
func (c *DanmakuCollector) connectAndRead(ctx context.Context) error {
	// 新版协议：必须经 getDanmuInfo 拿专用 host + token（直连旧入口已失效）
	servers, err := GetDanmuInfo(ctx, c.RoomID)
	if err != nil {
		return fmt.Errorf("获取弹幕接入点失败: %w", err)
	}
	var lastErr error
	for _, ds := range servers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = c.session(ctx, ds)
		if lastErr == nil || ctx.Err() != nil {
			return lastErr // 正常结束或主动取消
		}
		c.log("节点 %s 失败：%v", ds.Host, lastErr)
	}
	return lastErr
}

// session 单个节点的完整会话（TLS → WS 握手 → 认证 → 心跳 → 读循环）
// 2026-10 实测定型（详见当天日志）：
//   - Go 标准 TLS（NextProtos http/1.1）即可，utls Chrome 指纹反而被拉黑拒连
//   - gobwas 握手带 Origin/UA；无子协议（服务器不回 chat，无需要求）
//   - 认证 uid=0（对齐浏览器实测，uid=1 会 1006）+ getDanmuInfo token
//   - 读帧用 wsutil.ReadServerData（客户端视角；ReadClientData 方向反了会报 mask 错误）
func (c *DanmakuCollector) session(ctx context.Context, ds *DanmuServer) error {
	hostport := net.JoinHostPort(ds.Host, strconv.Itoa(ds.WssPort))

	// ① Go 标准 TLS
	d := net.Dialer{Timeout: 10 * time.Second}
	tcp, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return fmt.Errorf("弹幕 TCP 连接失败: %w", err)
	}
	tlsConn := tls.Client(tcp, &tls.Config{ServerName: ds.Host, NextProtos: []string{"http/1.1"}})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tcp.Close()
		return fmt.Errorf("弹幕 TLS 握手失败: %w", err)
	}
	defer tlsConn.Close()
	c.lastBeat.Store(time.Now().Unix())

	// ② gobwas WS 握手（ws:// 复用已建 TLS 连接）
	wsConn, _, _, err := ws.Dialer{
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tlsConn, nil
		},
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"User-Agent": []string{barrageWSUserAgent},
			"Origin":     []string{"https://live.bilibili.com"},
		}),
	}.Dial(ctx, "ws://"+hostport+"/sub")
	if err != nil {
		return fmt.Errorf("弹幕 WS 握手失败: %w", err)
	}
	defer wsConn.Close()

	// ③ 认证包（uid=0 对齐浏览器 + getDanmuInfo token）
	roomIDNum, _ := strconv.Atoi(c.RoomID)
	auth, _ := json.Marshal(map[string]any{
		"uid": 0, "roomid": roomIDNum, "protover": 2,
		"platform": "web", "type": 2, "key": ds.Token,
	})
	if err := wsutil.WriteClientBinary(wsConn, buildPacket(opAuth, authProtoVer, auth)); err != nil {
		return err
	}

	// ④ 心跳（30s 一次）
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatDone:
				return
			case <-t.C:
				_ = wsutil.WriteClientBinary(wsConn, buildPacket(opHeartbeat, authProtoVer, []byte("[object Object]")))
			}
		}
	}()

	// ⑤ 读循环
	wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
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
		_ = wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
		if op == ws.OpBinary {
			c.handleFrame(data)
		}
	}
}

// buildPacket 组装 B站弹幕协议帧
func buildPacket(op int32, protover int16, body []byte) []byte {
	total := 16 + len(body)
	buf := make([]byte, total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	binary.BigEndian.PutUint16(buf[4:6], 16)
	binary.BigEndian.PutUint16(buf[6:8], uint16(protover))
	binary.BigEndian.PutUint32(buf[8:12], uint32(op))
	binary.BigEndian.PutUint32(buf[12:16], 1)
	copy(buf[16:], body)
	return buf
}

// handleFrame 处理一帧（可能含多个 16B 头包）
func (c *DanmakuCollector) handleFrame(frame []byte) {
	for len(frame) >= 16 {
		pktLen := int(binary.BigEndian.Uint32(frame[0:4]))
		if pktLen < 16 || pktLen > len(frame) {
			return // 帧不完整/损坏，丢弃
		}
		c.handlePacket(frame[:pktLen])
		frame = frame[pktLen:]
	}
}

func (c *DanmakuCollector) handlePacket(pkt []byte) {
	headerLen := int(binary.BigEndian.Uint16(pkt[4:6]))
	protover := int16(binary.BigEndian.Uint16(pkt[6:8]))
	op := int32(binary.BigEndian.Uint32(pkt[8:12]))
	body := pkt[headerLen:]

	switch op {
	case opHeartbeatRpl:
		c.lastBeat.Store(time.Now().Unix())
		return
	case opAuthReply:
		c.log("弹幕认证成功（房间 %s）", c.RoomID)
		return
	case opMessage:
		// 解压（protover=2 zlib）
		payload := body
		if protover == protoZlib {
			decoded, err := zlibDecode(body)
			if err != nil {
				c.log("zlib 解压失败：%v", err)
				return
			}
			payload = decoded
		}
		// 解压后可能是多条 JSON 拼接（无 16B 头），逐条解析
		for _, raw := range splitJSONs(payload) {
			c.handleMessage(raw)
		}
	default:
		// op=3 心跳回复等其余类型忽略
	}
}

// handleMessage 解析单条 B站消息 JSON，命中 DANMU_MSG 则聚合
func (c *DanmakuCollector) handleMessage(raw []byte) {
	var m struct {
		Cmd  string          `json:"cmd"`
		Info json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Cmd != "DANMU_MSG" {
		return
	}
	var info []json.RawMessage
	if err := json.Unmarshal(m.Info, &info); err != nil || len(info) < 2 {
		return
	}
	// info[1] = 弹幕文本
	var text string
	if err := json.Unmarshal(info[1], &text); err != nil {
		return
	}
	if text == "" {
		return
	}
	// 密度桶（以本地时间对齐 5s；弹幕时间戳解析差异大，不用源时间）
	now := time.Now().Unix()
	c.accumulate(now)
}

// accumulate 弹幕计数入桶，跨桶时 flush
func (c *DanmakuCollector) accumulate(now int64) {
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

func (c *DanmakuCollector) flushBucket() {
	if c.lastBucket != 0 && c.curCount > 0 && c.OnBucket != nil {
		c.OnBucket(c.RoomID, c.lastBucket, c.curCount)
	}
	c.lastBucket = 0
	c.curCount = 0
}

// splitJSONs 把可能的多条 JSON 拼接拆成单条（按 { } 括号配对，简单可靠）
func splitJSONs(b []byte) [][]byte {
	var out [][]byte
	depth := 0
	start := -1
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, b[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

func zlibDecode(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, 16<<20))
}