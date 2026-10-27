package web

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/store"
)

// mkTestStore 临时库（modernc sqlite 需落盘）
func mkTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// pubJob 造一条 running 发布任务（节流闸的数据源：jobs.input 的 account 字段）
func pubJob(t *testing.T, account string) *core.Job {
	t.Helper()
	return &core.Job{
		ID:     fmt.Sprintf("j_test_%d", time.Now().UnixNano()),
		Kind:   "publish",
		Status: core.StatusRunning,
		Input:  `{"account":"` + account + `","clip":"clips/clip_01.mp4","platforms":["bilibili"]}`,
		Dir:    "data/jobs/tmp",
	}
}

// TestThrottleReasonRiskCooldown 手动发布必须受风控冷却闸（2026-10-28 补）。
// 0.6.1 的熔断只在泵侧 publishThrottled 生效，web 侧 throttleReason 漏了这一闸
// 却注释写"与调度侧同语义"——冷却期内从审核台点一次发布就能继续撞 137022，
// 并让 risk_pause_count+1 把冷却 24h 翻成 48h。用户最常走的就是手动路径。
func TestThrottleReasonRiskCooldown(t *testing.T) {
	st := mkTestStore(t)
	if _, err := st.CreateAccount(&store.Account{Platform: "bilibili", Name: "bili_x", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	rl := config.RateLimit{} // 三闸全关，确保本测试只考察风控闸

	if why := throttleReason(st, rl, "bili_x"); why != "" {
		t.Fatalf("未冷却时不应被拦，got %q", why)
	}

	// 冷却中：必须拦住，且提示可手动恢复
	if err := st.MarkAccountRiskPaused("bili_x", time.Now().Add(2*time.Hour).Unix(), 2); err != nil {
		t.Fatal(err)
	}
	why := throttleReason(st, rl, "bili_x")
	if !strings.Contains(why, "风控冷却") {
		t.Fatalf("冷却中必须被拦且提示风控冷却，got %q", why)
	}
	if !strings.Contains(why, "第 2 次触发") {
		t.Fatalf("应透出触发次数（指数退避依据），got %q", why)
	}

	// 冷却过期：自动放行（到点自愈，无需人工）
	if err := st.MarkAccountRiskPaused("bili_x", time.Now().Add(-time.Minute).Unix(), 2); err != nil {
		t.Fatal(err)
	}
	if why := throttleReason(st, rl, "bili_x"); why != "" {
		t.Fatalf("冷却过期后不应再拦，got %q", why)
	}
}

// TestThrottleReasonOtherGates 三闸仍按原语义工作（回归：补风控闸不得打乱顺序与判定）
func TestThrottleReasonOtherGates(t *testing.T) {
	st := mkTestStore(t)
	const acc = "bili_x"

	// 在途上限：造一条 running 发布任务
	j := pubJob(t, acc)
	if err := st.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	if why := throttleReason(st, config.RateLimit{MaxInflight: 1}, acc); !strings.Contains(why, "在途") {
		t.Fatalf("在途闸应拦住，got %q", why)
	}

	// 每日上限
	if why := throttleReason(st, config.RateLimit{DailyMax: 1}, acc); !strings.Contains(why, "今日") {
		t.Fatalf("每日闸应拦住，got %q", why)
	}
}
