package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"chopperbot/internal/wexec"
)

// 本文件：ffmpeg 抽帧（关卡 1 干净度采样）——固定间隔 + 可选场景切换帧，
// 与 whisper/biliup 同款外部 CLI 集成模式（wexec 隔离子进程）。

// SampledFrame 一张采样帧（缩略帧也保原分辨率：模板匹配/OCR 的区域坐标
// 直接对应源画面，不做缩放）
type SampledFrame struct {
	Path  string  `json:"path"`
	T     float64 `json:"t"`     // 近似时间（秒）：固定帧=序号×间隔；场景帧按总时长均分近似
	Scene bool    `json:"scene"` // 场景切换帧
}

// ExtractSamples 从视频抽帧到 outDir：
//   - 固定间隔 intervalSec（fps=1/N 滤镜），帧时间精确为 (n-1)×N
//   - sceneDetect 时补充 select='gt(scene,0.4)' 场景切换帧，时间按总时长均分近似
//     （ffmpeg 帧序文件名拿不到精确 pts，止血版近似即可——决策日志标注）
func ExtractSamples(ctx context.Context, ffmpegBin, video, outDir string, intervalSec int, sceneDetect bool) ([]SampledFrame, error) {
	if ffmpegBin == "" {
		return nil, fmt.Errorf("ffmpeg 未配置（engines.ffmpeg），体检门无法抽帧")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if intervalSec <= 0 {
		intervalSec = 30
	}
	var frames []SampledFrame

	// ① 固定间隔帧
	args := []string{"-y", "-i", video, "-vf", fmt.Sprintf("fps=1/%d", intervalSec), "-q:v", "2",
		filepath.ToSlash(filepath.Join(outDir, "fixed_%04d.jpg"))}
	if _, err := wexec.Command(ctx, ffmpegBin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("抽帧失败: %v", err)
	}
	fixed, _ := listFrames(outDir, "fixed_")
	for i, f := range fixed {
		frames = append(frames, SampledFrame{Path: f, T: float64(i * intervalSec)})
	}

	// ② 场景切换帧（近似时间：拿总时长按帧数均分）
	if sceneDetect {
		args := []string{"-y", "-i", video, "-vf", "select='gt(scene,0.4)'", "-vsync", "vfr", "-q:v", "2",
			filepath.ToSlash(filepath.Join(outDir, "scene_%04d.jpg"))}
		if _, err := wexec.Command(ctx, ffmpegBin, args...).CombinedOutput(); err == nil {
			scene, _ := listFrames(outDir, "scene_")
			if n := len(scene); n > 0 {
				dur := probeDuration(ctx, ffmpegBin, video)
				for i, f := range scene {
					frames = append(frames, SampledFrame{Path: f, T: dur * float64(i) / float64(n), Scene: true})
				}
			}
		} // 场景帧失败不致命：固定间隔帧仍在，关卡照常判
	}

	sort.Slice(frames, func(i, j int) bool { return frames[i].T < frames[j].T })
	if len(frames) == 0 {
		return nil, fmt.Errorf("抽帧结果为空（视频损坏或时长过短）")
	}
	return frames, nil
}

// ExtractCenterFrames 从视频均匀抽 n 帧（场级基线构建用：首段 5 帧）
func ExtractCenterFrames(ctx context.Context, ffmpegBin, video, outDir string, n int) ([]SampledFrame, error) {
	if ffmpegBin == "" {
		return nil, fmt.Errorf("ffmpeg 未配置")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 5
	}
	dur := probeDuration(ctx, ffmpegBin, video)
	if dur <= 0 {
		dur = 300
	}
	var frames []SampledFrame
	for i := 0; i < n; i++ {
		t := dur * (float64(i) + 0.5) / float64(n)
		p := filepath.Join(outDir, fmt.Sprintf("base_%02d.jpg", i))
		args := []string{"-y", "-ss", strconv.FormatFloat(t, 'f', 2, 64), "-i", video,
			"-frames:v", "1", "-q:v", "2", filepath.ToSlash(p)}
		if _, err := wexec.Command(ctx, ffmpegBin, args...).CombinedOutput(); err != nil {
			continue // 单帧失败跳过（分段尾可能越界），有 3 帧可用即可
		}
		frames = append(frames, SampledFrame{Path: p, T: t})
	}
	if len(frames) < 3 {
		return nil, fmt.Errorf("基线抽帧不足（%d/%d 帧）", len(frames), n)
	}
	return frames, nil
}

// probeDuration ffprobe 时长（失败返回 0，调用方自行兜底近似）
func probeDuration(ctx context.Context, ffmpegBin, video string) float64 {
	probe := strings.TrimSuffix(ffmpegBin, filepath.Ext(ffmpegBin)) + "ffprobe"
	// engines 里 probe 与 ffmpeg 同目录命名（ffprobe.exe/ffmpeg.exe）；
	// 找不到时退回 ffmpeg 同目录猜测，再不行返回 0
	out, err := wexec.Command(ctx, probe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", video).CombinedOutput()
	if err == nil {
		if d, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); perr == nil {
			return d
		}
	}
	return 0
}

func listFrames(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".jpg") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}
