package pipeline

import (
	"testing"

	"chopperbot/internal/engine"
)

// TestSRTTime 秒 → SRT 时间戳格式
func TestSRTTime(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "00:00:00,000"},
		{1.5, "00:00:01,500"},
		{62.345, "00:01:02,345"},
		{3661.999, "01:01:01,999"},
		{-3, "00:00:00,000"}, // 负数钳 0
	}
	for _, c := range cases {
		if got := srtTime(c.in); got != c.want {
			t.Errorf("srtTime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildSRT 双语与纯中文两种样式
func TestBuildSRT(t *testing.T) {
	trans := []engine.Segment{{Start: 0, End: 2.5, Text: "确认着陆。"}}
	origJSON := `[{"start":0,"end":2.5,"text":"Touchdown confirmed."}]`

	// 纯中文
	got := buildSRT(trans, "zh", origJSON)
	want := "1\n00:00:00,000 --> 00:00:02,500\n确认着陆。\n\n"
	if got != want {
		t.Errorf("zh 样式:\n got: %q\nwant: %q", got, want)
	}

	// 双语（上原文下中文）
	got = buildSRT(trans, "bilingual", origJSON)
	want = "1\n00:00:00,000 --> 00:00:02,500\nTouchdown confirmed.\n确认着陆。\n\n"
	if got != want {
		t.Errorf("bilingual 样式:\n got: %q\nwant: %q", got, want)
	}
}

// TestClamp 边界钳制
func TestClamp(t *testing.T) {
	if clamp(-1, 0, 10) != 0 {
		t.Error("下界")
	}
	if clamp(11, 0, 10) != 10 {
		t.Error("上界")
	}
	if clamp(5, 0, 10) != 5 {
		t.Error("范围内应原样")
	}
}
