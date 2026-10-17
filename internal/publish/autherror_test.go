package publish

import "testing"

// TestIsAuthError 认证类失败判定：停号依据的关键词覆盖面。
// 旧版只匹配"登录态缺失"——cookie 过期（-101/csrf/401）与抖音措辞全部漏网
func TestIsAuthError(t *testing.T) {
	positive := []string{
		"B站登录态缺失（data\\accounts\\bilibili-x\\cookies.json）：请先执行 clipstream login",
		"登录态已失效：请重新扫码",
		"未登录，无法投稿",
		"biliup 失败: exit status 1\n[error] code: -101 账号未登录",
		"B站 401 Unauthorized",
		"csrf token 校验失败",
		"cookie 已过期",
		"invalid access token",
	}
	for _, s := range positive {
		if !IsAuthError(s) {
			t.Errorf("应判定为认证失败：%q", s)
		}
	}
	negative := []string{
		"biliup 失败: 网络超时",
		"上传失败：文件损坏",
		"",
		"分区 122 不存在",
	}
	for _, s := range negative {
		if IsAuthError(s) {
			t.Errorf("不应判定为认证失败：%q", s)
		}
	}
}
