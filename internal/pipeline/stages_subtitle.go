package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chopperbot/internal/core"
	"chopperbot/internal/engine"
	"chopperbot/internal/publish"
)

// subtitleStages M3.1 字幕链：prepare → translate → burn → finalize
// 从源剪辑任务取切片时间范围与转写，LLM 翻译后烧双语/中文 SRT，产出的 *_sub.mp4 回审核台可发布
func subtitleStages() []Stage {
	return []Stage{
		{Name: "prepare", Run: stageSubtitlePrepare},
		{Name: "translate", Run: stageSubtitleTranslate},
		{Name: "burn", Run: stageSubtitleBurn},
		{Name: "finalize", Run: stageSubtitleFinalize},
	}
}

// sourceClipInfo 源任务结果里的切片信息
type sourceClipInfo struct {
	File  string  `json:"file"`
	Cover string  `json:"cover"`
	Title string  `json:"title"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

func stageSubtitlePrepare(_ context.Context, job *core.Job, env *Env) error {
	req, err := publish.ParseSubtitleRequest(job.Input)
	if err != nil {
		return err
	}
	// 源任务与其切片信息
	src, err := env.Store.GetJob(req.SourceJob)
	if err != nil {
		return fmt.Errorf("源任务不存在: %s", req.SourceJob)
	}
	var srcResult struct {
		Clips []sourceClipInfo `json:"clips"`
	}
	if err := json.Unmarshal([]byte(src.Result), &srcResult); err != nil || len(srcResult.Clips) == 0 {
		return fmt.Errorf("源任务 %s 无切片结果（需先跑 clips 管线）", req.SourceJob)
	}
	var clip *sourceClipInfo
	for i := range srcResult.Clips {
		if srcResult.Clips[i].File == req.Clip {
			clip = &srcResult.Clips[i]
			break
		}
	}
	if clip == nil {
		return fmt.Errorf("源任务 %s 中找不到切片 %s", req.SourceJob, req.Clip)
	}
	videoPath := filepath.Join(env.Cfg.DataDir, "jobs", req.SourceJob, filepath.FromSlash(req.Clip))
	if _, err := os.Stat(videoPath); err != nil {
		return fmt.Errorf("切片文件不存在: %s", videoPath)
	}

	// 源转写片段 → 裁出切片时间窗并相对化
	var segs []engine.Segment
	data, err := os.ReadFile(filepath.Join(src.Dir, "transcript.json"))
	if err != nil {
		return fmt.Errorf("源任务缺少 transcript.json（转写不可用），无法翻译: %w", err)
	}
	if err := json.Unmarshal(data, &segs); err != nil {
		return fmt.Errorf("解析源转写失败: %w", err)
	}
	clipStart, clipEnd := clip.Start, clip.End
	// 纳入条件：与切片时间窗的有效重叠 ≥0.3s（0.2s 缓冲只用于跨界衔接，
	// 沾边片段有效长度不足时丢弃，避免出现 0s→0s 的不可见字幕）
	const pad, minOverlap = 0.2, 0.3
	window := make([]engine.Segment, 0, 16)
	for _, s := range segs {
		effStart := math.Max(s.Start, clipStart-pad)
		effEnd := math.Min(s.End, clipEnd+pad)
		if effEnd-effStart < minOverlap || strings.TrimSpace(s.Text) == "" {
			continue
		}
		window = append(window, engine.Segment{
			Start: math.Max(0, s.Start-clipStart),
			End:   math.Min(clipEnd-clipStart, s.End-clipStart),
			Text:  strings.TrimSpace(s.Text),
		})
	}
	if len(window) == 0 {
		return fmt.Errorf("切片 %s 时间窗内没有可翻译的语音内容", req.Clip)
	}

	b, _ := json.MarshalIndent(window, "", "  ")
	if err := os.WriteFile(jobFile(job, "segments.json"), b, 0o644); err != nil {
		return err
	}
	job.SetParam("sub_source_job", req.SourceJob)
	job.SetParam("sub_style", req.Style)
	job.SetParam("sub_title", clip.Title)
	job.SetParam("sub_cover", clip.Cover)
	job.SetParam("sub_video", videoPath)
	env.log(job, "字幕计划：切片 %s [%.0fs-%.0fs]，%d 条待译片段，样式 %s", req.Clip, clip.Start, clip.End, len(window), req.Style)
	return nil
}

func stageSubtitleTranslate(ctx context.Context, job *core.Job, env *Env) error {
	if env.LLM == nil || !env.LLM.Available() {
		return fmt.Errorf("字幕翻译需要 LLM（config.yaml 的 llm 段未配置）")
	}
	data, err := os.ReadFile(jobFile(job, "segments.json"))
	if err != nil {
		return err
	}
	var segs []engine.Segment
	if err := json.Unmarshal(data, &segs); err != nil {
		return err
	}
	style := job.ParamsMap()["sub_style"]

	// LLM 批量翻译（切片≤300s，片段数可控，单次调用）
	type item struct {
		ID int    `json:"id"`
		EN string `json:"en"`
	}
	type zhItem struct {
		ID int    `json:"id"`
		ZH string `json:"zh"`
	}
	items := make([]item, len(segs))
	for i, s := range segs {
		items[i] = item{ID: i + 1, EN: s.Text}
	}
	in, _ := json.Marshal(items)
	env.log(job, "DeepSeek 翻译 %d 条片段…", len(segs))
	system := `你是专业视频字幕译者。把输入 JSON 数组中每条英文翻译成简体中文：语义准确、口语自然、符合字幕阅读节奏，专有名词规范。只输出 JSON 数组 [{"id":1,"zh":"译文"}]，不要输出任何其他内容。`
	reply, err := env.LLM.Chat(ctx, system, string(in))
	if err != nil {
		return fmt.Errorf("LLM 翻译失败: %w", err)
	}
	reply = strings.TrimSpace(reply)
	if i := strings.Index(reply, "["); i >= 0 {
		reply = reply[i:]
	}
	if i := strings.LastIndex(reply, "]"); i >= 0 {
		reply = reply[:i+1]
	}
	var zh []zhItem
	if err := json.Unmarshal([]byte(reply), &zh); err != nil {
		return fmt.Errorf("LLM 翻译输出解析失败: %w（原文: %s）", err, tailText(reply, 200))
	}
	zhMap := make(map[int]string, len(zh))
	for _, z := range zh {
		zhMap[z.ID] = strings.TrimSpace(z.ZH)
	}
	for i := range segs {
		if z, ok := zhMap[i+1]; ok {
			segs[i].Text = z // 暂存译文，写 SRT 时按样式取
		}
	}
	if style == "bilingual" {
		// 原文 JSON 存参数，写 SRT 时拼双语行
		if d2, err := os.ReadFile(jobFile(job, "segments.json")); err == nil {
			job.SetParam("sub_orig_json", string(d2))
		}
	}
	b, _ := json.MarshalIndent(segs, "", "  ")
	if err := os.WriteFile(jobFile(job, "segments_trans.json"), b, 0o644); err != nil {
		return err
	}

	srt := buildSRT(segs, style, job.ParamsMap()["sub_orig_json"])
	if err := os.WriteFile(jobFile(job, "sub.srt"), []byte(srt), 0o644); err != nil {
		return err
	}
	job.SetParam("sub_srt", jobFile(job, "sub.srt"))
	env.log(job, "翻译完成并生成 %s SRT（%d 条）", map[bool]string{true: "双语", false: "纯中文"}[style == "bilingual"], len(segs))
	return nil
}

func stageSubtitleBurn(ctx context.Context, job *core.Job, env *Env) error {
	params := job.ParamsMap()
	video := params["sub_video"]
	srt := params["sub_srt"]
	if video == "" || srt == "" {
		return fmt.Errorf("参数缺失（video/srt）")
	}
	// 输出到源任务 clips 目录：clip_06.mp4 → clip_06_sub.mp4（与原切片同库，发布可直接引用）
	srcJob := params["sub_source_job"]
	srcClipsDir := filepath.Join(env.Cfg.DataDir, "jobs", srcJob, "clips")
	base := filepath.Base(video)
	out := filepath.Join(srcClipsDir, strings.TrimSuffix(base, filepath.Ext(base))+"_sub.mp4")
	if err := env.FFMPEG.BurnSubtitles(ctx, video, out, filepath.Dir(srt), filepath.Base(srt)); err != nil {
		return err
	}
	subRel, _ := filepath.Rel(filepath.Join(env.Cfg.DataDir, "jobs", srcJob), out)
	job.SetParam("sub_out", filepath.ToSlash(subRel))
	job.SetParam("sub_out_abs", out)
	env.log(job, "✔ 字幕已烧入：%s", filepath.ToSlash(subRel))
	return nil
}

func stageSubtitleFinalize(_ context.Context, job *core.Job, env *Env) error {
	params := job.ParamsMap()
	data, _ := os.ReadFile(jobFile(job, "segments.json"))
	n := 0
	var segs []engine.Segment
	if json.Unmarshal(data, &segs) == nil {
		n = len(segs)
	}
	res := publish.SubtitleResult{
		SourceJob: params["sub_source_job"],
		Original:  jobParamsClip(job),
		Subtitled: params["sub_out"],
		Cover:     params["sub_cover"],
		Title:     params["sub_title"],
		Style:     params["sub_style"],
		Segments:  n,
	}
	job.SetResult(res)
	job.Status = core.StatusReview // 字幕切片回审核台，发布决策权在用户

	env.notify("ClipStream 字幕切片待审核",
		fmt.Sprintf("任务 %s 完成：《%s》双语/中文字幕版进入审核台，可在控制台预览与发布", job.ID, res.Title))
	return nil
}

// jobParamsClip 从任务 Input 取原始 clip 字段
func jobParamsClip(job *core.Job) string {
	req, err := publish.ParseSubtitleRequest(job.Input)
	if err != nil {
		return ""
	}
	return req.Clip
}

// ---- SRT 生成 ----

// buildSRT 生成 SRT 内容。style=bilingual 时上原文下中文；zh 时仅中文
func buildSRT(trans []engine.Segment, style, origJSON string) string {
	var orig []engine.Segment
	if style == "bilingual" {
		_ = json.Unmarshal([]byte(origJSON), &orig)
	}
	var sb strings.Builder
	for i, s := range trans {
		sb.WriteString(fmt.Sprintf("%d\n%s --> %s\n", i+1, srtTime(s.Start), srtTime(s.End)))
		switch style {
		case "bilingual":
			origText := ""
			if i < len(orig) {
				origText = orig[i].Text
			}
			sb.WriteString(origText + "\n" + s.Text + "\n\n")
		default:
			sb.WriteString(s.Text + "\n\n")
		}
	}
	return sb.String()
}

// srtTime 秒 → SRT 时间戳 HH:MM:SS,mmm
func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	d := time.Duration(sec * float64(time.Second))
	return fmt.Sprintf("%02d:%02d:%02d,%03d",
		int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, int(d.Milliseconds())%1000)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if hi > lo && v > hi {
		return hi
	}
	return v
}

func tailText(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
