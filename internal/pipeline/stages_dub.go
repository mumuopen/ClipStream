package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"chopperbot/internal/core"
	"chopperbot/internal/engine"
	"chopperbot/internal/publish"
)

// dubStages M3.2 配音解说链：prepare → script → synthesize → mix → finalize
// 流程：取切片时间窗内转写 → LLM 写中文解说稿 → edge-tts 合成 → 变速对齐 → 替换音轨 → 回审核台
func dubStages() []Stage {
	return []Stage{
		{Name: "prepare", Run: stageDubPrepare},
		{Name: "script", Run: stageDubScript},
		{Name: "synthesize", Run: stageDubSynthesize},
		{Name: "mix", Run: stageDubMix},
		{Name: "finalize", Run: stageDubFinalize},
	}
}

func stageDubPrepare(_ context.Context, job *core.Job, env *Env) error {
	req, err := publish.ParseDubRequest(job.Input)
	if err != nil {
		return err
	}
	src, clip, err := loadSourceClipForDub(env, req.SourceJob, req.Clip)
	if err != nil {
		return err
	}
	videoPath := filepath.Join(env.Cfg.DataDir, "jobs", req.SourceJob, filepath.FromSlash(req.Clip))
	if _, err := os.Stat(videoPath); err != nil {
		return fmt.Errorf("切片文件不存在: %s", videoPath)
	}
	// 视频时长（解说稿与变速按这个算）
	videoDur, err := env.FFMPEG.ProbeDuration(context.Background(), videoPath)
	if err != nil {
		return fmt.Errorf("探测切片时长失败: %w", err)
	}

	// 时间窗内转写片段（与字幕链同规则）
	var segs []engine.Segment
	data, err := os.ReadFile(filepath.Join(src.Dir, "transcript.json"))
	if err != nil {
		return fmt.Errorf("源任务缺少 transcript.json，无法写解说稿: %w", err)
	}
	if err := json.Unmarshal(data, &segs); err != nil {
		return fmt.Errorf("解析源转写失败: %w", err)
	}
	var texts []string
	for _, s := range segs {
		if s.End > clip.Start && s.Start < clip.End && strings.TrimSpace(s.Text) != "" {
			texts = append(texts, strings.TrimSpace(s.Text))
		}
	}
	if len(texts) == 0 {
		return fmt.Errorf("切片 %s 时间窗内没有可用的语音内容", req.Clip)
	}
	job.SetParam("dub_source_job", req.SourceJob)
	job.SetParam("dub_video", videoPath)
	job.SetParam("dub_video_dur", fmt.Sprintf("%.3f", videoDur))
	job.SetParam("dub_voice", engine.NormalizeVoice(req.Voice))
	job.SetParam("dub_keep_audio", fmt.Sprintf("%v", req.KeepAudio))
	job.SetParam("dub_title", clip.Title)
	job.SetParam("dub_cover", clip.Cover)
	job.SetParam("dub_script_src", strings.Join(texts, "\n"))
	env.log(job, "配音计划：切片 %s（%.0fs）｜音色 %s｜%d 条转写素材", req.Clip, videoDur, engine.NormalizeVoice(req.Voice), len(texts))
	return nil
}

func stageDubScript(ctx context.Context, job *core.Job, env *Env) error {
	if env.LLM == nil || !env.LLM.Available() {
		return fmt.Errorf("解说稿需要 LLM（config.yaml 的 llm 段未配置）")
	}
	params := job.ParamsMap()
	dur := 0.0
	fmt.Sscanf(params["dub_video_dur"], "%f", &dur)
	// 中文口播语速约 4 字/秒，按视频时长控制解说稿字数
	targetChars := int(dur * 4)
	system := fmt.Sprintf(`你是短视频解说撰稿人。根据给定的视频转写素材，写一段适合口播的简体中文解说稿。
要求：只输出解说稿正文（不要标题、不要引号、不要任何解释）；口语自然、有节奏感、留出自然停顿；严格控制在 %d 字左右（视频时长约 %.0f 秒，中文语速约4字/秒），宁短勿长。`, targetChars, dur)
	reply, err := env.LLM.Chat(ctx, system, "转写素材：\n"+params["dub_script_src"])
	if err != nil {
		return fmt.Errorf("解说稿生成失败: %w", err)
	}
	script := strings.TrimSpace(reply)
	script = strings.Trim(script, "\"“”'")
	if err := os.WriteFile(jobFile(job, "script.txt"), []byte(script), 0o644); err != nil {
		return err
	}
	job.SetParam("dub_script_len", fmt.Sprintf("%d", len([]rune(script))))
	env.log(job, "解说稿完成（%d 字）：%s", len([]rune(script)), clipStr(script, 60))
	return nil
}

func stageDubSynthesize(ctx context.Context, job *core.Job, env *Env) error {
	params := job.ParamsMap()
	script, err := os.ReadFile(jobFile(job, "script.txt"))
	if err != nil {
		return err
	}
	// edge-tts 合成（mp3）
	raw := jobFile(job, "dub_raw.mp3")
	env.log(job, "edge-tts 合成（音色 %s）…", params["dub_voice"])
	tts := &engine.EdgeTTS{Bin: "edge-tts"}
	if err := tts.Synthesize(ctx, string(script), params["dub_voice"], raw); err != nil {
		return err
	}
	// 变速/补静音对齐到视频时长（wav 无损处理链）
	rawDur, _ := env.FFMPEG.ProbeDuration(ctx, raw)
	fitted := jobFile(job, "dub_fit.wav")
	dur := 0.0
	fmt.Sscanf(params["dub_video_dur"], "%f", &dur)
	if err := env.FFMPEG.FitAudioToDuration(ctx, raw, fitted, dur); err != nil {
		return err
	}
	job.SetParam("dub_audio", fitted)
	env.log(job, "解说音轨：%.1fs → 对齐 %.1fs", rawDur, dur)
	return nil
}

func stageDubMix(ctx context.Context, job *core.Job, env *Env) error {
	params := job.ParamsMap()
	video := params["dub_video"]
	audio := params["dub_audio"]
	if video == "" || audio == "" {
		return fmt.Errorf("参数缺失（video/audio）")
	}
	srcJob := params["dub_source_job"]
	srcClipsDir := filepath.Join(env.Cfg.DataDir, "jobs", srcJob, "clips")
	base := filepath.Base(video)
	// clip_06_sub.mp4 → clip_06_dub.mp4（不叠后缀链：dub 版总是从"最终画面版"出发）
	outName := dubOutName(base)
	out := filepath.Join(srcClipsDir, outName)
	keepAudio := params["dub_keep_audio"] == "true"
	if err := env.FFMPEG.ReplaceAudio(ctx, video, audio, out, keepAudio); err != nil {
		return err
	}
	subRel, _ := filepath.Rel(filepath.Join(env.Cfg.DataDir, "jobs", srcJob), out)
	job.SetParam("dub_out", filepath.ToSlash(subRel))
	env.log(job, "✔ 配音完成：%s（原声 %s）", filepath.ToSlash(subRel), map[bool]string{true: "保留作背景", false: "已替换"}[keepAudio])
	return nil
}

func stageDubFinalize(_ context.Context, job *core.Job, env *Env) error {
	params := job.ParamsMap()
	res := publish.DubResult{
		SourceJob: params["dub_source_job"],
		Original:  dubReqClip(job),
		Dubbed:    params["dub_out"],
		Cover:     params["dub_cover"],
		Title:     params["dub_title"],
		Voice:     params["dub_voice"],
	}
	fmt.Sscanf(params["dub_script_len"], "%d", &res.ScriptLen)
	job.SetResult(res)
	job.Status = core.StatusReview // 发布决策权在用户

	env.notify("ClipStream 配音切片待审核",
		fmt.Sprintf("任务 %s 完成：《%s》解说配音版进入审核台", job.ID, res.Title))
	return nil
}

// ---- 工具 ----

// loadSourceClip 读源任务的切片信息（clips 与 subtitle/dub 任务都会更新结果，dub 也可能引用 _sub 版）
func loadSourceClip(env *Env, sourceJob, clipFile string) (*core.Job, *sourceClipInfo, error) {
	src, err := env.Store.GetJob(sourceJob)
	if err != nil {
		return nil, nil, fmt.Errorf("源任务不存在: %s", sourceJob)
	}
	var srcResult struct {
		Clips []sourceClipInfo `json:"clips"`
	}
	if err := json.Unmarshal([]byte(src.Result), &srcResult); err != nil || len(srcResult.Clips) == 0 {
		return nil, nil, fmt.Errorf("源任务 %s 无切片结果", sourceJob)
	}
	for i := range srcResult.Clips {
		if srcResult.Clips[i].File == clipFile {
			return src, &srcResult.Clips[i], nil
		}
	}
	return nil, nil, fmt.Errorf("源任务 %s 中找不到切片 %s", sourceJob, clipFile)
}

// loadSourceClipForDub dub 用：输入可能是 _sub/_dub 派生文件（不在 clips 数组中），
// 先精确匹配，失败则退化到基础切片名取时间窗/标题/封面
func loadSourceClipForDub(env *Env, sourceJob, clipFile string) (*core.Job, *sourceClipInfo, error) {
	src, clip, err := loadSourceClip(env, sourceJob, clipFile)
	if err == nil {
		return src, clip, nil
	}
	base := clipFile
	for _, suf := range []string{"_sub.mp4", "_dub.mp4"} {
		if strings.HasSuffix(base, suf) {
			base = strings.TrimSuffix(base, suf) + ".mp4"
			break
		}
	}
	if base != clipFile {
		if s2, c2, e2 := loadSourceClip(env, sourceJob, base); e2 == nil {
			return s2, c2, nil
		}
	}
	return nil, nil, err
}

func dubReqClip(job *core.Job) string {
	req, err := publish.ParseDubRequest(job.Input)
	if err != nil {
		return ""
	}
	return req.Clip
}

// dubOutName 配音产物名：只换扩展名，不剥 _sub。
// 旧版把 clip_06.mp4 与 clip_06_sub.mp4 都映射成 clip_06_dub.mp4——同一场直播
// 先给原片配音、再给字幕版配音时，第二次静默覆盖第一次的产物（结果里还写着
// 两条都成功）。
func dubOutName(base string) string {
	ext := filepath.Ext(base)
	if ext == "" {
		return base + "_dub.mp4"
	}
	return strings.TrimSuffix(base, ext) + "_dub" + ext
}

func clipStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
