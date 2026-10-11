package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WhisperTranscriber whisper.cpp 的 whisper-cli 引擎
// 未配置 binary/model 时返回 ErrUnavailable，管线优雅降级
type WhisperTranscriber struct {
	Bin   string
	Model string
	FF    *FFMPEG // 抽音频用：v1.9 miniaudio 只认裸音频（flac/mp3/ogg/wav），MP4 需先抽 16k 单声道 WAV
}

func (w *WhisperTranscriber) Transcribe(ctx context.Context, media, workDir string) ([]Segment, error) {
	if w.Bin == "" || w.Model == "" {
		return nil, fmt.Errorf("%w: whisper 引擎未配置（binary=%q model=%q）", ErrUnavailable, w.Bin, w.Model)
	}
	if !filepath.IsAbs(w.Bin) {
		if _, err := exec.LookPath(w.Bin); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}

	// v1.9 miniaudio 只认裸音频：先抽 16kHz 单声道 WAV（已经是裸音频且 FF 未接时直接用原文件）
	input := media
	switch strings.ToLower(filepath.Ext(media)) {
	case ".flac", ".mp3", ".ogg", ".wav":
		// 裸音频直接喂
	default:
		if w.FF == nil {
			return nil, fmt.Errorf("whisper 无法直接读取 %s（v1.9 仅支持裸音频），且未接入 ffmpeg 抽音频", filepath.Ext(media))
		}
		wav := filepath.Join(workDir, "audio-16k.wav")
		if err := w.FF.ExtractAudio16k(ctx, media, wav); err != nil {
			return nil, fmt.Errorf("抽取音频失败（MP4 等容器需先转 WAV）: %w", err)
		}
		input = wav
	}

	// whisper.cpp v1.9 参数：-of 输出前缀（无扩展名）；-l auto 自动检测语言（默认 en 会把中文硬当英文转写）；-np 减少日志噪音
	args := []string{
		"-m", w.Model,
		"-f", input,
		"-l", "auto",
		"-oj",
		"-of", filepath.Join(workDir, "whisper"),
		"-np",
	}
	if b, err := exec.CommandContext(ctx, w.Bin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("whisper 转写失败: %v\n%s", err, tail(b, 2000))
	}
	data, err := os.ReadFile(filepath.Join(workDir, "whisper.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 whisper 输出失败: %w", err)
	}
	// v1.9 JSON 结构：transcription[].offsets.from/to（毫秒）+ text
	var out struct {
		Transcription []struct {
			Offsets struct {
				From int64 `json:"from"` // 毫秒
				To   int64 `json:"to"`
			} `json:"offsets"`
			Text string `json:"text"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析 whisper 输出失败: %w", err)
	}
	segs := make([]Segment, 0, len(out.Transcription))
	for _, t := range out.Transcription {
		segs = append(segs, Segment{
			Start: float64(t.Offsets.From) / 1000,
			End:   float64(t.Offsets.To) / 1000,
			Text:  strings.TrimSpace(t.Text),
		})
	}
	return segs, nil
}

// NoopASR 无引擎时的兜底（避免空指针）
type NoopASR struct{}

func (NoopASR) Transcribe(_ context.Context, _, _ string) ([]Segment, error) {
	return nil, fmt.Errorf("%w: 未配置 ASR 引擎", ErrUnavailable)
}
