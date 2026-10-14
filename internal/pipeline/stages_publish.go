package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"chopperbot/internal/core"
	"chopperbot/internal/publish"
)

// publishStages M2 发布管线：prepare → publish → finalize
func publishStages() []Stage {
	return []Stage{
		{Name: "prepare", Run: stagePublishPrepare},
		{Name: "publish", Run: stagePublishRun},
		{Name: "finalize", Run: stagePublishFinalize},
	}
}

// ---- prepare：解析请求、校验文件与平台 ----

func stagePublishPrepare(_ context.Context, job *core.Job, env *Env) error {
	req, err := publish.ParseRequest(job.Input)
	if err != nil {
		return err
	}
	video := filepath.Join(env.Cfg.DataDir, "jobs", req.SourceJob, filepath.FromSlash(req.Clip))
	if _, err := os.Stat(video); err != nil {
		return fmt.Errorf("切片文件不存在: %s（源任务 %s）", video, req.SourceJob)
	}
	cover := ""
	if req.Cover != "" {
		c := filepath.Join(env.Cfg.DataDir, "jobs", req.SourceJob, filepath.FromSlash(req.Cover))
		if _, err := os.Stat(c); err == nil {
			cover = c
		} else {
			env.log(job, "封面不存在，跳过封面：%s", req.Cover)
		}
	}
	job.SetParam("video_path", video)
	job.SetParam("cover_path", cover)
	env.log(job, "发布计划：%d 个平台 [%s]｜标题：%s｜切片：%s", len(req.Platforms), joinStrings(req.Platforms, ","), req.Title, req.Clip)
	return nil
}

// ---- publish：分发执行 ----

func stagePublishRun(ctx context.Context, job *core.Job, env *Env) error {
	req, err := publish.ParseRequest(job.Input)
	if err != nil {
		return err
	}
	params := job.ParamsMap()
	result := publish.Dispatch(ctx, env.Cfg, req, params["video_path"], params["cover_path"], job.Dir,
		func(format string, args ...any) { env.log(job, format, args...) })

	b, _ := json.Marshal(result)
	job.SetParam("publish_results", string(b))
	if !result.AllOK {
		return errors.New("部分或全部平台发布失败（详见 publish_results 与日志）")
	}
	return nil
}

// ---- finalize：落结果、状态与通知（失败自动回池 + 账号登录态回写）----

func stagePublishFinalize(_ context.Context, job *core.Job, env *Env) error {
	var result publish.Result
	if s := job.ParamsMap()["publish_results"]; s != "" {
		_ = json.Unmarshal([]byte(s), &result)
	}
	job.SetResult(result)

	if result.AllOK {
		job.Status = core.StatusDone
	} else {
		job.Status = core.StatusFailed
	}

	// 失败善后（无人值守核心：不能让内容静默损失）
	req, _ := publish.ParseRequest(job.Input)
	postmortem := ""
	if !result.AllOK && req != nil {
		loginExpired := false
		for _, r := range result.Results {
			if !r.OK && strings.Contains(r.Detail, "登录态缺失") {
				loginExpired = true
			}
		}
		// 1) 登录态缺失：账号标 login_expired（routeAccount 自动避开，UI 提示重新扫码）
		if loginExpired && req.Account != "" {
			if err := env.Store.SetAccountStatus("bilibili", req.Account, "login_expired"); err == nil {
				postmortem = fmt.Sprintf("\n账号「%s」登录态已过期，已停用其自动发布——请在发布中心重新扫码登录。", req.Account)
			}
		}
		// 2) 调度派发的任务失败：池条目回 pending，下次有可用账号时自动重试
		if req.PoolItemID > 0 {
			if err := env.Store.SetPoolItemStatus(req.PoolItemID, "pending"); err == nil {
				if postmortem == "" {
					postmortem = "\n该切片已退回待发池，账号可用后下次定时发布自动重试。"
				} else {
					postmortem += "\n该切片已退回待发池，账号可用后下次定时发布自动重试。"
				}
			}
		}
	}

	// pushme 通知发布结果
	var lines string
	for _, r := range result.Results {
		status := "✔"
		if !r.OK {
			status = "✗"
		}
		lines += fmt.Sprintf("\n%s %s：%s", status, r.Platform, r.Detail)
	}
	if result.DryRun {
		env.notify("ClipStream 发布演练完成", "任务 "+job.ID+"（dry-run，未真发）"+lines)
	} else {
		env.notify("ClipStream 发布结果", "任务 "+job.ID+lines+postmortem)
	}
	return nil
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
