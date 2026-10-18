package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// 防呆：直播间链接走错入口。yt-dlp 对实时流只能抓到几秒残片，
	// 连续分段录制+弹幕采集是 daemon（直播关注）的活——直接指路而非产出垃圾。
	if isLiveRoomURL(in) {
		return fmt.Errorf("直播间链接 %q 不能走「投喂视频链接」：yt-dlp 对直播流只抓实时残片。\n请加到控制台「直播关注」页 —— daemon 会连续分段录制（600s/段）并采集弹幕，下播自动送切。本工具仅用于离线视频（B站稿件/YouTube 等）或本地文件", in)
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

// isLiveRoomURL 判断是否直播间地址（直播 ≠ 离线视频，抓取/路径不同）
// 虎牙所有房间都是直播；B站/抖音仅 live.* 子域是直播（www.bilibili.com/video/BV.. 是稿件，仍走搬运）。
func isLiveRoomURL(raw string) bool {
	u := strings.TrimSpace(raw)
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return false
	}
	host := strings.ToLower(p.Host)
	path := strings.Trim(p.Path, "/")
	switch {
	case strings.Contains(host, "huya.com") && isNumeric(path):
		return true
	case strings.Contains(host, "live.bilibili.com") || strings.Contains(host, "live.douyin.com"):
		return true
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
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

// ---- highlight（双信号：ASR 转写 + 弹幕密度）----

// Hotspot 弹幕热点区间（相对视频时间轴，秒）
type Hotspot struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Density float64 `json:"density"` // 区间内最高弹幕/5s
}

// liveSegRe 直播分段路径：data/live/<roomID>_<yyyyMMdd_HHmmss>/seg_<NNN>.flv
var liveSegRe = regexp.MustCompile(`[/\\]live[/\\](\d+)_(\d{8}_\d{6})[/\\](seg|merged)_(\d+)\.flv$`)

// RoomIDFromPath 从分段/合并产物路径解析来源房间号（入池记录 room_id，供
// 直播↔账号绑定的优先路由）。非直播分段返回空串
func RoomIDFromPath(path string) string {
	if m := liveSegRe.FindStringSubmatch(path); len(m) == 5 {
		return m[1]
	}
	return ""
}

// barrageHotspots 从视频路径关联直播会话，拉取弹幕密度桶并计算热点区间。
// 普通视频（非 data/live/ 分段）返回空，行为与原管线一致。
// seg_*.flv：窗口 = 会话起点 + 段序号 x 分段时长（墙钟轴即内容轴）。
// merged_*.flv（碎段流合并产物）：优先读同目录 .meta.json（各段墙钟时刻与内容轴
// 偏移），把弹幕墙钟时间映射到内容轴——虎牙碎段流内容轴有重连空洞（墙钟 ~5.3s/段
// 仅 ~2.2s 内容），直接相减会错位近一倍；meta 缺失时退回序号 x 分段时长近似。
func barrageHotspots(ctx context.Context, job *core.Job, env *Env) ([]Hotspot, error) {
	videoPath := job.ParamsMap()["video"]
	m := liveSegRe.FindStringSubmatch(videoPath)
	if len(m) != 5 {
		return nil, nil // 非直播分段，无弹幕信号
	}
	roomID := m[1]
	startUnix, err := time.ParseInLocation("20060102_150405", m[2], time.Local)
	if err != nil {
		return nil, nil
	}
	segIdx, _ := strconv.Atoi(m[4])
	segDur := int64(env.Cfg.Live.SegmentSec)
	if segDur <= 0 {
		segDur = 600
	}

	var winStart, winEnd int64                 // 弹幕拉取窗口（墙钟）
	mapToContent := func(int64) float64 { return -1 } // 墙钟 ts -> 内容轴秒；-1 表示按窗口起点直减
	if m[3] == "merged" {
		// 合并产物：读侧车 meta 精确定位窗口与内容轴映射
		if b, rerr := os.ReadFile(strings.TrimSuffix(videoPath, ".flv") + ".meta.json"); rerr == nil {
			var meta core.MergedSegMeta
			if jerr := json.Unmarshal(b, &meta); jerr == nil && len(meta.Segs) > 0 && meta.End > meta.Start {
				winStart, winEnd = meta.Start, meta.End
				mapToContent = func(ts int64) float64 { return wallToContent(meta, ts) }
			}
		}
		if winEnd == 0 { // 无 meta（旧产物）：按序号近似
			winStart = startUnix.Unix() + int64(segIdx)*segDur
			winEnd = winStart + segDur
		}
	} else {
		winStart = startUnix.Unix() + int64(segIdx)*segDur
		winEnd = winStart + segDur
	}

	buckets, err := env.Store.ListBarrage(roomID, winStart)
	if err != nil {
		return nil, err
	}
	if len(buckets) < 3 {
		return nil, nil // 弹幕太稀疏，无热点意义
	}
	type pt struct {
		rel float64 // 相对窗口起点秒（merged 场景为内容轴秒）
		cnt int
	}
	var pts []pt
	var sum int
	for _, b := range buckets {
		if b.TS < winStart || b.TS >= winEnd {
			continue
		}
		rel := mapToContent(b.TS)
		if rel < 0 {
			rel = float64(b.TS-winStart) + 2.5
		}
		if rel < 0 {
			continue
		}
		pts = append(pts, pt{rel: rel, cnt: b.Count})
		sum += b.Count
	}
	if len(pts) < 3 {
		return nil, nil
	}
	avg := float64(sum) / float64(len(pts))
	threshold := avg * 1.6 // 显著高于平均才算热点
	if threshold < 4 {
		threshold = 4
	}
	// 合并相邻热点桶（间隔 ≤12s 视为同一波）
	sort.Slice(pts, func(i, j int) bool { return pts[i].rel < pts[j].rel })
	var hotspots []Hotspot
	cur := Hotspot{}
	open := false
	for _, p := range pts {
		if float64(p.cnt) < threshold {
			if open {
				hotspots = append(hotspots, cur)
				open = false
			}
			continue
		}
		if !open {
			cur = Hotspot{Start: p.rel, End: p.rel + 5, Density: float64(p.cnt)}
			open = true
		} else if p.rel-cur.End <= 12 {
			cur.End = p.rel + 5
			if float64(p.cnt) > cur.Density {
				cur.Density = float64(p.cnt)
			}
		} else {
			hotspots = append(hotspots, cur)
			cur = Hotspot{Start: p.rel, End: p.rel + 5, Density: float64(p.cnt)}
		}
	}
	if open {
		hotspots = append(hotspots, cur)
	}
	// 过滤过短/过长热点（对齐最终切片过滤 8-300s）
	var kept []Hotspot
	for _, h := range hotspots {
		if d := h.End - h.Start; d >= 8 && d <= 300 {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// wallToContent 把弹幕墙钟时间映射到合并产物内容轴（秒）。
// 段级映射：定位 ts 落在哪段的墙钟区间（段区间 = [前段完成时刻, 本段完成时刻]，
// 首段起点为 meta.Start），段内按 内容时长/墙钟跨度 线性缩放（假设段内空洞均匀，
// 5s 弹幕桶粒度下误差可忽略）；越界钳制到 [0, 总内容时长]。
func wallToContent(meta core.MergedSegMeta, ts int64) float64 {
	if len(meta.Segs) == 0 {
		return -1
	}
	if ts <= meta.Start {
		return 0
	}
	last := meta.Segs[len(meta.Segs)-1]
	total := last.Offset + last.Dur
	if ts >= meta.End {
		return total
	}
	prevWall := meta.Start
	for _, s := range meta.Segs {
		if ts <= s.Wall {
			span := float64(s.Wall - prevWall)
			if span <= 0 {
				return s.Offset
			}
			return s.Offset + float64(ts-prevWall)/span*s.Dur
		}
		prevWall = s.Wall
	}
	return total
}

// hotspotsToClips 弹幕热点直接作为高光候选（无 LLM 时的规则回退）
func hotspotsToClips(hs []Hotspot) []engine.Clip {
	var clips []engine.Clip
	for _, h := range hs {
		clips = append(clips, engine.Clip{
			Start: h.Start,
			End:   h.End,
			Title: fmt.Sprintf("弹幕爆点 %.0f 条/5s", h.Density),
			Score: math.Min(99, h.Density*10),
		})
	}
	return clips
}

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
	// 双信号：先取弹幕热点（直播分段专属，普通视频为空）
	hotspots, herr := barrageHotspots(ctx, job, env)
	if herr != nil {
		env.log(job, "弹幕热点分析失败（忽略，继续单信号）：%v", herr)
	}
	if len(segs) > 0 && env.LLM != nil && env.LLM.Available() {
		llmClips, err := llmHighlight(ctx, env.LLM, segs, hotspots)
		if err != nil {
			env.log(job, "LLM 高光分析失败（回退等分切片）：%v", err)
		} else if len(llmClips) > 0 {
			clips = llmClips
			fromLLM = true
			env.log(job, "LLM 高光分析完成：%d 段候选（弹幕热点参考 %d 个）", len(llmClips), len(hotspots))
		}
	}
	// 无 LLM/LLM 失败但有弹幕热点：弹幕规则回退（纯等分仅剩最终兜底）
	if len(clips) == 0 && len(hotspots) > 0 {
		clips = hotspotsToClips(hotspots)
		env.log(job, "使用弹幕热点切片：%d 段（无 LLM 回退）", len(clips))
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

	// 过滤非法区间：LLM 输出先钳制到视频时长内（负起点归零/越界终点裁齐/整段越界丢弃），
	// 下限 15s 对所有来源生效；上限 300s 仅约束 LLM 输出（等分回退按构造保证全覆盖）
	kept, rejected := filterClips(clips, duration, fromLLM)
	if len(kept) == 0 && rejected > 0 {
		// 候选全灭不硬失败：降级等分兜底（挂机哲学——宁发等分片，不整单报废）
		if duration <= 0 {
			if d, err := env.FFMPEG.ProbeDuration(ctx, job.ParamsMap()["video"]); err == nil {
				duration = d
			}
		}
		fbKept, _ := filterClips(fallbackChunks(duration), duration, false)
		if len(fbKept) > 0 {
			env.log(job, "高光候选 %d 段全部被过滤，降级等分切片 %d 段", len(clips), len(fbKept))
			kept = fbKept
		}
	}
	if len(kept) == 0 {
		return fmt.Errorf("高光切片为空：视频时长 %.1fs，候选 %d 段（过滤 %d）均未通过校验——源视频短于 15s 最小切片下限或区间全非法，请检查下载/录制产物是否只有几秒存根", duration, len(clips), rejected)
	}
	b, _ := json.MarshalIndent(kept, "", "  ")
	if err := os.WriteFile(jobFile(job, "clips.json"), b, 0o644); err != nil {
		return err
	}
	env.log(job, "高光切片候选：%d 段", len(kept))
	return nil
}

// filterClips 校验并裁剪高光候选：
//   - LLM 输出钳制到视频时长内：负起点归零、超时长终点裁齐、整段越界丢弃
//   - 下限 15s 对所有来源生效（几秒的碎片没有传播价值——用户实测吐槽“1秒切片”）；
//     上限 300s 仅约束 LLM（等分/弹幕按构造有效）
func filterClips(clips []engine.Clip, duration float64, fromLLM bool) (kept []engine.Clip, rejected int) {
	for _, c := range clips {
		if fromLLM && duration > 0 {
			if c.Start >= duration {
				rejected++
				continue
			}
			if c.Start < 0 {
				c.Start = 0
			}
			if c.End > duration {
				c.End = duration
			}
		}
		d := c.End - c.Start
		if d >= 15 && c.Start >= 0 && c.End > c.Start && (!fromLLM || d <= 300) {
			kept = append(kept, c)
		} else {
			rejected++
		}
	}
	return kept, rejected
}

// llmHighlight 用 LLM 从转写文本 + 弹幕热点（双信号）选出高光区间
func llmHighlight(ctx context.Context, llm *engine.LLMClient, segs []engine.Segment, hotspots []Hotspot) ([]engine.Clip, error) {
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
	system := `你是一个短视频高光剪辑师。根据带时间戳的直播/视频转写文本，挑选最有传播潜力的高光片段（爆点、金句、名场面、高能操作）。只输出 JSON 数组，不要任何其他文字，格式：[{"start":秒,"end":秒,"title":"爆款标题","score":0-100}]。要求：每段 20-60 秒（低于 15 秒的碎片绝不要），区间互不重叠，最多 6 段，时间必须严格取自文本时间戳。`
	user := "转写文本：\n" + sb.String()
	// 双信号：弹幕互动峰值作为第二信号注入（观众真实反应验证过的效果点，优先采纳）
	if len(hotspots) > 0 {
		var hs strings.Builder
		for _, h := range hotspots {
			fmt.Fprintf(&hs, "[%.1f-%.1f] 平均 %.0f 条/5s\n", h.Start, h.End, h.Density)
		}
		system += ` 补充信号：以下为观众弹幕互动高峰区间（同一视频时间轴），代表真实效果爆点，命中区间优先纳入（可微调起止）。`
		user := "转写文本：\n" + sb.String() + "\n\n观众互动高峰：\n" + hs.String()
		reply, err := llm.Chat(ctx, system, user)
		if err != nil {
			return nil, err
		}
		return parseClipsReply(reply)
	}
	reply, err := llm.Chat(ctx, system, user)
	if err != nil {
		return nil, err
	}
	return parseClipsReply(reply)
}

// parseClipsReply 解析 LLM 输出的 JSON 数组（去 ```json 包裹）
func parseClipsReply(reply string) ([]engine.Clip, error) {
	reply = strings.TrimSpace(reply)
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

	// 自动审核（开关开时）：分级闸门决定切片去向（入池/留审/拒发）
	auto, manual, rejected := applyAutoReview(job, env)
	if env.Cfg.AutoReview.Enabled {
		// 挂机模式的通知：结果导向，早上看手机知道昨晚发生了什么
		env.notify("ClipStream 自动审核完成",
			fmt.Sprintf("任务 %s：自动入池 %d｜留人工 %d｜拒发 %d\n控制台：%s",
				job.ID, auto, manual, rejected, consoleURL))
		// 全自动过闸：无人工项时不占审核台（保持 done 语义由 pool 接管发布）
		if manual == 0 {
			job.Status = core.StatusDone
		} else {
			job.Status = core.StatusReview // 留人工项的等早晨处理
		}
		return nil
	}

	// 人工模式（默认）：全部进审核台，等放行
	job.Status = core.StatusReview
	env.notify("ClipStream 切片待审核",
		fmt.Sprintf("任务 %s 完成，%d 个切片进入审核台：%s", job.ID, len(r.Clips), consoleURL))
	return nil
}
