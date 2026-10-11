package publish

import (
	"encoding/json"
	"fmt"
)

// SubtitleRequest 字幕任务请求（对审核台切片加烧录字幕）
type SubtitleRequest struct {
	SourceJob string `json:"source_job"` // 源剪辑任务 ID
	Clip      string `json:"clip"`       // 切片相对路径，如 clips/clip_06.mp4
	Style     string `json:"style"`      // bilingual（原文+中文）| zh（仅中文），默认 bilingual
}

// ParseSubtitleRequest 解析并校验字幕请求
func ParseSubtitleRequest(s string) (*SubtitleRequest, error) {
	var r SubtitleRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("字幕请求 JSON 解析失败: %w", err)
	}
	if r.Clip == "" {
		return nil, fmt.Errorf("字幕请求缺少 clip")
	}
	if r.Style == "" {
		r.Style = "bilingual"
	}
	if r.Style != "bilingual" && r.Style != "zh" {
		return nil, fmt.Errorf("style 仅支持 bilingual / zh，收到 %q", r.Style)
	}
	return &r, nil
}

// SubtitleResult 字幕任务结果（审核台可对该文件发起发布）
type SubtitleResult struct {
	SourceJob string `json:"source_job"`
	Original  string `json:"original"`  // 原切片
	Subtitled string `json:"subtitled"` // 烧字幕后的切片（可发布）
	Cover     string `json:"cover"`     // 封面（沿用原切片）
	Title     string `json:"title"`     // 切片标题
	Style     string `json:"style"`     // bilingual | zh
	Segments  int    `json:"segments"`  // 翻译的转写片段数
}
