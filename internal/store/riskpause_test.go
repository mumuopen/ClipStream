package store

import (
	"testing"
)

// TestAccountRiskPaused 风控冷却：置冷却→查询命中；手动恢复清终点保留计数
func TestAccountRiskPaused(t *testing.T) {
	s := mkStore(t)
	acc, err := s.CreateAccount(&Account{Platform: "bilibili", Name: "bili_test"})
	if err != nil {
		t.Fatal(err)
	}
	_ = acc

	// 初始未冷却
	if until, count := s.AccountRiskPaused("bili_test"); until != 0 || count != 0 {
		t.Fatalf("初始应未冷却，得到 until=%d count=%d", until, count)
	}

	// 第 1 次触发：冷却 24h，计数 1
	if err := s.MarkAccountRiskPaused("bili_test", 1700000000, 1); err != nil {
		t.Fatal(err)
	}
	until, count := s.AccountRiskPaused("bili_test")
	if until != 1700000000 || count != 1 {
		t.Fatalf("置冷却后应可查到，得到 until=%d count=%d", until, count)
	}

	// 手动恢复：清终点，保留计数
	if err := s.ClearAccountRiskPause("bili_test"); err != nil {
		t.Fatal(err)
	}
	until, count = s.AccountRiskPaused("bili_test")
	if until != 0 {
		t.Fatalf("手动恢复后应清终点，得到 until=%d", until)
	}
	if count != 1 {
		t.Fatalf("手动恢复应保留计数（再触发继续翻倍），得到 count=%d", count)
	}

	// 不存在的账号：零值不报错
	if until, count := s.AccountRiskPaused("nobody"); until != 0 || count != 0 {
		t.Fatalf("不存在账号应返回零值")
	}
}

// TestPurgeableJobsReferenced 非终态引用保护（0.6.2 事故修复）：字幕/配音成品
// 写在源 clips 任务目录里，审核台 review 状态的字幕任务引用的素材在源目录——
// 清理必须跳过被引用的源任务，否则审核台待审素材 404
func TestPurgeableJobsReferenced(t *testing.T) {
	s := mkStore(t)
	ins := func(id, kind, status, input string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO jobs (id,kind,status,dir,input,created_at) VALUES (?,?,?,?,?,0)`, id, kind, status, "data/jobs/"+id, input); err != nil {
			t.Fatal(err)
		}
	}
	// 源切片任务：done 且池无引用（若无引用保护则可清）
	ins("src-done", "clips", "done", `{}`)
	// 审核台里的字幕任务：review，引用 src-done 的素材目录
	ins("sub-review", "subtitle", "review", `{"source_job":"src-done","clip":"clips/a.mp4"}`)
	// 另一个无引用的终态任务：可清
	ins("jp-fail", "publish", "failed", `{}`)

	targets, err := s.PurgeableJobs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range targets {
		got[j.ID] = true
	}
	if got["src-done"] {
		t.Fatal("被审核台字幕任务引用的源任务 src-done 必须保留（素材在役）")
	}
	if !got["jp-fail"] {
		t.Fatal("无引用的终态任务 jp-fail 应可清")
	}

	// 字幕任务终态后，源任务恢复可清
	if _, err := s.db.Exec(`UPDATE jobs SET status='done' WHERE id='sub-review'`); err != nil {
		t.Fatal(err)
	}
	targets2, _ := s.PurgeableJobs()
	got2 := map[string]bool{}
	for _, j := range targets2 {
		got2[j.ID] = true
	}
	if !got2["src-done"] {
		t.Fatal("引用方终态后 src-done 应回到可清目标")
	}
	if got2["sub-review"] {
		t.Fatal("subtitle 任务不在清理 kind 范围（只清 clips/publish）")
	}

	// 在途 publish 引用同样保护（上传中删源目录=上传崩）
	if _, err := s.db.Exec(`UPDATE jobs SET status='running' WHERE id='sub-review'`); err != nil {
		t.Fatal(err)
	}
	targets3, _ := s.PurgeableJobs()
	for _, j := range targets3 {
		if j.ID == "src-done" {
			t.Fatal("在途任务引用期间 src-done 不应可清")
		}
	}
}

// TestDeleteAllPoolItems include_pending 清空待发池（风控冷却期 pending 占背压
// 水位形成录制死锁，用户拍板可一键清空）
func TestDeleteAllPoolItems(t *testing.T) {
	s := mkStore(t)
	for _, st := range []string{"pending", "dispatched", "failed"} {
		if _, err := s.AddPoolItem(&PoolItem{SourceJob: "j1", Clip: "clips/" + st + ".mp4", Title: "t", Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteAllPoolItems()
	if err != nil || n != 3 {
		t.Fatalf("应清空全部 3 条，得到 n=%d err=%v", n, err)
	}
	if pend, _ := s.CountPendingPoolItems(); pend != 0 {
		t.Fatalf("清空后 pending 应为 0，得到 %d", pend)
	}
}

// TestPurgeableJobs 一键清理口径：终态任务可清，pending 引用/非终态引用保护
func TestPurgeableJobs(t *testing.T) {
	s := mkStore(t)
	// 任务：jc1 终态（无引用→可清）；jc2 终态（pending 引用→保留）；
	// jc3 运行中（不终态→保留）；jp1 失败发布任务（终态→可清）
	ins := func(id, kind, status, dir string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO jobs (id,kind,status,dir,created_at) VALUES (?,?,?,?,0)`, id, kind, status, dir); err != nil {
			t.Fatal(err)
		}
	}
	ins("jc1", "clips", "done", "data/jobs/jc1")
	ins("jc2", "clips", "done", "data/jobs/jc2")
	ins("jc3", "clips", "running", "data/jobs/jc3")
	ins("jp1", "publish", "failed", "data/jobs/jp1")

	// 池条目：jc2 有 pending（引用其素材）；jc1 只有 dispatched（将被清理，不算活跃）
	if _, err := s.AddPoolItem(&PoolItem{SourceJob: "jc2", Clip: "clips/a.mp4", Title: "t", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPoolItem(&PoolItem{SourceJob: "jc1", Clip: "clips/b.mp4", Title: "t", Status: "dispatched"}); err != nil {
		t.Fatal(err)
	}

	targets, err := s.PurgeableJobs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range targets {
		got[j.ID] = true
	}
	if !got["jc1"] || !got["jp1"] {
		t.Fatalf("终态无引用任务应可清（jc1/jp1），得到 %v", got)
	}
	if got["jc2"] {
		t.Fatal("pending 池条目引用的任务 jc2 必须保留（素材还在待发）")
	}
	if got["jc3"] {
		t.Fatal("运行中任务 jc3 不应出现在清理目标")
	}

	// 已完结池条目统计与删除
	if n, _ := s.CountFinishedPoolItems(); n != 1 {
		t.Fatalf("已完结池条目应 1 条（dispatched），得到 %d", n)
	}
	deleted, err := s.DeleteFinishedPoolItems()
	if err != nil || deleted != 1 {
		t.Fatalf("应删除 1 条已完结池条目，得到 %d err=%v", deleted, err)
	}
	// 清理池后 jc1 的 dispatched 引用消失——PurgeableJobs 口径不变（只看 pending）
	if n, _ := s.CountFinishedPoolItems(); n != 0 {
		t.Fatalf("清空后应 0 条，得到 %d", n)
	}
}
