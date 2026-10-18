package store

import (
	"testing"
	"time"
)

// TestWatchBindRoundtrip 直播↔账号绑定链路：设置 → 按房间查询（dispatchOne
// 路由依据）→ daemon 常规回写不冲掉绑定 → 解绑
func TestWatchBindRoundtrip(t *testing.T) {
	s := mkStore(t)
	w := &Watch{Platform: "bilibili", URL: "https://live.bilibili.com/6", RoomID: "6", Title: "测试房"}
	id, err := s.AddWatch(w)
	if err != nil {
		t.Fatal(err)
	}

	// 未见绑定时为空串（走标签路由）
	if got := s.WatchBindAccount("6"); got != "" {
		t.Fatalf("未绑定房间应返回空串，got %q", got)
	}

	// 绑定 + 回读
	if err := s.SetWatchBind(id, "电竞主号"); err != nil {
		t.Fatal(err)
	}
	if got := s.WatchBindAccount("6"); got != "电竞主号" {
		t.Fatalf("绑定的账号名 %q，want 电竞主号", got)
	}

	// daemon 周期性 UpdateWatch（回写录播状态）不能冲掉绑定：
	// w 从 DB 读出带 BindAccount，全字段写回无损
	got, err := s.WatchByRoomID("6")
	if err != nil {
		t.Fatal(err)
	}
	got.Status = "recording"
	got.SegCount = 3
	if err := s.UpdateWatch(got); err != nil {
		t.Fatal(err)
	}
	if again := s.WatchBindAccount("6"); again != "电竞主号" {
		t.Fatalf("UpdateWatch 冲掉了绑定：%q", again)
	}

	// 解绑（空串）→ 回到标签路由
	if err := s.SetWatchBind(id, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.WatchBindAccount("6"); got != "" {
		t.Fatalf("解绑后应返回空串，got %q", got)
	}

	// 不存在的房间/空房间号：安全返回空串
	if got := s.WatchBindAccount("999"); got != "" {
		t.Fatalf("不存在的房间应返回空串，got %q", got)
	}
	if got := s.WatchBindAccount(""); got != "" {
		t.Fatalf("空房间号应返回空串，got %q", got)
	}
}

// TestPoolItemRoomIDRoundtrip 池条目 room_id 落库与回读（绑定路由的传递链）
func TestPoolItemRoomIDRoundtrip(t *testing.T) {
	s := mkStore(t)
	it := &PoolItem{SourceJob: "j1", Clip: "clips/clip_01.mp4", Title: "高燃团战",
		Tags: "游戏,高燃", Platforms: "bilibili", RoomID: "6"}
	id, err := s.AddPoolItem(it)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListPoolItems(false)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListPoolItems: %v items=%d", err, len(items))
	}
	if items[0].RoomID != "6" {
		t.Fatalf("room_id 落库回读错误：%q want 6", items[0].RoomID)
	}
	_ = id
}

// TestPoolItemActiveCount 成品清理前置闩：pending/dispatched 计为活跃（源任务
// 目录不能删），failed/无条目可清
func TestPoolItemActiveCount(t *testing.T) {
	s := mkStore(t)
	mk := func(clip, status string, id int64) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO content_pool (source_job,clip,title,status,room_id,created_at) VALUES (?,?,?,?,?,?)`,
			"j1", clip, "t", status, "6", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		_ = id
	}
	if n := s.PoolItemActiveCount("j1"); n != 0 {
		t.Fatalf("无条目应为 0，got %d", n)
	}
	mk("clips/a.mp4", "failed", 0)
	if n := s.PoolItemActiveCount("j1"); n != 0 {
		t.Fatalf("仅 failed 条目应为 0，got %d", n)
	}
	mk("clips/b.mp4", "pending", 0)
	mk("clips/c.mp4", "dispatched", 0)
	if n := s.PoolItemActiveCount("j1"); n != 2 {
		t.Fatalf("pending+dispatched 应为 2，got %d", n)
	}

	// 终态任务查询（成品清理候选）：done/failed 且 updated_at 过期
	addJob := func(id, kind, status string, updated int64) {
		if _, err := s.db.Exec(`INSERT INTO jobs (id,kind,status,updated_at,created_at) VALUES (?,?,?,?,?)`,
			id, kind, status, updated, updated); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	addJob("jOld1", "clips", "done", now-10*86400)
	addJob("jOld2", "publish", "done", now-10*86400)
	addJob("jNew", "clips", "done", now-60)
	addJob("jRun", "clips", "running", now-10*86400) // 终态才算成品
	old, err := s.ListArtifactJobsBefore(now - 3*86400)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range old {
		got[j.ID] = true
	}
	if !got["jOld1"] || !got["jOld2"] {
		t.Fatalf("超期终态任务应入选：%v", got)
	}
	if got["jNew"] || got["jRun"] {
		t.Fatalf("新任务/非终态不应入选：%v", got)
	}
}
