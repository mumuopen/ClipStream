package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chopperbot/internal/core"
	"chopperbot/internal/publish"
)

// publishStages M2 发布管线：prepare → publish → finalize
// 关键约束：stagePublishRun 永不返回 error（发布失败一律落到 result 里）——
// runner 的管线循环在阶段失败时中止（runner.go:99），finalize 会被跳过，
// 而"失败善后"（回池重试/login_expired 停号/结果通知）全在 finalize：
// 2026-10-19 实测发现旧版发布失败后条目永远卡在 dispatched、账号不停号，
// 就是 run 阶段失败把 finalize 短路了。只有 prepare 的请求级错误（JSON 坏/
// 字段缺失）允许中止——那类任务连 pool_item_id 都解析不出，善后无从谈起。
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
		// 素材缺失：善后回池计数（连败 3 次终态退池）再失败——防条目永远卡
		// dispatched（finalize 不跑就没有任何善后；这是 prepare 失败的唯一路径）
		if req.PoolItemID > 0 {
			final, _ := env.Store.MarkPoolItemRetry(req.PoolItemID, 3)
			_ = final
		}
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

// ---- publish：分发执行（失败不 return error，见 publishStages 注释）----

func stagePublishRun(ctx context.Context, job *core.Job, env *Env) error {
	req, err := publish.ParseRequest(job.Input)
	if err != nil {
		return err // 请求级错误（理论不可能：prepare 已校验过）
	}
	params := job.ParamsMap()
	result := publish.Dispatch(ctx, env.Cfg, req, params["video_path"], params["cover_path"], job.Dir,
		func(format string, args ...any) { env.log(job, format, args...) })

	b, _ := json.Marshal(result)
	job.SetParam("publish_results", string(b))
	return nil
}

// ---- finalize：落结果、状态与通知（必然执行：run 不失败，见 publishStages 注释）----

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
		// 任务 error 带首个失败平台具体原因（风控/封号码/网络错误列表一眼可见，不用点开详情）
		var firstFail string
		for _, r := range result.Results {
			if !r.OK {
				firstFail = r.Platform + "：" + firstNonEmptyLine(r.Detail, 200)
				break
			}
		}
		job.Error = fmt.Sprintf("发布失败——%s（详单见任务详情）", firstFail)
	}

	// 失败善后（无人值守核心：不能让内容静默损失）
	req, _ := publish.ParseRequest(job.Input)
	postmortem := ""
	rateLimited := false
	if !result.AllOK && req != nil {
		// 1) 登录态/认证类失败：对应平台账号标 login_expired（routeAccount 自动
		// 避开，UI 提示重新扫码）。按失败平台逐一停号（旧版写死 bilibili，
		// 抖音账号永不停号）；文案匹配走 publish.IsAuthError（覆盖两平台措辞
		// 与 biliup -101/csrf/401 退出特征，旧版只认“登录态缺失”四字形同虚设）
		stopped := []string{}
		for _, r := range result.Results {
			if r.OK || !publish.IsAuthError(r.Detail) {
				continue
			}
			if req.Account != "" && env.Store.SetAccountStatus(r.Platform, req.Account, "login_expired") == nil {
				stopped = append(stopped, r.Platform+"·"+req.Account)
			}
		}
		if len(stopped) > 0 {
			postmortem = fmt.Sprintf("\n账号「%s」登录态已失效，已停用其自动发布——请在发布中心重新扫码登录后续用。", strings.Join(stopped, "、"))
		}
		// 1.5) 平台限流熔断（2026-10-24 服务器实测教训：10min 间隔连投 5 条
		// 触发 B站 137022"投稿过于频繁"；此后 22h 每次 10min 的重试都在刷新
		// 限流窗口，120 次徒劳请求无一成功且越撞越久）：识别限流文案立即账号级
		// 冷却——publishThrottled 拦后续派发，冷却到点自动恢复；连续触发指数
		// 退避（base→×2→×4…上限 7 天），手动恢复保留计数（梯度继续）
		if req.Account != "" {
			for _, r := range result.Results {
				if r.OK || !publish.IsRateLimited(r.Detail) {
					continue
				}
				rateLimited = true
				base := env.Cfg.Publish.RateLimit.RiskCooldownHours
				if base <= 0 {
					base = 24
				}
				_, count := env.Store.AccountRiskPaused(req.Account)
				n := count + 1
				cd := time.Duration(base) * time.Hour
				for i := 1; i < n && cd < 7*24*time.Hour; i++ {
					cd *= 2
				}
				if cd > 7*24*time.Hour {
					cd = 7 * 24 * time.Hour
				}
				until := time.Now().Add(cd).Unix()
				if err := env.Store.MarkAccountRiskPaused(req.Account, until, n); err == nil {
					env.log(job, "风控熔断：账号「%s」限流冷却 %s（至 %s，第 %d 次触发）", req.Account, cd.Round(time.Hour), time.Unix(until, 0).Format("01-02 15:04"), n)
					postmortem += fmt.Sprintf("\n检测到平台限流（投稿过于频繁）：账号「%s」已自动暂停发布，冷却约 %s（至 %s）后自动恢复，期间发布泵静默等待不再投稿。", req.Account, cd.Round(time.Hour), time.Unix(until, 0).Format("01-02 15:04"))
				}
				break // 按账号熔断一次足矣（多平台同账号共用冷却）
			}
		}
		// 2) 调度派发的任务失败：回池冷却重试（连续失败 3 次置 failed 终态，
		// 不再自动派发——防坏条目反复撞墙吃光当日配额；UI"已退池"红色提示人工处置）。
		// 多平台部分成功：先剔除已成功平台再回池——重派只补发失败平台；
		// 剔除后剩空（每平台至少成功过一次）条目完结为 dispatched 不回池
		if req.PoolItemID > 0 {
			succeeded := platformNames(result, true)
			poolDone := false
			if len(succeeded) > 0 {
				if left, terr := env.Store.TrimPoolItemPlatforms(req.PoolItemID, succeeded); terr == nil && left == "" {
					_ = env.Store.SetPoolItemStatus(req.PoolItemID, "dispatched")
					env.log(job, "多平台发布已达成（成功：%s），条目完结", strings.Join(succeeded, ","))
					postmortem += "\n多平台发布已全部达成（个别平台此前失败现已重试成功），条目完结。"
					poolDone = true
				}
			}
		if !poolDone {
			if rateLimited {
				// 限流不是内容的错：不计失败次数（防 3 次退池误伤好条目——
				// 冷却期间每次重试都在撞限流墙，若计数会让整池条目被白白退池）。
				// 直接回 pending，冷却结束后发布泵自动重发；此前已成功的平台
				// 在 trim 阶段已剔除，不会重发
				_ = env.Store.SetPoolItemStatus(req.PoolItemID, "pending")
				env.log(job, "平台限流：条目回池不计失败次数，账号冷却后自动重发")
				postmortem += "\n本次失败为平台限流，不计入失败次数；条目已退回待发池，冷却结束后自动重发。"
			} else if final, err := env.Store.MarkPoolItemRetry(req.PoolItemID, 3); err == nil {
					if len(succeeded) > 0 {
						postmortem += fmt.Sprintf("\n已成功平台（%s）不再重发，仅补发失败平台。", strings.Join(succeeded, ","))
					}
					if final {
						postmortem += "\n该切片连续发布失败 3 次，已退池终态（不再自动重试）——请检查后删除或重新入池。"
					} else {
						postmortem += "\n该切片已退回待发池，冷却后由发布泵自动重试。"
					}
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
		lines += fmt.Sprintf("\n%s %s：%s", status, r.Platform, truncLine(r.Detail, 200))
	}
	if result.DryRun {
		env.notify("ClipStream 发布演练完成", "任务 "+job.ID+"（dry-run，未真发）"+lines)
	} else {
		env.notify("ClipStream 发布结果", "任务 "+job.ID+lines+postmortem)
	}
	return nil
}

// platformNames 按结果取平台名列表（ok=true 取成功平台，false 取失败平台）
func platformNames(result publish.Result, ok bool) []string {
	var out []string
	for _, r := range result.Results {
		if r.OK == ok {
			out = append(out, r.Platform)
		}
	}
	return out
}

// firstNonEmptyLine 取多行文本首个非空行
func firstNonEmptyLine(s string, max int) string {	for _, line := range strings.Split(s, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return truncLine(l, max)
		}
	}
	return "未知原因"
}

func truncLine(l string, max int) string {
	if len(l) > max {
		return l[:max] + "…"
	}
	return l
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
