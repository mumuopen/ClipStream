package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// ---- finalize：落结果、状态与通知 ----

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
		env.notify("GuanClip 发布演练完成", "任务 "+job.ID+"（dry-run，未真发）"+lines)
	} else {
		env.notify("GuanClip 发布结果", "任务 "+job.ID+lines)
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
