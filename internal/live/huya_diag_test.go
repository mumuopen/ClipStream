package live

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 真网诊断：持续拉虎牙流 70 秒，统计 EOF 断流次数。
// 用途：区分「虎牙流本身每几秒断」（平台特性）与「daemon 逻辑 5 秒切段」（我们的 bug）。
func TestHuyaStreamEndurance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Skip("手动诊断用：需要时去掉本行")
	h, err := GetHuyaRoomInfo(ctx, "998")
	if err != nil {
		t.Fatal(err)
	}
	urls, err := HuyaPlayURLs(h)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("stream: %s, urls: %d\n", h.StreamName, len(urls))

	dir := t.TempDir()
	conns := 0
	var total int64
	start := time.Now()
	for time.Since(start) < 70*time.Second {
		conns++
		_, nb, serr := StreamFLV(ctx, urls[0], dir, 600, conns, nil)
		total += nb
		if nb > 0 {
			fmt.Printf("  conn #%d: %.2f MB, err=%v\n", conns, float64(nb)/1e6, serr)
		} else {
			fmt.Printf("  conn #%d: 0 bytes, err=%v\n", conns, serr)
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	verdict := "OK: single conn survives (daemon logic fine, short segs have another cause)"
	if conns > 10 {
		verdict = "FAIL: stream self-breaks frequently (Huya CDN trait, need merge)"
	}
	fmt.Printf("70s: %d conns, %.2f MB total -- %s\n", conns, float64(total)/1e6, verdict)
}
