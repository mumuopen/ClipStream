package live

import (
	"encoding/binary"
	"testing"
)

// TestDanmakuMalformedHeaderNoPanic 畸形弹幕帧不得 panic（2026-10-28 修）。
// headerLen 来自服务端下发的 uint16（可达 65535），旧实现直接 pkt[headerLen:]
// → slice bounds panic；而弹幕采集跑在 daemon 的 goroutine 里、Start 没有
// recover，单帧就能打崩整个录制守护，所有并发场次一起断。
func TestDanmakuMalformedHeaderNoPanic(t *testing.T) {
	c := NewDanmakuCollector("6")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("畸形帧不得 panic（会打崩整个 daemon）：%v", r)
		}
	}()

	// ① headerLen 超出包长（65535 > 16）
	big := make([]byte, 16)
	binary.BigEndian.PutUint16(big[4:6], 65535)
	c.handlePacket(big)

	// ② headerLen 小于协议头（0）——旧实现会让后半段解析读到 header 字节
	zero := make([]byte, 16)
	binary.BigEndian.PutUint16(zero[4:6], 0)
	c.handlePacket(zero)

	// ③ 正常包仍应被接受（回归：防御不能把合法帧也拒掉）
	ok := make([]byte, 16)
	binary.BigEndian.PutUint16(ok[4:6], 16)
	binary.BigEndian.PutUint16(ok[6:8], 1)
	binary.BigEndian.PutUint32(ok[8:12], uint32(opHeartbeatRpl))
	c.handlePacket(ok)
	if c.lastBeat.Load() == 0 {
		t.Fatal("合法心跳包应被处理（lastBeat 更新）")
	}
}
