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

// TestCleanupProtectsQueuedSession 已送切但仍在队列里等待消费的会话目录不得被删
// （2026-10-28 修）。scanClipQueue 是 10s 一条串行消费，一次下播送切 35 段要
// 6 分钟；期间若磁盘低于水位，水位清理会把排在最前面的那个目录 RemoveAll，
// 管线 Cut 就地失败（切片文件不存在）。
func TestCleanupProtectsQueuedSession(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Retention.SegmentsDays = 1
	cfg.Retention.MinFreePercent = 1 // 别让水位清理干扰本用例
	cfg.Retention.MaxUsagePercent = 0

	// 超期会话目录（名字里的时间戳决定超期判定）
	name := "6_" + time.Now().Add(-30*24*time.Hour).Format("20060102_150405")
	segDir := filepath.Join(dir, "live", name)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(segDir, "seg_000.flv")
	if err := os.WriteFile(seg, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// 同级的另一个超期会话（无队列引用）→ 应被清理，作为对照
	otherName := "7_" + time.Now().Add(-30*24*time.Hour).Format("20060102_150405")
	otherDir := filepath.Join(dir, "live", otherName)
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(otherDir, "seg_000.flv"), make([]byte, 1024), 0o644)

	// 队列里登记 segDir 的分段（serve 尚未消费）
	queueDir := filepath.Join(dir, "live", "queue")
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queueDir, "clip-1.seg"), []byte(seg+"\t标题\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := RunCleanup(cfg, st, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(segDir); err != nil {
		t.Fatalf("队列引用中的会话目录不得被删：%v", err)
	}
	if _, err := os.Stat(otherDir); !os.IsNotExist(err) {
		t.Fatalf("无引用的超期会话应被清理")
	}
}

// TestCleanupProtectsPendingClip 背压期已登记待补送的会话同样不得被当空壳删
func TestCleanupProtectsPendingClip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Retention.SegmentsDays = 1
	cfg.Retention.MinFreePercent = 1
	cfg.Retention.MaxUsagePercent = 0

	name := "8_" + time.Now().Add(-30*24*time.Hour).Format("20060102_150405")
	segDir := filepath.Join(dir, "live", name)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(segDir, "seg_000.flv"), make([]byte, 1024), 0o644)

	// 写待送切登记
	reg := fmt.Sprintf(`[{"seg_dir":%q,"title":"标题","room_id":"8","at":%d}]`, segDir, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(dir, "live", "pending_clips.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := RunCleanup(cfg, st, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(segDir); err != nil {
		t.Fatalf("待送切登记的会话目录不得被删：%v", err)
	}
}

// TestCleanupConsumedSegDays 已送切（.queued）的会话按 consumed_seg_days 更积极清理：
// config 里这个字段一直有注释却从未被引用，挂机时源分段会占满 segments_days。
func TestCleanupConsumedSegDays(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Retention.SegmentsDays = 7
	cfg.Retention.ConsumedSegDays = 1
	cfg.Retention.MinFreePercent = 1
	cfg.Retention.MaxUsagePercent = 0

	// 3 天前、已送切 → consumed_seg_days=1 应清
	name := "9_" + time.Now().Add(-3*24*time.Hour).Format("20060102_150405")
	segDir := filepath.Join(dir, "live", name)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(segDir, "seg_000.flv"), make([]byte, 1024), 0o644)
	_ = os.WriteFile(filepath.Join(segDir, ".queued"), []byte("x"), 0o644)

	// 3 天前、未送切 → 仍在 segments_days=7 内，应留
	name2 := "10_" + time.Now().Add(-3*24*time.Hour).Format("20060102_150405")
	segDir2 := filepath.Join(dir, "live", name2)
	if err := os.MkdirAll(segDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(segDir2, "seg_000.flv"), make([]byte, 1024), 0o644)

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := RunCleanup(cfg, st, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Fatal("已送切且超过 consumed_seg_days 的会话应被清理")
	}
	if _, err := os.Stat(segDir2); err != nil {
		t.Fatalf("未送切的会话仍在 segments_days 内，不得删除：%v", err)
	}
}
