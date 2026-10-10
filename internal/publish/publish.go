package publish

import (
	"encoding/json"
	"fmt"
)

// Request 发布请求（来自审核台或 CLI，JSON 序列化后作为 publish 任务的 Input）
type Request struct {
	SourceJob string   `json:"source_job"` // 源剪辑任务 ID
	Clip      string   `json:"clip"`       // 切片相对路径，如 clips/clip_01.mp4
	Cover     string   `json:"cover"`      // 封面相对路径（可空）
	Title     string   `json:"title"`
	Desc      string   `json:"desc"`
	Tags      []string `json:"tags"`
	Platforms []string `json:"platforms"` // bilibili / douyin
	Account   string   `json:"account"`   // 账号别名，默认 default
	DryRun    bool     `json:"dry_run"`   // 走到发布按钮前停止并截图
}

// PlatformResult 单平台发布结果
type PlatformResult struct {
	Platform   string `json:"platform"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	Screenshot string `json:"screenshot,omitempty"`
}

// Result 发布任务总结果
type Result struct {
	DryRun  bool             `json:"dry_run,omitempty"`
	Results []PlatformResult `json:"results"`
	AllOK   bool             `json:"all_ok"`
}

// LogFunc 日志回调（接管线日志）
type LogFunc func(format string, args ...any)

// ParseRequest 从 JSON 解析发布请求
func ParseRequest(s string) (*Request, error) {
	var r Request
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("发布请求 JSON 解析失败: %w", err)
	}
	if len(r.Platforms) == 0 {
		return nil, fmt.Errorf("发布请求缺少 platforms")
	}
	if r.Clip == "" {
		return nil, fmt.Errorf("发布请求缺少 clip")
	}
	if r.Account == "" {
		r.Account = "default"
	}
	return &r, nil
}
