package publish

import (
	"context"
	"fmt"
	"strings"

	"chopperbot/internal/config"
)

// Dispatch 发布分发：逐平台执行，单平台失败不影响其余平台
func Dispatch(ctx context.Context, cfg *config.Config, req *Request, videoPath, coverPath, workDir string, logf LogFunc) Result {
	result := Result{DryRun: req.DryRun, AllOK: true}
	for _, p := range req.Platforms {
		switch p {
		case "bilibili":
			pc, err := LoadPlatformConfig("bilibili", cfg.Publish.PlatformsDir)
			if err != nil {
				logf("bilibili 配置加载失败：%v", err)
				result.Results = append(result.Results, PlatformResult{Platform: p, OK: false, Detail: err.Error()})
				result.AllOK = false
				continue
			}
		r, err := BiliupPublish(ctx, cfg, pc, req, videoPath, coverPath, logf)
			if err != nil {
				logf("✗ B站发布失败：%v（%s）", err, r.Detail)
				// 错误必须进 result.Detail：finalize 的停号/回池判定按 Detail 匹配，
				// 详情为空时登录丢失等关键错误永远匹配不上（2026-10-19 审查发现的断链）
				if r.Detail == "" {
					r.Detail = err.Error()
				}
				result.AllOK = false
			}
			result.Results = append(result.Results, r)

		case "douyin":
			pc, err := LoadPlatformConfig("douyin", cfg.Publish.PlatformsDir)
			if err != nil {
				logf("douyin 配置加载失败：%v", err)
				result.Results = append(result.Results, PlatformResult{Platform: p, OK: false, Detail: err.Error()})
				result.AllOK = false
				continue
			}
			r, err := DouyinPublish(ctx, cfg, pc, req, videoPath, coverPath, workDir, logf)
			if err != nil {
				logf("✗ 抖音发布失败：%v（%s）", err, r.Detail)
				if r.Detail == "" {
					r.Detail = err.Error()
				}
				result.AllOK = false
			}
			result.Results = append(result.Results, r)

		default:
			logf("不支持的平台：%s（支持 bilibili / douyin）", p)
			result.Results = append(result.Results, PlatformResult{Platform: p, OK: false, Detail: fmt.Sprintf("不支持的平台: %s", p)})
			result.AllOK = false
		}
	}
	return result
}

// IsAuthError 登录态/认证类失败判定（停号依据）：供 publish finalize 匹配。
// 旧版只匹配“登录态缺失”四字——cookie 文件在但 token 过期时（biliup 报 -101/
// csrf/401）与抖音路径措辞（“登录态已失效”“未登录”）全部漏网，账号永不停号。
func IsAuthError(detail string) bool {
	d := strings.ToLower(detail)
	for _, kw := range []string{"登录态缺失", "登录态已失效", "未登录", "login expired"} {
		if strings.Contains(detail, kw) {
			return true
		}
	}
	for _, kw := range []string{"-101", "csrf", "token", "401", "cookie"} {
		if strings.Contains(d, kw) {
			return true
		}
	}
	return false
}

// IsRateLimited 平台限流判定（风控熔断依据）：B 站"投稿过于频繁"（错误码
// 137022，2026-10-24 服务器实测：10min 间隔连投 5 条即触发，22h 不解封——
// 每次失败重试都在刷新限流窗口，越撞越久）与抖音"操作太频繁"等文案。
// 命中即账号级冷却（publishThrottled 拦截派发），不让泵每 10 分钟继续撞墙。
func IsRateLimited(detail string) bool {
	d := strings.ToLower(detail)
	for _, kw := range []string{"过于频繁", "太频繁", "操作频繁", "频率过高", "频次过高", "rate limit", "frequen", "137022"} {
		if strings.Contains(d, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
