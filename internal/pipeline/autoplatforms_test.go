package pipeline

import (
	"path/filepath"
	"testing"

	"chopperbot/internal/store"
)

// TestDefaultPlatforms 自动入池的默认目标平台（2026-10-28）。
// 旧版硬编码 "bilibili"：只配抖音账号的部署里条目永远路由不到账号
// （routeAccount 要求平台匹配）→ 池永不下降 → 背压永不解除 → 录制永久停摆。
func TestDefaultPlatforms(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	env := &Env{Store: st}

	// ① 一个账号都没有：回退 bilibili（此时本来就发不出去，保持原行为）
	if got := env.defaultPlatforms(); got != "bilibili" {
		t.Fatalf("无账号时应回退 bilibili，got %q", got)
	}

	// ② 只有抖音账号：必须用 douyin，否则池永久阻塞
	if _, err := st.CreateAccount(&store.Account{Platform: "douyin", Name: "dy_main", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := env.defaultPlatforms(); got != "douyin" {
		t.Fatalf("只有抖音账号时应选 douyin，got %q", got)
	}

	// ③ 补上 B 站账号：优先 bilibili（保持既有行为，不会突然多平台同发）
	if _, err := st.CreateAccount(&store.Account{Platform: "bilibili", Name: "bili_main", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := env.defaultPlatforms(); got != "bilibili" {
		t.Fatalf("有 B 站账号时应优先 bilibili，got %q", got)
	}

	// ④ B 站账号登录过期：不算可用，退回 douyin
	if err := st.SetAccountStatus("bilibili", "bili_main", "login_expired"); err != nil {
		t.Fatal(err)
	}
	if got := env.defaultPlatforms(); got != "douyin" {
		t.Fatalf("B 站账号失效时应退回 douyin，got %q", got)
	}
}
