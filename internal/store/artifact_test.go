package store

import (
	"testing"

	"chopperbot/internal/core"
)

// mkJob 建一条最小任务行（成品退役判据测试用）
func mkJob(t *testing.T, s *Store, id, kind, status string) {
	t.Helper()
	j := &core.Job{ID: id, Kind: kind, Status: core.JobStatus(status), Dir: "data/jobs/" + id}
	if err := s.CreateJob(j); err != nil {
		t.Fatal(err)
	}
}

// mkRefJob 建一条引用 source_job 的任务（字幕/配音/发布都用 input.source_job 指源目录）
func mkRefJob(t *testing.T, s *Store, id, kind, sourceJob, status string) {
	t.Helper()
	j := &core.Job{
		ID:     id,
		Kind:   kind,
		Status: core.JobStatus(status),
		Input:  `{"source_job":"` + sourceJob + `","clip":"clips/clip_01.mp4"}`,
		Dir:    "data/jobs/" + id,
	}
	if err := s.CreateJob(j); err != nil {
		t.Fatal(err)
	}
}

// TestJobArtifactInUse 成品退役判据（2026-10-28 修正）：
// dispatched 不再算「素材在役」——否则发布成功的源目录永远清不掉
// （data/jobs 只进不出 → 挂机数周磁盘撑满 → 录制静默停摆）。
// 但 pending 待发、非终态任务（审核台字幕/配音、在途发布）的引用仍必须保护。
func TestJobArtifactInUse(t *testing.T) {
	s := mkStore(t)
	const src = "j_clips_1"
	mkJob(t, s, src, "clips", "done")

	// ① 无任何引用：可退役
	if s.JobArtifactInUse(src) {
		t.Fatal("无引用时应判定为可清理")
	}

	it := &PoolItem{SourceJob: src, Clip: "clips/clip_01.mp4", Title: "标题", Platforms: "bilibili"}
	id, err := s.AddPoolItem(it)
	if err != nil {
		t.Fatal(err)
	}

	// ② 已发布（dispatched）：素材是否在途要看活的发布任务，不由池状态决定 → 可退役
	if err := s.SetPoolItemStatus(id, "dispatched"); err != nil {
		t.Fatal(err)
	}
	if s.JobArtifactInUse(src) {
		t.Fatal("dispatched 条目不应再阻止成品退役（旧 PoolItemActiveCount 口径的堵点）")
	}

	// ③ 待发（pending）：派发直接读源目录 mp4 → 必须保留
	if err := s.SetPoolItemStatus(id, "pending"); err != nil {
		t.Fatal(err)
	}
	if !s.JobArtifactInUse(src) {
		t.Fatal("pending 条目引用源目录时必须保留")
	}

	// ④ 非终态任务引用（审核台待审字幕）：成品写在源 clips 目录 → 必须保留
	if err := s.SetPoolItemStatus(id, "dispatched"); err != nil {
		t.Fatal(err)
	}
	mkRefJob(t, s, "j_sub_1", "subtitle", src, "review")
	if !s.JobArtifactInUse(src) {
		t.Fatal("review 中的字幕任务引用源目录时必须保留")
	}

	// ⑤ 引用方转为终态 → 恢复可清
	sub, err := s.GetJob("j_sub_1")
	if err != nil {
		t.Fatal(err)
	}
	sub.Status = core.StatusDone
	if err := s.UpdateJob(sub); err != nil {
		t.Fatal(err)
	}
	if s.JobArtifactInUse(src) {
		t.Fatal("引用方任务终态后应恢复可清")
	}

	// ⑥ 在途发布（running）同样受保护
	mkRefJob(t, s, "j_pub_1", "publish", src, "running")
	if !s.JobArtifactInUse(src) {
		t.Fatal("running 中的发布任务引用源目录时必须保留")
	}
}
