package store

import "testing"

// TestStyleBaseline 场级风格基线生命周期：
// 建 v1 → 比对读回 → 置漂移 → 切片侧读到 drifted → rebaseline 重建 v2 清标记
func TestStyleBaseline(t *testing.T) {
	s := mkStore(t)
	// 无基线：RoomStyleState ok=false（切片侧记 skipped）
	if _, _, ok := s.RoomStyleState("r1"); ok {
		t.Fatal("无基线应 ok=false")
	}
	// 首段建基线 v1
	ver, err := s.UpsertStyleBaseline("r1", "sess-1010", `{"w":1280,"h":720}`)
	if err != nil || ver != 1 {
		t.Fatalf("首建应 v1，得到 ver=%d err=%v", ver, err)
	}
	sid, ft, v, drifted, ok := s.StyleBaseline("r1")
	if !ok || sid != "sess-1010" || v != 1 || drifted {
		t.Fatalf("基线读回不符: sid=%s v=%d drifted=%v ok=%v", sid, v, drifted, ok)
	}
	if ft != `{"w":1280,"h":720}` {
		t.Fatalf("特征 JSON 应原样读回，得到 %s", ft)
	}
	// 切片侧：未漂移放行
	if d, v, ok := s.RoomStyleState("r1"); !ok || d || v != 1 {
		t.Fatalf("未漂移场应 drifted=false，得到 %v/%d/%v", d, v, ok)
	}
	// 后续段漂移 → 标记
	if err := s.MarkStyleDrift("r1", "sess-1010", "pHash 距离 18 > 12"); err != nil {
		t.Fatal(err)
	}
	if d, _, ok := s.RoomStyleState("r1"); !ok || !d {
		t.Fatal("漂移后切片侧应读到 drifted=true（整场拦截转人工）")
	}
	// rebaseline（人工确认后）：特征重建 + v2 + 清漂移
	ver, err = s.UpsertStyleBaseline("r1", "sess-1010", `{"w":1920,"h":1080}`)
	if err != nil || ver != 2 {
		t.Fatalf("rebaseline 应 v2，得到 %d err=%v", ver, err)
	}
	_, ft2, v2, d2, ok2 := s.StyleBaseline("r1")
	if !ok2 || v2 != 2 || d2 || ft2 != `{"w":1920,"h":1080}` {
		t.Fatalf("rebaseline 后应为新特征 v2 无漂移：%s v=%d d=%v", ft2, v2, d2)
	}
	// 多房间隔离：r2 独立基线
	if _, err := s.UpsertStyleBaseline("r2", "sess-2020", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStyleDrift("r2", "sess-2020", "色调漂移"); err != nil {
		t.Fatal(err)
	}
	if d, _, ok := s.RoomStyleState("r1"); !ok || d {
		t.Fatal("r2 漂移不应影响 r1")
	}
	if d, _, ok := s.RoomStyleState("r2"); !ok || !d {
		t.Fatal("r2 自身漂移应可见")
	}
	// 同房间新会话（重播）覆盖最近性：RoomStyleState 读最新一场
	if _, err := s.UpsertStyleBaseline("r1", "sess-1030", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, _, v, _, ok := s.StyleBaseline("r1"); !ok || v != 1 || sid == "" {
		t.Fatal("新会话应新建 v1（最近性切换）")
	}
}
