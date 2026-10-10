package pipeline

import (
	"math"
	"testing"
)

// TestFallbackChunksCoverage 等分切片必须覆盖全时长（修复点：n 封顶后尾部丢失）
func TestFallbackChunksCoverage(t *testing.T) {
	cases := []struct {
		name     string
		dur      float64
		wantN    int
		wantLast float64 // 最后一段的 end 应等于视频时长
	}{
		{"150s", 150, 3, 150},        // ceil(150/60)=3 段
		{"600s", 600, 10, 600},       // 整除：10 段 × 60s
		{"7200s 超长", 7200, 20, 7200}, // n 封顶 20，重均分 360s/段
		{"30s 短片", 30, 1, 30},
		{"10s 极短", 10, 1, 10},
	}
	for _, c := range cases {
		got := fallbackChunks(c.dur)
		if len(got) != c.wantN {
			t.Fatalf("%s: 期望 %d 段，实际 %d 段", c.name, c.wantN, len(got))
		}
		if got[0].Start != 0 {
			t.Errorf("%s: 首段 start=%.1f 应为 0", c.name, got[0].Start)
		}
		if last := got[len(got)-1]; math.Abs(last.End-c.wantLast) > 0.11 {
			t.Errorf("%s: 尾段 end=%.1f 未覆盖全时长 %.0f", c.name, last.End, c.wantLast)
		}
		// 相邻段衔接：无重叠无缝隙
		for i := 1; i < len(got); i++ {
			if got[i].Start < got[i-1].End-0.11 {
				t.Errorf("%s: 第 %d 段与上一段重叠", c.name, i+1)
			}
		}
	}
}

// TestFallbackChunksInvalid 非法时长应返回空
func TestFallbackChunksInvalid(t *testing.T) {
	if got := fallbackChunks(0); got != nil {
		t.Errorf("dur=0 应返回 nil，实际 %v", got)
	}
	if got := fallbackChunks(-5); got != nil {
		t.Errorf("dur=-5 应返回 nil，实际 %v", got)
	}
}

// TestTailString tail 截断辅助
func TestTailString(t *testing.T) {
	if tail("short", 10) != "short" {
		t.Errorf("短字符串不应截断")
	}
	if got := tail("0123456789ABCDEF", 4); got != "...CDEF" {
		t.Errorf("长字符串应保留尾部，实际 %q", got)
	}
}
