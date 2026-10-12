package pipeline

// 双信号高光：弹幕热点计算与合并切片（M4.2 收官）单元测试

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/store"
)

// newTestEnv 造临时 DB + 模拟弹幕流量：
// 会话起点 = 文件名时间戳 20260512_120000（与 seg 路径解析一致），0-600s 平时 2-4 条/5s，
// 300-330s 爆发 30+ 条/5s；另造第二会话验证窗口隔离
func newTestEnv(t *testing.T) *Env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// 用文件名时间戳的本地 unix 值造桶（与 seg 路径解析一致）
	t0, err := time.ParseInLocation("20060102_150405", "20260512_120000", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	start := t0.Unix()
	for ts := start; ts < start+600; ts += 5 {
		cnt := 2 + int((ts/5)%3)
		if ts >= start+300 && ts < start+330 {
			cnt = 30 + int((ts/5)%10)
		}
		if err := st.AddBarrage("441195", ts, cnt); err != nil {
			t.Fatal(err)
		}
	}
	for ts := start + 10000; ts < start+10100; ts += 5 {
		if err := st.AddBarrage("441195", ts, 50); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Live.SegmentSec = 600
	return &Env{Cfg: cfg, Store: st}
}

func TestBarrageHotspots(t *testing.T) {
	env := newTestEnv(t)
	base := filepath.Join("data", "live", "441195_20260512_120000")

	// seg_000：0-600s 窗口，热点应在 300-330s
	job0 := &core.Job{}
	job0.SetParam("video", filepath.Join(base, "seg_000.flv"))
	hs, err := barrageHotspots(context.Background(), job0, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) == 0 {
		t.Fatal("应检测到热点（300-330s 弹幕高峰）")
	}
	found := false
	for _, h := range hs {
		if h.Start >= 295 && h.Start <= 305 && h.End <= 335 && h.Density >= 30 {
			found = true
		}
	}
	if !found {
		t.Fatalf("未在 300-330s 附近找到热点，实际：%+v", hs)
	}

	// seg_001：600-1200s 无弹幕 → 无热点
	job1 := &core.Job{}
	job1.SetParam("video", filepath.Join(base, "seg_001.flv"))
	hs1, _ := barrageHotspots(context.Background(), job1, env)
	if len(hs1) != 0 {
		t.Fatalf("seg_001 无弹幕应无热点，实际 %+v", hs1)
	}

	// 普通视频（非直播分段）→ 无热点
	job2 := &core.Job{}
	job2.SetParam("video", filepath.Join("data", "jobs", "x", "out.mp4"))
	hs2, _ := barrageHotspots(context.Background(), job2, env)
	if len(hs2) != 0 {
		t.Fatalf("普通视频应无热点，实际 %+v", hs2)
	}
}

func TestHotspotsToClips(t *testing.T) {
	clips := hotspotsToClips([]Hotspot{{Start: 300, End: 330, Density: 35}})
	if len(clips) != 1 {
		t.Fatal("应产出 1 个切片候选")
	}
	if clips[0].Start != 300 || clips[0].End != 330 {
		t.Fatalf("区间错误: %+v", clips[0])
	}
	if clips[0].Score <= 0 || clips[0].Title == "" {
		t.Fatalf("标题/得分缺失: %+v", clips[0])
	}
}