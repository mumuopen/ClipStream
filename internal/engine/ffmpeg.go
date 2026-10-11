package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// FFMPEG ffmpeg/ffprobe 引擎
type FFMPEG struct {
	Bin      string
	ProbeBin string
}

func (f *FFMPEG) probeBin() string {
	if f.ProbeBin != "" {
		return f.ProbeBin
	}
	return "ffprobe"
}

// ProbeDuration 获取视频时长（秒）
func (f *FFMPEG) ProbeDuration(ctx context.Context, path string) (float64, error) {
	out, err := exec.CommandContext(ctx, f.probeBin(),
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe 时长解析失败: %s", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// Cut 精剪（重编码，稳妥）：in[start,end] -> out
func (f *FFMPEG) Cut(ctx context.Context, in, out string, start, end float64) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", start),
		"-to", fmt.Sprintf("%.3f", end),
		"-i", in,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "21",
		"-c:a", "aac", "-movflags", "+faststart",
		out,
	}
	if b, err := exec.CommandContext(ctx, f.Bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 切片失败: %v\n%s", err, tail(b, 1500))
	}
	return nil
}

// ExtractFrame 截取 t 秒处一帧存为 jpg
func (f *FFMPEG) ExtractFrame(ctx context.Context, in, out string, t float64) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", t),
		"-i", in,
		"-frames:v", "1", "-q:v", "2",
		out,
	}
	if b, err := exec.CommandContext(ctx, f.Bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 封面提取失败: %v\n%s", err, tail(b, 1500))
	}
	return nil
}

// ExtractAudio16k 抽取 16kHz 单声道 PCM WAV（whisper.cpp v1.9 miniaudio 只认裸音频）
func (f *FFMPEG) ExtractAudio16k(ctx context.Context, in, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-vn", "-ar", "16000", "-ac", "1",
		"-c:a", "pcm_s16le",
		out,
	}
	if b, err := exec.CommandContext(ctx, f.Bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 抽音频失败: %v\n%s", err, tail(b, 1500))
	}
	return nil
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}
