package publish

import (
	"context"
	"fmt"

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
