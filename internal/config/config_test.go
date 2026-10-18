package config

import (
	"testing"
	"time"
)

// TestParseActiveHours 窗口串解析：空=全天、正常窗口、跨夜、非法格式全分区
func TestParseActiveHours(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		zeroWin bool // start/end 为零值（空窗口）
	}{
		{"", true, true},
		{"   ", true, true},
		{"10:00-23:00", true, false},
		{"9:5-22:30", true, false},   // 允许 H:M 短格式
		{"22:00-06:00", true, false}, // 跨夜
		{"10:00", false, false},
		{"10:00-", false, false},
		{"-23:00", false, false},
		{"25:00-23:00", false, false},
		{"10:99-23:00", false, false},
		{"10:00-10:00", false, false}, // 起止相同=无效窗口
		{"abc-23:00", false, false},
	}
	for _, c := range cases {
		start, end, ok := ParseActiveHours(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseActiveHours(%q) ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if !c.wantOK {
			continue
		}
		if c.zeroWin && (!start.IsZero() || !end.IsZero()) {
			t.Errorf("ParseActiveHours(%q) 空窗口应返回零值时刻", c.in)
		}
	}
}

// TestInActiveHours 窗口命中的全部语义分支（含跨夜与防御默认）
func TestInActiveHours(t *testing.T) {
	mk := func(h, m int) time.Time {
		n := time.Now()
		return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, time.Local)
	}
	// 正常窗口 10:00-23:00
	if !InActiveHours("10:00-23:00", mk(10, 0)) {
		t.Error("窗口起点应命中（闭区间起点）")
	}
	if InActiveHours("10:00-23:00", mk(9, 59)) {
		t.Error("窗口前 1 分钟不应命中")
	}
	if !InActiveHours("10:00-23:00", mk(22, 59)) {
		t.Error("窗口末尾内应命中")
	}
	if InActiveHours("10:00-23:00", mk(23, 0)) {
		t.Error("窗口终点不应命中（开区间终点）")
	}
	// 跨夜窗口 22:00-06:00：深夜与清晨在窗内，白天在窗外
	if !InActiveHours("22:00-06:00", mk(23, 30)) {
		t.Error("跨夜窗口：深夜应命中")
	}
	if !InActiveHours("22:00-06:00", mk(5, 0)) {
		t.Error("跨夜窗口：清晨应命中")
	}
	if InActiveHours("22:00-06:00", mk(12, 0)) {
		t.Error("跨夜窗口：中午不应命中")
	}
	// 空窗口 = 全天；非法窗口防御默认放行（不能因配置错误锁死全部发布）
	if !InActiveHours("", mk(3, 0)) {
		t.Error("空窗口应全天可发")
	}
	if !InActiveHours("garbage", mk(3, 0)) {
		t.Error("非法窗口防御默认应放行（不锁死发布）")
	}
}
