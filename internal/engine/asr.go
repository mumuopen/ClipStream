package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// WhisperTranscriber whisper.cpp 的 whisper-cli 引擎
// 未配置 binary/model 时返回 ErrUnavailable，管线优雅降级
type WhisperTranscriber struct {
	Bin   string
	Model string
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
	// whisper-cli -m model -f media -oj -o <workDir>/whisper
	// 输出 JSON 文件：<workDir>/whisper.json
	args := []string{"-m", w.Model, "-f", media, "-oj", "-o", filepath.Join(workDir, "whisper")}
	if b, err := exec.CommandContext(ctx, w.Bin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("whisper 转写失败: %v\n%s", err, tail(b, 2000))
	}
	data, err := os.ReadFile(filepath.Join(workDir, "whisper.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 whisper 输出失败: %w", err)
	}
	var out struct {
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析 whisper 输出失败: %w", err)
	}
	segs := make([]Segment, 0, len(out.Segments))
	for _, s := range out.Segments {
		segs = append(segs, Segment{Start: s.Start, End: s.End, Text: s.Text})
	}
	return segs, nil
}

// NoopASR 无引擎时的兜底（避免空指针）
type NoopASR struct{}

func (NoopASR) Transcribe(_ context.Context, _, _ string) ([]Segment, error) {
	return nil, fmt.Errorf("%w: 未配置 ASR 引擎", ErrUnavailable)
}