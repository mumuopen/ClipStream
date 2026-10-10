// Package engine 外挂引擎适配层：平台本体只编排，重活全在外部进程/API
package engine

import (
	"context"
	"errors"
)

// ErrUnavailable 引擎不可用（未安装/未配置），调用方应优雅降级
var ErrUnavailable = errors.New("engine unavailable")

// Segment 转写片段（带时间戳）
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// ASR 语音转写引擎接口
type ASR interface {
	Transcribe(ctx context.Context, media, workDir string) ([]Segment, error)
}

// Clip 高光切片候选
type Clip struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
	Score float64 `json:"score"`
}