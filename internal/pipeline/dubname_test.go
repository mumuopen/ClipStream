package pipeline

import "testing"

// TestDubOutNameNoCollision 原片与字幕版的配音产物不得同名（2026-10-28 修）。
// 旧版把 clip_06.mp4 和 clip_06_sub.mp4 都映射成 clip_06_dub.mp4——
// 同一场直播先给原片配音、再给字幕版配音时，第二次静默覆盖第一次。
func TestDubOutNameNoCollision(t *testing.T) {
	cases := map[string]string{
		"clip_06.mp4":     "clip_06_dub.mp4",
		"clip_06_sub.mp4": "clip_06_sub_dub.mp4",
		"clip_01.mkv":     "clip_01_dub.mkv",
		"clip_02":         "clip_02_dub.mp4",
	}
	seen := map[string]string{}
	for in, want := range cases {
		got := dubOutName(in)
		if got != want {
			t.Errorf("dubOutName(%q) = %q, want %q", in, got, want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q 与 %q 的配音产物冲突（同名 %q）", prev, in, got)
		}
		seen[got] = in
	}
}
