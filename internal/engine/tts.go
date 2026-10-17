package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"chopperbot/internal/wexec"
)

// EdgeTTS 微软 edge-tts 引擎（子进程，联网合成，免费音色）
type EdgeTTS struct {
	Bin   string // 可执行文件，默认 "edge-tts"
	Voice string // 默认音色
}

// 常用音色
var EdgeVoices = map[string]string{
	"云健(男·播音)": "zh-CN-YunyangNeural",
	"云希(男·沉稳)": "zh-CN-YunxiNeural",
	"晓晓(女·亲和)": "zh-CN-XiaoxiaoNeural",
	"晓伊(女·温柔)": "zh-CN-XiaoyiNeural",
}

// NormalizeVoice 音色别名→音色ID
func NormalizeVoice(v string) string {
	if v == "" {
		return "zh-CN-YunyangNeural"
	}
	if id, ok := EdgeVoices[v]; ok {
		return id
	}
	if !strings.Contains(v, "-") {
		return "zh-CN-YunyangNeural"
	}
	return v
}

// Synthesize 合成文本到 mp3 文件（exec 不经 shell，中文参数安全）
func (e *EdgeTTS) Synthesize(ctx context.Context, text, voice, out string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("合成文本为空")
	}
	bin := e.Bin
	if bin == "" {
		bin = "edge-tts"
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	args := []string{"--voice", NormalizeVoice(voice), "--text", text, "--write-media", out}
	if b, err := wexec.Command(ctx, bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("edge-tts 合成失败: %v\n%s", err, tail(b, 1000))
	}
	if _, err := os.Stat(out); err != nil {
		return fmt.Errorf("edge-tts 未产出音频文件: %w", err)
	}
	return nil
}

// FitAudioToDuration 把音轨时长对齐到目标：过长 atempo 压缩（≤1.35x，仍超则截断），过短补静音
// in/out 为 wav（无损处理链）
func (f *FFMPEG) FitAudioToDuration(ctx context.Context, in, out string, target float64) error {
	dur, err := f.ProbeDuration(ctx, in)
	if err != nil {
		return fmt.Errorf("探测解说音轨时长失败: %w", err)
	}
	if dur < 0.5 || target < 0.5 {
		return fmt.Errorf("音轨或目标时长异常: dur=%.2f target=%.2f", dur, target)
	}
	af := "apad"
	if dur > target*1.02 {
		tempo := dur / target
		if tempo > 1.35 {
			tempo = 1.35 // 变速上限，超出部分截断
		}
		af = "atempo=" + strconv.FormatFloat(tempo, 'f', 4, 64) + ",apad"
	}
	if dur < target {
		// 短了补静音即可
	} else if dur >= target*0.98 && dur <= target*1.02 {
		// 基本一致，仅补齐微小误差
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-af", af,
		"-t", strconv.FormatFloat(target, 'f', 3, 64),
		"-c:a", "pcm_s16le",
		out,
	}
	if b, err := wexec.Command(ctx, f.Bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("音轨时长对齐失败: %v\n%s", err, tail(b, 1000))
	}
	return nil
}

// ReplaceAudio 用新音轨替换视频原声（视频流直拷，保留已烧录字幕画质）
// keepOriginalAudio=true 时把原声压到 15% 作背景混入
func (f *FFMPEG) ReplaceAudio(ctx context.Context, video, audioWav, out string, keepOriginalAudio bool) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if keepOriginalAudio {
		args = append(args,
			"-i", video, "-i", audioWav,
			"-filter_complex", "[0:a]volume=0.15[bg];[bg][1:a]amix=inputs=2:duration=first:dropout_transition=0[a]",
			"-map", "0:v", "-map", "[a]",
		)
	} else {
		args = append(args,
			"-i", video, "-i", audioWav,
			"-map", "0:v", "-map", "1:a",
		)
	}
	args = append(args,
		"-c:v", "copy",
		"-c:a", "aac", "-b:a", "160k",
		"-shortest",
		out,
	)
	if b, err := wexec.Command(ctx, f.Bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("替换音轨失败: %v\n%s", err, tail(b, 1500))
	}
	return nil
}
