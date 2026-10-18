package live

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/store"
)

// TestDropOldest 最旧优先删除 + stop 条件达标即停（水位/占用上限共用语义）
func TestDropOldest(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) cleanupSession {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(p, "seg_0001.flv"), make([]byte, 1024), 0o644)
		return cleanupSession{path: p, ts: time.Now().Add(-age), size: 1024}
	}
	// 最旧在前排序（与 RunCleanup 一致）
	sessions := []cleanupSession{mk("a", 3*time.Hour), mk("b", 2*time.Hour), mk("c", 1*time.Hour)}

	// 场景一：删一个即满足 stop → 只删最旧的 a
	deleted, freed := dropOldest(sessions, func() bool { return false })
	// stop 恒 false：删光
	if deleted != 3 || freed != 3072 {
		t.Fatalf("恒不满足应删光：deleted=%d freed=%d", deleted, freed)
	}
	for _, n := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("%s 应被删除", n)
		}
	}

	// 场景二：stop 立即为真 → 一个都不删
	sessions2 := []cleanupSession{mk("d", 3*time.Hour)}
	if d, _ := dropOldest(sessions2, func() bool { return true }); d != 0 {
		t.Fatalf("stop 即真时不应删除，deleted=%d", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); err != nil {
		t.Fatalf("d 不应被删除：%v", err)
	}

	// 场景三：删掉第一个后满足（模拟水位回升）
	sessions3 := []cleanupSession{mk("e", 3*time.Hour), mk("f", 1*time.Hour)}
	n := 0
	d, _ := dropOldest(sessions3, func() bool { n++; return n > 1 })
	// 检查 e 时 n=1 不满足 → 删 e；检查 f 时 n=2 满足 → 停
	if d != 1 {
		t.Fatalf("删 1 个即满足应停止，deleted=%d", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "f")); err != nil {
		t.Fatalf("f 不应被删除：%v", err)
	}
}

// TestRunCleanupRecordingProtect 录制中目录白名单：水位/超期清理绝不碰
// watches.status=recording 的 seg_dir（Windows 写入中强删=录制报废）
func TestRunCleanupRecordingProtect(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	liveDir := filepath.Join(dataDir, "live")
	for _, name := range []string{"100_20200101_000000", "200_20200102_000000"} {
		p := filepath.Join(liveDir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(p, "seg_0001.flv"), make([]byte, 2048), 0o644)
	}

	// 造库：房间 100 正在录制（seg_dir 指向 30 天前的旧目录）
	dbPath := filepath.Join(root, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	recDir := filepath.Join(liveDir, "100_20200101_000000")
	if _, err := st.AddWatch(&store.Watch{
		Platform: "bilibili", URL: "https://live.bilibili.com/100", RoomID: "100",
		Status: "recording", SegDir: recDir,
	}); err != nil {
		t.Fatal(err)
	}
	if ws, e := st.ListWatches(); e != nil || len(ws) != 1 {
		t.Fatalf("ListWatches: %v len=%d", e, len(ws))
	} else {
		t.Logf("watch: status=%q segDir=%q", ws[0].Status, ws[0].SegDir)
	}

	cfg := &config.Config{DataDir: dataDir,
		Retention: config.Retention{Enabled: true, SegmentsDays: 7, ConsumedSegDays: 1, MinFreePercent: 10, CheckMinutes: 60}}
	stats, err := RunCleanup(cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 录制中的 100 保留（哪怕 30 天前），非录制的 200 超期删除
	if _, err := os.Stat(recDir); err != nil {
		t.Fatalf("录制中目录不应被清理：%v", err)
	}
	if _, err := os.Stat(filepath.Join(liveDir, "200_20200102_000000")); !os.IsNotExist(err) {
		t.Fatal("超期非录制目录应被清理")
	}
	if stats.DeletedSessions != 1 {
		t.Fatalf("应只删除 1 个超期非录制目录，got %d", stats.DeletedSessions)
	}
}
