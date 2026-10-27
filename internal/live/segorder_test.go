package live

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestListSegFilesNumericOrder 段序必须按段号数字排（2026-10-28 修）。
// filepath.Glob 返回字典序，seg_%03d.flv 到第 1000 段起会错序
// （"seg_1000" < "seg_999"，第 5 字节 '1'<'9'）；虎牙 5-15s 短连接下
// 挂机约 1.5 小时即破 1000 段，mergeAndQueue 按此顺序拼接会得到时间轴
// 错乱的产物——FLV 合法、tag 数正确，只有画面顺序是错的。
func TestListSegFilesNumericOrder(t *testing.T) {
	dir := t.TempDir()
	// 刻意乱序创建，且跨越 999/1000 边界
	for _, n := range []int{1001, 999, 1000, 2, 10, 1} {
		p := filepath.Join(dir, fmt.Sprintf("seg_%03d.flv", n))
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(65 << 10); err != nil { // > 64KB 阈值
			f.Close()
			t.Fatal(err)
		}
		f.Close()
	}
	// 低于阈值的段（空文件/写一半）必须被过滤，不参与排序
	if err := os.WriteFile(filepath.Join(dir, "seg_1002.flv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listSegFiles(dir)
	var idx []int
	for _, p := range got {
		idx = append(idx, segIndex(p))
	}
	want := []int{1, 2, 10, 999, 1000, 1001}
	if len(idx) != len(want) {
		t.Fatalf("有效段数 %d（%v），want %d", len(idx), idx, len(want))
	}
	for i := range want {
		if idx[i] != want[i] {
			t.Fatalf("段序 %v，want %v", idx, want)
		}
	}
}
