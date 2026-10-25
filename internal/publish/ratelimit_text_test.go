package publish

import "testing"

// TestIsRateLimited 平台限流文案识别（风控熔断依据）：
// 正例来自 2026-10-24 服务器实测（B 站 137022"投稿过于频繁，请稍后再试"，
// 10min 间隔连投 5 条触发，22h 未解封）
func TestIsRateLimited(t *testing.T) {
	yes := []string{
		"投稿过于频繁，请稍后再试",
		`biliup 失败: exit status 1\n...上传成功后提交被拒："投稿过于频繁，请稍后再试"（137022）`,
		"操作太频繁了，休息一下再试吧",
		"请求频率过高",
		"ERROR: rate limited (code 137022)",
		"Request too frequent, please try later",
	}
	for _, s := range yes {
		if !IsRateLimited(s) {
			t.Errorf("应识别为限流: %q", s)
		}
	}
	no := []string{
		"",
		"biliup 失败: exit status 1（连接超时）",
		"登录态已失效",
		"稿件审核未通过",
		"封面尺寸不符",
		"网络连接被重置",
	}
	for _, s := range no {
		if IsRateLimited(s) {
			t.Errorf("不应误判为限流: %q", s)
		}
	}
}
