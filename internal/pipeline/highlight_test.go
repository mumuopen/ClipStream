package pipeline

// 双信号高光：弹幕热点计算与合并切片（M4.2 收官）单元测试

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/engine"
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

// TestBarrageHotspotsMerged 合并产物（merged_*.flv）的弹幕热点映射：
// 虎牙碎段流内容轴有重连空洞——600s 墙钟窗口仅 300s 内容，弹幕必须按 meta
// 段级映射换算，否则热点位置错位近一倍（300s 墙钟处的内容实际在 150s 处）。
func TestBarrageHotspotsMerged(t *testing.T) {
	env := newTestEnv(t)
	t0, _ := time.ParseInLocation("20060102_150405", "20260512_120000", time.Local)
	base := filepath.Join("data", "live", "441195_20260512_120000")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll("data") })
	video := filepath.Join(base, "merged_000.flv")
	if err := os.WriteFile(video, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模拟虎牙空洞：组墙钟 600s、内容仅 300s（两段各 150s 内容占 300s 墙钟）
	meta := core.MergedSegMeta{
		Start: t0.Unix(),
		End:   t0.Unix() + 600,
		Segs: []core.MergedSeg{
			{Name: "seg_000", Wall: t0.Unix() + 300, Offset: 0, Dur: 150},
			{Name: "seg_001", Wall: t0.Unix() + 600, Offset: 150, Dur: 150},
		},
	}
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(strings.TrimSuffix(video, ".flv")+".meta.json", mb, 0o644); err != nil {
		t.Fatal(err)
	}

	job := &core.Job{}
	job.SetParam("video", video)
	hs, err := barrageHotspots(context.Background(), job, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) == 0 {
		t.Fatal("应检测到热点")
	}
	// 弹幕爆发在墙钟 300-330s → 段 1 区间线性映射到内容轴 150-165s
	found := false
	for _, h := range hs {
		if h.Start >= 145 && h.Start <= 158 {
			found = true
		}
	}
	if !found {
		t.Fatalf("热点应映射到内容轴 ~150s（墙钟空洞换算），实际：%+v", hs)
	}
	// 对照：若未做映射，热点会错位在 ~300s（墙钟直减）
	for _, h := range hs {
		if h.Start > 250 {
			t.Fatalf("热点 %v 疑似未按 meta 映射（落在墙钟直减位置 ~300s）", h)
		}
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

// ---- 高光候选过滤/钩制（LLM 边界 + 8s/300s 双限 + 降级兜底前提） ----

func TestFilterClips(t *testing.T) {
	mk := func(start, end float64) engine.Clip {
		return engine.Clip{Start: start, End: end, Title: "t", Score: 80}
	}
	cases := []struct {
		name         string
		clips        []engine.Clip
		duration     float64
		fromLLM      bool
		wantKept     int
		wantRejected int
	}{
		{"LLM 正常区间", []engine.Clip{mk(10, 90)}, 300, true, 1, 0},
		{"LLM 越界裁齐后短于 15s 拒绝", []engine.Clip{mk(50, 200)}, 60, true, 0, 1},
		{"LLM 负起点归零", []engine.Clip{mk(-10, 30)}, 100, true, 1, 0},
		{"LLM 整段越界丢弃", []engine.Clip{mk(120, 200)}, 100, true, 0, 1},
		{"LLM 超 300s 上限拒绝", []engine.Clip{mk(0, 400)}, 500, true, 0, 1},
		{"下限 15s：14.9s 拒绝", []engine.Clip{mk(0, 14.9)}, 100, true, 0, 1},
		{"下限 15s：恰好 15s 保留", []engine.Clip{mk(0, 15)}, 100, true, 1, 0},
		{"非 LLM 不钩制（等分按构造）", []engine.Clip{mk(0, 500)}, 100, false, 1, 0},
		{"时长未知不钩制但守 300s", []engine.Clip{mk(0, 30), mk(0, 400)}, 0, true, 1, 1},
		{"混合：保留+拒绝各计", []engine.Clip{mk(10, 60), mk(0, 5), mk(200, 280)}, 300, true, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, rejected := filterClips(tc.clips, tc.duration, tc.fromLLM)
			if len(kept) != tc.wantKept || rejected != tc.wantRejected {
				t.Fatalf("kept=%d rejected=%d, want kept=%d rejected=%d", len(kept), rejected, tc.wantKept, tc.wantRejected)
			}
		})
	}
}

func TestFilterClipsClamp(t *testing.T) {
	// LLM 给出 [50, 200] 而视频只有 150s：应裁成 [50, 150]（100s ≥ 下限）保留
	kept, _ := filterClips([]engine.Clip{{Start: 50, End: 200, Title: "x", Score: 70}}, 150, true)
	if len(kept) != 1 {
		t.Fatalf("kept=%d, want 1", len(kept))
	}
	if kept[0].Start != 50 || kept[0].End != 150 {
		t.Fatalf("钩制结果 [%.1f, %.1f], want [50, 150]", kept[0].Start, kept[0].End)
	}
}

func TestFallbackChunksShortVideo(t *testing.T) {
	// 降级兜底前提：短视频时 fallbackChunks 产出可预期——<5s 无产出，100s 两段全覆盖
	if got := fallbackChunks(3); len(got) != 0 {
		t.Fatalf("3s 视频 fallbackChunks 产出 %d 段, want 0", len(got))
	}
	got := fallbackChunks(100)
	if len(got) != 2 {
		t.Fatalf("100s 视频 fallbackChunks 产出 %d 段, want 2", len(got))
	}
	for i, c := range got {
		if c.End-c.Start < 5 {
			t.Fatalf("第 %d 段过短: %.1f-%.1f", i+1, c.Start, c.End)
		}
	}
}