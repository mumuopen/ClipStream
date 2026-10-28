package pipeline

// 自动审核（M5 挂机无人值守）：切片完成后不再等人工，分级闸门决定去向。
//
//	开关关（默认）：全部进人工审核台 —— 与历史行为一致
//	开关开：
//	  硬闸（任一命中 → 拒发不入池，仅留档）：
//	    - 时长 < HardMinDurSec 或 > HardMaxDurSec
//	  质闸（任一命中 → 进人工审核台，等早晨处理）：
//	    - 无 LLM 评分（等分回退切片：内容质量未知）
//	    - LLM 评分 < MinScore（LLM 觉得不够好）
//	    - 转写为空（无有效语音：内容不明）
//	    - 标题/转写命中敏感词黑名单（发布必翻车）
//	  全过 → 自动入池 → 调度错峰发布
//
// 每日摘要：serve 侧 dailyDigestTicker 每天 09:00 pushme（自动发了什么/拦了什么）

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"chopperbot/internal/core"
	"chopperbot/internal/store"
)

// AutoReviewVerdict 单切片的审核判定
type AutoReviewVerdict struct {
	Clip    ClipResult
	Action  string `json:"action"`  // auto_pool | manual | reject
	Reason  string `json:"reason"`  // 判定原因（人和日志都看得懂）
}

// AutoReviewClips 对一个 clips 任务的全部切片跑自动审核，返回逐条判定。
// tagValues：切片的生成信号来源（弹幕热点有无），供质闸参考。
func AutoReviewClips(job *core.Job, env *Env, transcriptEmpty bool, bannedWords []string) []AutoReviewVerdict {
	cfg := env.Cfg.AutoReview
	var out []AutoReviewVerdict

	var r struct {
		Clips []ClipResult `json:"clips"`
	}
	_ = json.Unmarshal([]byte(job.Result), &r)

	for _, c := range r.Clips {
		dur := c.End - c.Start
		v := AutoReviewVerdict{Clip: c}

		// ── 硬闸：拒发（不入池，不占人工审核台；留 job 结果里可查）──
		if cfg.HardMinDurSec > 0 && dur < float64(cfg.HardMinDurSec) {
			v.Action, v.Reason = "reject", fmt.Sprintf("时长 %.0fs 低于下限 %ds", dur, cfg.HardMinDurSec)
			out = append(out, v)
			continue
		}
		if cfg.HardMaxDurSec > 0 && dur > float64(cfg.HardMaxDurSec) {
			v.Action, v.Reason = "reject", fmt.Sprintf("时长 %.0fs 超过上限 %ds", dur, cfg.HardMaxDurSec)
			out = append(out, v)
			continue
		}

		// ── 质闸：进人工审核台 ──
		if c.Score <= 0 {
			v.Action, v.Reason = "manual", "无 LLM 评分（等分回退切片，内容质量未知）"
			out = append(out, v)
			continue
		}
		if int(c.Score) < cfg.MinScore {
			v.Action, v.Reason = "manual", fmt.Sprintf("LLM 评分 %d 低于阈值 %d", int(c.Score), cfg.MinScore)
			out = append(out, v)
			continue
		}
		if transcriptEmpty {
			v.Action, v.Reason = "manual", "转写为空（纯音乐或无法识别，内容不明）"
			out = append(out, v)
			continue
		}
		if w := hitBanned(c.Title, bannedWords); w != "" {
			v.Action, v.Reason = "manual", fmt.Sprintf("标题命中敏感词「%s」", w)
			out = append(out, v)
			continue
		}

		// ── 全过：自动入池 ──
		v.Action, v.Reason = "auto_pool", fmt.Sprintf("评分 %d ≥ %d，时长 %.0fs", int(c.Score), cfg.MinScore, dur)
		out = append(out, v)
	}
	return out
}

// hitBanned 命中黑名单词（大小写不敏感），返回命中的词；未命中返回空
func hitBanned(s string, words []string) string {
	if s == "" || len(words) == 0 {
		return ""
	}
	ls := strings.ToLower(s)
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" && strings.Contains(ls, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// defaultPlatforms 自动入池的默认目标平台：优先 bilibili（保持既有行为不变），
// 只有非 bilibili 账号时用该平台——旧版硬编码 "bilibili"，只配抖音账号的部署里
// 条目永远路由不到账号（routeAccount 要求平台匹配），池永不下降 → 背压永不解除
// → 录制与送切永久停摆，且 maybeNotifyNoMatch 一天只提醒一次。
// 一个可用账号都没有时仍回退 "bilibili"（此时本来就发不出去，保持原行为）。
func (env *Env) defaultPlatforms() string {
	as, err := env.Store.ListAccounts()
	if err != nil {
		return "bilibili"
	}
	seen := map[string]bool{}
	var order []string
	for _, a := range as {
		if a.Status != "ok" || a.Platform == "" || seen[a.Platform] {
			continue
		}
		seen[a.Platform] = true
		order = append(order, a.Platform)
	}
	if seen["bilibili"] {
		return "bilibili"
	}
	if len(order) > 0 {
		return order[0]
	}
	return "bilibili"
}

// applyAutoReview 在 stageFinalize 后执行：判定 + 入池 + 日志。
// 返回 (auto入池数, 留人工数, 拒绝数)。
func applyAutoReview(job *core.Job, env *Env) (auto, manual, rejected int) {
	if !env.Cfg.AutoReview.Enabled {
		return 0, 0, 0 // 开关关：一切照旧，全部进人工审核台
	}

	// 转写是否为空（质闸信号）
	transcriptEmpty := false
	if b, err := os.ReadFile(jobFile(job, "transcript.json")); err == nil {
		var segs []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &segs) == nil {
			joined := ""
			for _, s := range segs {
				joined += strings.TrimSpace(s.Text)
			}
			transcriptEmpty = joined == ""
		}
	} else {
		transcriptEmpty = true // 无转写文件（whisper 未配/跳过）
	}

	verdicts := AutoReviewClips(job, env, transcriptEmpty, env.Cfg.AutoReview.BannedWords)
	for _, v := range verdicts {
		switch v.Action {
		case "auto_pool":
			// 投稿指纹治理：简介=切片区间转写摘要、标签=词典+标题抽词——
			// 空简介+固定标签的模板化投稿是平台搬运检测的经典机器特征
			desc := extractTranscriptSummary(job.Dir, v.Clip.Start, v.Clip.End)
			// 来源直播间（直播↔账号绑定路由的依据；非直播来源为空走标签路由）
			roomID := RoomIDFromPath(job.ParamsMap()["video"])
			item := &store.PoolItem{
				SourceJob: job.ID,
				Clip:      v.Clip.File,
				Cover:     v.Clip.Cover,
				Title:     v.Clip.Title,
				Desc:      desc,
				Tags:      deriveTags(v.Clip.Title, desc),
				Platforms: env.defaultPlatforms(),
				RoomID:    roomID,
			}
			if _, err := env.Store.AddPoolItem(item); err != nil {
				if errors.Is(err, store.ErrAlreadyInPool) {
					// 已在池（人工先入过池/上一轮已自动入过）：不算失败，不退回人工
					env.log(job, "[auto] 《%s》已在待发池，跳过重复入池", v.Clip.Title)
					auto++
					continue
				}
				env.log(job, "自动入池失败《%s》：%v", v.Clip.Title, err)
				manual++
				continue
			}
			auto++
			env.log(job, "[auto] 自动入池《%s》（%s｜简介 %d 字｜标签 %s）",
				v.Clip.Title, v.Reason, len([]rune(desc)), item.Tags)
		case "manual":
			manual++
			env.log(job, "[auto] 留人工审核《%s》（%s）", v.Clip.Title, v.Reason)
		case "reject":
			rejected++
			env.log(job, "[auto] 拒发《%s》（%s）", v.Clip.Title, v.Reason)
		}
	}
	return auto, manual, rejected
}