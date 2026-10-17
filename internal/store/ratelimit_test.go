package store

import (
	"path/filepath"
	"testing"
	"time"
)

// mkStore 内存库临时文件（modernc sqlite 需落盘，用 t.TempDir）
func mkStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Close() }) // Windows：不关句柄 TempDir 清不掉
	return s
}

// addPublishJob 插一条发布任务（模拟调度/手动派发轨迹）
func addPublishJob(t *testing.T, s *Store, id, account string, createdAt int64) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO jobs (id,kind,status,created_at,input) VALUES (?,?,?,?,?)`,
		id, "publish", "done", createdAt, `{"account":"`+account+`","clip":"x.mp4"}`)
	if err != nil {
		t.Fatal(err)
	}
}

// TestAcctInputPattern LIKE 模式：下划线账号名（bili_395147691）不能误匹配
// 同形异名（biliX395147691）——_ 必须 ESCAPE
func TestAcctInputPattern(t *testing.T) {
	s := mkStore(t)
	now := time.Now().Unix()
	addPublishJob(t, s, "j1", "bili_395147691", now-10)
	addPublishJob(t, s, "j2", "biliX395147691", now-5) // 干扰项：_ 若未转义会同时命中

	last, err := s.LastPublishAt("bili_395147691")
	if err != nil {
		t.Fatal(err)
	}
	if last != now-10 {
		t.Fatalf("LastPublishAt 应命中 j1(now-10)，得到 %d", last)
	}
	last2, _ := s.LastPublishAt("biliX395147691")
	if last2 != now-5 {
		t.Fatalf("干扰账号应命中 j2(now-5)，得到 %d", last2)
	}
}

// TestLastPublishAtEmpty 无记录/全局查询
func TestLastPublishAtEmpty(t *testing.T) {
	s := mkStore(t)
	if last, _ := s.LastPublishAt("nobody"); last != 0 {
		t.Fatalf("无记录应返回 0，得到 %d", last)
	}
	now := time.Now().Unix()
	addPublishJob(t, s, "j1", "a", now)
	addPublishJob(t, s, "j2", "b", now+1)
	if g, _ := s.LastPublishAt(""); g != now+1 {
		t.Fatalf("全局查询应取最新，得到 %d", g)
	}
}

// TestCountPublishSince 每日上限计数：跨天边界（只数今日）+ 账号隔离
func TestCountPublishSince(t *testing.T) {
	s := mkStore(t)
	now := time.Now()
	today0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()

	addPublishJob(t, s, "j1", "a", today0-3600)     // 昨天：不计
	addPublishJob(t, s, "j2", "a", today0+100)      // 今日
	addPublishJob(t, s, "j3", "a", today0+200)      // 今日
	addPublishJob(t, s, "j4", "b", today0+300)      // 今日但另一账号

	n, err := s.CountPublishSince("a", today0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("账号 a 今日应 2 条，得到 %d", n)
	}
	if g, _ := s.CountPublishSince("", today0); g != 3 {
		t.Fatalf("全局今日应 3 条，得到 %d", g)
	}
}
