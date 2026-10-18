package live

import "testing"

// TestBackpressureOn 池背压滞回判定全分支：触发/维持/解除/关闭/非法水位回退
func TestBackpressureOn(t *testing.T) {
	// 关闭（high=0）：任何状态/pending 都不触发
	if BackpressureOn(true, 999, 0, 10) {
		t.Fatal("high=0 应永不触发")
	}
	// 未触发 → pending 达到 high 触发
	if !BackpressureOn(false, 20, 20, 10) {
		t.Fatal("pending==high 应触发")
	}
	if !BackpressureOn(false, 21, 20, 10) {
		t.Fatal("pending>high 应触发")
	}
	// 已触发 → 降回 high 之下但仍在 low 之上：维持（滞回防临界抖动）
	if !BackpressureOn(true, 19, 20, 10) {
		t.Fatal("触发后 pending>low 应维持")
	}
	if !BackpressureOn(true, 11, 20, 10) {
		t.Fatal("pending=11 > low=10 应维持")
	}
	// 降回 low（含）以下：解除
	if BackpressureOn(true, 10, 20, 10) {
		t.Fatal("pending<=low 应解除")
	}
	if BackpressureOn(true, 0, 20, 10) {
		t.Fatal("pending=0 应解除")
	}
	// 未触发时 pending 在 (low, high) 区间：不触发（只有越过 high 才进背压）
	if BackpressureOn(false, 15, 20, 10) {
		t.Fatal("未触发态 pending<high 不应触发")
	}
	// low 非法（0 / ≥high）回退为 high/2：等价 low=10
	if !BackpressureOn(true, 11, 20, 0) {
		t.Fatal("low=0 应回退 high/2=10 维持语义")
	}
	if BackpressureOn(true, 10, 20, 0) {
		t.Fatal("low=0 应回退 high/2=10 解除语义")
	}
	if BackpressureOn(true, 10, 20, 25) {
		t.Fatal("low>=high 应回退 high/2=10 解除语义")
	}
}
