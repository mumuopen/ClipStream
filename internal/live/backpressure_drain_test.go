package live

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/store"
)

// TestPendingClipDrain 背压期下播的会话必须能补送（2026-10-28 修）。
// 旧版池满时 endSession 只推一条“发布追平后自动恢复”的通知就 return，
// 分段既没送切也没人补送，最终被 segments_days 整目录删掉。
func TestPendingClipDrain(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Live.AutoClip = true
	cfg.Live.PoolHigh = 0 // 关闭背压 → backpressureActive 为 false，drain 可执行
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := NewDaemon(cfg, st, nil)

	segDir := filepath.Join(dir, "live", "6_20260101_000000")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 两个有效段（>64KB 才算有效）
	for _, n := range []int{0, 1} {
		p := filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", n))
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(65 << 10); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	d.addPendingClip(segDir, "测试标题", "6")
	d.addPendingClip(segDir, "测试标题", "6") // 同目录去重
	if got := d.loadPendingClips(); len(got) != 1 {
		t.Fatalf("登记应去重为 1 条，got %d", len(got))
	}
	if !PendingClipDirs(dir)[filepath.Clean(segDir)] {
		t.Fatal("PendingClipDirs 应包含登记目录（磁盘治理保护依赖它）")
	}

	d.drainPendingClips()

	entries, err := os.ReadDir(d.QueueDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("补送应产生 2 个队列条目，got %d", len(entries))
	}
	if got := d.loadPendingClips(); len(got) != 0 {
		t.Fatalf("补送成功后登记应清空，got %d", len(got))
	}
	if _, err := os.Stat(queuedMarker(segDir)); err != nil {
		t.Fatal("补送成功应写 .queued 标记")
	}

	// 再跑一次：不得重复送切（同一分段生成两个 clips 任务）
	d.addPendingClip(segDir, "测试标题", "6")
	d.drainPendingClips()
	entries2, _ := os.ReadDir(d.QueueDir())
	if len(entries2) != 2 {
		t.Fatalf("已送切的目录不应再送一次，队列条目 %d", len(entries2))
	}
}

// TestSegDirStaleIgnoresEmptySegs 空段不得刷新“素材停滞”计时（2026-10-28 修）。
// StreamFLV 在数据到达前就 os.Create 段文件；“200 但 body 立即 EOF”的冷流下
// 每次重连都新建空段并刷新目录 mtime，旧判定（含目录 mtime）永远不触发
// max_stale_sec，会话永远 recording。
func TestSegDirStaleIgnoresEmptySegs(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "seg_000.flv")
	f, err := os.Create(empty)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(empty, old, old)
	// 目录 mtime 保持新鲜（新建文件时就会这样）
	_ = os.Chtimes(dir, time.Now(), time.Now())

	since := time.Now().Add(-20 * time.Minute)
	if got := segDirStale(dir, since); got < 10*time.Minute {
		t.Fatalf("只有空段时应判定停滞（用会话开始时刻兜底），got %v", got)
	}

	// 有内容的段 → 视为素材在流动
	p := filepath.Join(dir, "seg_001.flv")
	f2, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if got := segDirStale(dir, since); got > time.Minute {
		t.Fatalf("有内容的段应视为新鲜，got %v", got)
	}

	// since 为零值且无有效段：不判停滞（0）
	if got := segDirStale(t.TempDir(), time.Time{}); got != 0 {
		t.Fatalf("无基线时应返回 0，got %v", got)
	}
}
