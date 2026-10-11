package publish

import (
	"encoding/json"
	"fmt"
)

// DubRequest 配音解说任务请求
type DubRequest struct {
	SourceJob string `json:"source_job"` // 源剪辑任务 ID
	Clip      string `json:"clip"`       // 切片相对路径（可传原版或 _sub 字幕版）
	Voice     string `json:"voice"`      // edge-tts 音色（别名或 ID），默认云健
	KeepAudio bool   `json:"keep_audio"` // true=保留原声做背景（压到15%）；false=纯解说
}

// ParseDubRequest 解析并校验配音请求
func ParseDubRequest(s string) (*DubRequest, error) {
	var r DubRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("配音请求 JSON 解析失败: %w", err)
	}
	if r.Clip == "" {
		return nil, fmt.Errorf("配音请求缺少 clip")
	}
	return &r, nil
}

// DubResult 配音任务结果
type DubResult struct {
	SourceJob string `json:"source_job"`
	Original  string `json:"original"`   // 输入切片
	Dubbed    string `json:"dubbed"`     // 配音成品（可发布）
	Cover     string `json:"cover"`      // 封面（沿用）
	Title     string `json:"title"`      // 切片标题
	Voice     string `json:"voice"`      // 实际音色 ID
	ScriptLen int    `json:"script_len"` // 解说稿字数
}
