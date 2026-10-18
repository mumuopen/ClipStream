package main

import (
	"testing"

	"chopperbot/internal/store"
)

func acc(platform, name, labels, status string, prio int) *store.Account {
	return &store.Account{Platform: platform, Name: name, Labels: labels, Status: status, Priority: prio}
}

// TestRouteAccountWithBind 绑定优先级全矩阵：命中 > 平台不匹配回退 > 失效回退 > 无绑定标签路由
func TestRouteAccountWithBind(t *testing.T) {
	bili := map[string]bool{"bilibili": true}

	// ① 绑定命中：绑定账号优先于更高优先级的非绑定账号
	accs := []*store.Account{
		acc("bilibili", "高优先级", "游戏", "ok", 9),
		acc("bilibili", "绑定号", "赛事", "ok", 1),
	}
	got := routeAccountWithBind(accs, bili, "游戏", "绑定号")
	if got == nil || got.Name != "绑定号" {
		t.Fatalf("绑定账号应优先：got %v", got)
	}

	// ② 绑定账号停号（login_expired）：回退标签路由
	accs[1].Status = "login_expired"
	if got = routeAccountWithBind(accs, bili, "游戏", "绑定号"); got == nil || got.Name != "高优先级" {
		t.Fatalf("绑定失效应回退标签路由：got %v", got)
	}

	// ③ 绑定账号平台不匹配条目平台（条目发 douyin、绑的是 bili 号）：回退
	dy := map[string]bool{"douyin": true}
	if got = routeAccountWithBind(accs, dy, "", "绑定号"); got != nil {
		t.Fatalf("平台不匹配应无路由（池里没有 douyin 账号）：got %v", got)
	}

	// ④ 绑定账号被节流让位（blocked 调用方已剔除）不在候选 → 走标签
	only := []*store.Account{acc("bilibili", "高优先级", "游戏", "ok", 9), acc("bilibili", "小号", "游戏", "ok", 2)}
	if got = routeAccountWithBind(only, bili, "游戏", "不在场的绑定"); got == nil || got.Name != "高优先级" {
		t.Fatalf("绑定账号不在候选应回退标签路由：got %v", got)
	}

	// ⑤ 无绑定：纯标签路由（回归保护）
	if got = routeAccountWithBind(only, bili, "游戏", ""); got == nil || got.Name != "高优先级" {
		t.Fatalf("无绑定应按标签+优先级：got %v", got)
	}
}
