package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/store"
)

// doDelete 直调 deleteJob handler（Go 1.22 PathValue 需手动 SetPathValue）
func doDelete(srv *Server, id string) int {
	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	srv.deleteJob(rec, req)
	return rec.Code
}

// TestDeleteJobPoolLink 删任务与池条目的联动（2026-10-28 修）。
// 旧实现只要 kind==publish 就无条件把池条目打回 pending，于是"删一条已经发布
// 成功的任务"= 把同一视频再发一遍；成功后 platforms 被 trim 成空串、泵又把空串
// 当 bilibili，所以重发只打 B 站，从抖音侧根本看不出来。
func TestDeleteJobPoolLink(t *testing.T) {
	st := mkTestStore(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	srv := &Server{cfg: cfg, store: st, qrSessions: map[string]*qrSession{}}

	it := &store.PoolItem{SourceJob: "j_clips", Clip: "clips/clip_01.mp4", Title: "标题", Platforms: "bilibili"}
	pid, err := st.AddPoolItem(it)
	if err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"source_job":"j_clips","clip":"clips/clip_01.mp4","title":"标题","account":"default","platforms":["bilibili"],"pool_item_id":%d}`, pid)

	// ① 已成功的发布任务：池条目保持 dispatched（已完结），不得重发
	if err := st.CreateJob(&core.Job{ID: "j_pub_done", Kind: "publish", Status: core.StatusDone, Input: input, Dir: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolItemStatus(pid, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if code := doDelete(srv, "j_pub_done"); code != 200 {
		t.Fatalf("删除应成功，code=%d", code)
	}
	all, err := st.ListPoolItems(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != "dispatched" {
		t.Fatalf("已成功任务被删后池条目应保持 dispatched（否则=重复投稿），got %+v", all)
	}

	// ② 未完成（queued）的发布任务：删任务必须回池，否则永远卡 dispatched
	if err := st.CreateJob(&core.Job{ID: "j_pub_queued", Kind: "publish", Status: core.StatusQueued, Input: input, Dir: "x"}); err != nil {
		t.Fatal(err)
	}
	// 模拟 dispatchOne：派发时先置 dispatched
	if err := st.SetPoolItemStatus(pid, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if code := doDelete(srv, "j_pub_queued"); code != 200 {
		t.Fatalf("删除应成功，code=%d", code)
	}
	pend, err := st.ListPoolItems(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].Status != "pending" {
		t.Fatalf("未完成任务被删后池条目应回 pending，got %+v", pend)
	}

	// ③ 已失败（finalize 已善后）的任务：不得再动池条目把结论重置
	if err := st.CreateJob(&core.Job{ID: "j_pub_failed", Kind: "publish", Status: core.StatusFailed, Input: input, Dir: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolItemStatus(pid, "failed"); err != nil {
		t.Fatal(err)
	}
	if code := doDelete(srv, "j_pub_failed"); code != 200 {
		t.Fatalf("删除应成功，code=%d", code)
	}
	all2, err := st.ListPoolItems(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all2) != 1 || all2[0].Status != "failed" {
		t.Fatalf("已退池条目不应被删任务重置，got %+v", all2)
	}
}
