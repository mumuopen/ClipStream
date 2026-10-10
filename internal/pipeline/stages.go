package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"chopperbot/internal/core"
	"chopperbot/internal/engine"
)

// clipStages M1 搬运转译管线：下载 → 转写 → 高光 → 切片 → 审核
func clipStages() []Stage {
	return []Stage{
		{Name: "download", Run: stageDownload},
		{Name: "transcribe", Run: stageTranscribe},
		{Name: "highlight", Run: stageHighlight},
		{Name: "cut", Run: stageCut},
		{Name: "finalize", Run: stageFinalize},
	}
}

func jobFile(job *core.Job, name string) string {
	return filepath.Join(job.Dir, name)
}

// ---- download：本地文件直用，否则 yt-dlp ----

func stageDownload(ctx context.Context, job *core.Job, env *Env) error {
	in := job.Input
	if fi, err := os.Stat(in); err == nil && !fi.IsDir() {
		abs, _ := filepath.Abs(in)
		env.log(job, "本地文件直入：%s", abs)
		job.SetParam("video", abs)
		return nil
	}
	env.log(job, "yt-dlp 下载：%s", in)
	path, err := env.YTDLP.Download(ctx, in, job.Dir, env.Cfg.Proxy)
	if err != nil {
		return err
	}
	job.SetParam("video", path)
	env.log(job, "下载完成：%s", path)
	return nil
}

// ---- transcribe ----

func stageTranscribe(ctx context.Context, job *core.Job, env *Env) error {
	video := job.ParamsMap()["video"]
	if video == "" {
		return errors.New("缺少视频路径（download 阶段未产出）")
	}
	segs, err := env.ASR.Transcribe(ctx, video, job.Dir)
	if err != nil {
		if errors.Is(err, engine.ErrUnavailable) {
			env.log(job, "ASR 引擎不可用，跳过转写（将使用等分切片回退）：%v", err)
			return nil
		}
		return fmt.Errorf("转写失败: %w", err)
	}
	b, _ := json.MarshalIndent(segs, "", "  ")
	if err := os.WriteFile(jobFile(job, "transcript.json"), b, 0o644); err != nil {
		return err
	}
	job.SetParam("transcript", jobFile(job, "transcript.json"))
	env.log(job, "转写完成：%d 条片段（%s）", len(segs), jobFile(job, "transcript.json"))
	return nil
}

// ---- highlight ----

func stageHighlight(ctx context.Context, job *core.Job, env *Env) error {
	var segs []engine.Segment
	duration := 0.0
	if data, err := os.ReadFile(jobFile(job, "transcript.json")); err == nil {
		if err := json.Unmarshal(data, &segs); err == nil {
			for _, s := range segs {
				if s.End > duration {
					duration = s.End
				}
			}
		}
	}

	var clips []engine.Clip
	fromLLM := false
	if len(segs) > 0 && env.LLM != nil && env.LLM.Available() {
		llmClips, err := llmHighlight(ctx, env.LLM, segs)
		if err != nil {
			env.log(job, "LLM 高光分析失败（回退等分切片）：%v", err)
		} else if len(llmClips) > 0 {
			clips = llmClips
			fromLLM = true
			env.log(job, "LLM 高光分析完成：%d 段候选", len(llmClips))
		}
	}
	if len(clips) == 0 {
		if duration <= 0 {
			if d, err := env.FFMPEG.ProbeDuration(ctx, job.ParamsMap()["video"]); err == nil {
				duration = d
			} else {
				return fmt.Errorf("无法获取视频时长: %w", err)
			}
		}
		clips = fallbackChunks(duration)
		env.log(job, "使用等分切片策略：%d 段（每段约 %.0fs）", len(clips), duration/float64(len(clips)))
	}

	// 过滤非法区间：下限 8s 对所有来源生效；上限 300s 仅约束 LLM 输出（等分回退按构造保证全覆盖）
	var kept []engine.Clip
	for _, c := range clips {
		d := c.End - c.Start
		if d >= 8 && c.Start >= 0 && c.End > c.Start && (!fromLLM || d <= 300) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return errors.New("高光切片为空（时长过短或区间非法）")
	}
	b, _ := json.MarshalIndent(kept, "", "  ")
	if err := os.WriteFile(jobFile(job, "clips.json"), b, 0o644); err != nil {
		return err
	}
	env.log(job, "高光切片候选：%d 段", len(kept))
	return nil
}

// llmHighlight 用 LLM 从转写文本中选出高光区间
func llmHighlight(ctx context.Context, llm *engine.LLMClient, segs []engine.Segment) ([]engine.Clip, error) {
	var sb strings.Builder
	for _, s := range segs {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%.1f-%.1f] %s\n", s.Start, s.End, text))
		if sb.Len() > 8000 {
			break
		}
	}
	system := `你是一个短视频高光剪辑师。根据带时间戳的直播/视频转写文本，挑选最有传播潜力的高光片段（爆点、金句、名场面、高能操作）。只输出 JSON 数组，不要任何其他文字，格式：[{"start":秒,"end":秒,"title":"爆款标题","score":0-100}]。要求：每段 15-120 秒，区间互不重叠，最多 6 段，时间必须严格取自文本时间戳。`
	reply, err := llm.Chat(ctx, system, "转写文本：\n"+sb.String())
	if err != nil {
		return nil, err
	}
	reply = strings.TrimSpace(reply)
	// 去掉可能包裹的 ```json ... ```
	if i := strings.Index(reply, "["); i >= 0 {
		reply = reply[i:]
	}
	if i := strings.LastIndex(reply, "]"); i >= 0 {
		reply = reply[:i+1]
	}
	var clips []engine.Clip
	if err := json.Unmarshal([]byte(reply), &clips); err != nil {
		return nil, fmt.Errorf("LLM 输出解析失败: %w（原文: %s）", err, tail(reply, 200))
	}
	return clips, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// fallbackChunks 无 LLM/转写时的等分切片：目标每段约 60s，最多 20 段，重新均分保证覆盖全时长
func fallbackChunks(duration float64) []engine.Clip {
	if duration <= 0 {
		return nil
	}
	n := int(math.Ceil(duration / 60))
	if n < 1 {
		n = 1
	}
	if n > 20 {
		n = 20
	}
	chunk := duration / float64(n) // 均分覆盖全时长（超长视频时每段>60s）
	clips := make([]engine.Clip, 0, n)
	for i := 0; i < n; i++ {
		start := math.Round(chunk*float64(i)*10) / 10
		end := math.Round(math.Min(chunk*float64(i+1), duration)*10) / 10
		if end-start < 5 {
			break
		}
		clips = append(clips, engine.Clip{
			Start: start,
			End:   end,
			Title: fmt.Sprintf("片段 %d", i+1),
			Score: 0,
		})
	}
	return clips
}

// ---- cut ----

type ClipResult struct {
	File  string  `json:"file"`
	Cover string  `json:"cover"`
	Title string  `json:"title"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Score float64 `json:"score"`
}

func stageCut(ctx context.Context, job *core.Job, env *Env) error {
	data, err := os.ReadFile(jobFile(job, "clips.json"))
	if err != nil {
		return fmt.Errorf("读取切片候选失败: %w", err)
	}
	var clips []engine.Clip
	if err := json.Unmarshal(data, &clips); err != nil {
		return err
	}
	video := job.ParamsMap()["video"]
	clipsDir := filepath.Join(job.Dir, "clips")
	if err := os.MkdirAll(clipsDir, 0o755); err != nil {
		return err
	}

	var results []ClipResult
	for i, c := range clips {
		outPath := filepath.Join(clipsDir, fmt.Sprintf("clip_%02d.mp4", i+1))
		if err := env.FFMPEG.Cut(ctx, video, outPath, c.Start, c.End); err != nil {
			env.log(job, "切片 %02d 失败：%v", i+1, err)
			continue
		}
		coverRel := ""
		cover := filepath.Join(clipsDir, fmt.Sprintf("clip_%02d.jpg", i+1))
		_ = env.FFMPEG.ExtractFrame(ctx, video, cover, (c.Start+c.End)/2)
		if _, err := os.Stat(cover); err == nil {
			coverRel = relPath(job.Dir, cover) // 提取失败时不留 404 引用
		}
		results = append(results, ClipResult{
			File:  relPath(job.Dir, outPath),
			Cover: coverRel,
			Title: c.Title,
			Start: c.Start,
			End:   c.End,
			Score: c.Score,
		})
		env.log(job, "✔ 切片 %02d：%.0fs-%.0fs ｜ %s", i+1, c.Start, c.End, c.Title)
	}
	if len(results) == 0 {
		return errors.New("所有切片均失败，请检查 ffmpeg 与源文件")
	}
	job.SetResult(map[string]any{"clips": results})
	return nil
}

func relPath(base, full string) string {
	p, err := filepath.Rel(base, full)
	if err != nil {
		return full
	}
	return filepath.ToSlash(p)
}

// ---- finalize ----

func stageFinalize(_ context.Context, job *core.Job, env *Env) error {
	job.Status = core.StatusReview // 进入审核台，等人工放行

	// pushme 通知：切片待审核
	var r struct {
		Clips []struct {
			Title string `json:"title"`
		} `json:"clips"`
	}
	_ = json.Unmarshal([]byte(job.Result), &r)
	consoleURL := "http://" + env.Cfg.Web.Listen
	if env.Cfg.Web.Listen == "" {
		consoleURL = "http://127.0.0.1:8899"
	}
	env.notify("GuanClip 切片待审核",
		fmt.Sprintf("任务 %s 完成，%d 个切片进入审核台：%s", job.ID, len(r.Clips), consoleURL))
	return nil
}
