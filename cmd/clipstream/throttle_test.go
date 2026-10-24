package main

import (
	"path/filepath"
	"strings"
	"testing"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/store"
)

// mkStore main 包测试库（临时文件库 + Windows TempDir 句柄清理）
func mkStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// addPubJob 直插一条发布任务行（status/input 直控；绕过 newJob 的目录副作用）
func addPubJob(t *testing.T, st *store.Store, id, status, account string, dryRun bool) {
	t.Helper()
	input := `{"account":"` + account + `"}`
	if dryRun {
		input = `{"account":"` + account + `","dry_run":true}`
	}
	if err := st.CreateJob(&core.Job{ID: id, Kind: "publish", Status: core.JobStatus(status), Input: input}); err != nil {
		t.Fatal(err)
	}
}

// TestThrottleInflight 在途闸：running 达上限拦、未达放行、0=不限；done/他人账号/dry-run 不占坑
func TestThrottleInflight(t *testing.T) {
	st := mkStore(t)
	addPubJob(t, st, "r1", "running", "alpha", false)
	addPubJob(t, st, "r2", "running", "alpha", false)
	addPubJob(t, st, "d1", "done", "alpha", false)   // 已完成不占坑
	addPubJob(t, st, "r3", "running", "beta", false) // 别的账号不占坑
	addPubJob(t, st, "r4", "running", "alpha", true) // dry-run 不占坑

	rl := config.RateLimit{MinIntervalMin: 0, DailyMax: 0, MaxInflight: 2}
	if why := publishThrottled(st, rl, "alpha"); !strings.Contains(why, "在途") {
		t.Fatalf("2 running 已达上限 2 应被拦，got %q", why)
	}
	rl.MaxInflight = 3
	if why := publishThrottled(st, rl, "alpha"); why != "" {
		t.Fatalf("2 running < 3 应放行，got %q", why)
	}
	rl.MaxInflight = 0
	if why := publishThrottled(st, rl, "alpha"); why != "" {
		t.Fatalf("0=不限应放行，got %q", why)
	}
	rl.MaxInflight = 1
	if why := publishThrottled(st, rl, "beta"); !strings.Contains(why, "在途") {
		t.Fatalf("beta 1 running = 上限 1 应被拦，got %q", why)
	}
}

// TestCountRunningPublish SQL 口径：按账号计数、全局计数、dry-run/非 running 排除
func TestCountRunningPublish(t *testing.T) {
	st := mkStore(t)
	addPubJob(t, st, "r1", "running", "alpha", false)
	addPubJob(t, st, "r2", "running", "alpha", false)
	addPubJob(t, st, "q1", "queued", "alpha", false) // 排队中不算在途
	addPubJob(t, st, "r3", "running", "beta", false)
	addPubJob(t, st, "r4", "running", "alpha", true) // dry-run 不算

	if n, err := st.CountRunningPublish("alpha"); err != nil || n != 2 {
		t.Fatalf("alpha 在途应 2，got n=%d err=%v", n, err)
	}
	if n, err := st.CountRunningPublish("beta"); err != nil || n != 1 {
		t.Fatalf("beta 在途应 1，got n=%d err=%v", n, err)
	}
	if n, err := st.CountRunningPublish(""); err != nil || n != 3 {
		t.Fatalf("全局在途应 3，got n=%d err=%v", n, err)
	}
}
