package core

import (
	"encoding/json"
	"time"
)

// JobStatus 任务状态
type JobStatus string

const (
	StatusQueued   JobStatus = "queued"   // 排队
	StatusRunning  JobStatus = "running"  // 执行中
	StatusReview   JobStatus = "review"   // 待人工审核（发布前闸门）
	StatusDone     JobStatus = "done"     // 完成
	StatusFailed   JobStatus = "failed"   // 失败
	StatusCanceled JobStatus = "canceled" // 已取消
)

func (s JobStatus) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Job 一个任务 = 一条管线实例
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`   // clips | live | ...
	Status    JobStatus `json:"status"`
	Stage     string    `json:"stage"`  // 当前阶段名
	Progress  int       `json:"progress"`
	Input     string    `json:"input"`  // URL 或本地文件路径
	Params    string    `json:"params"` // 阶段间传递的键值（JSON）
	Result    string    `json:"result"` // 最终产物描述（JSON）
	Error     string    `json:"error"`
	Log       string    `json:"log"`
	Dir       string    `json:"dir"` // 任务工作目录
	CreatedAt int64     `json:"createdAt"`
	UpdatedAt int64     `json:"updatedAt"`
}

func (j *Job) AppendLog(line string) {
	const cap = 200_000
	if len(j.Log) > cap {
		j.Log = j.Log[len(j.Log)-(cap/2):]
	}
	j.Log += "[" + time.Now().Format("15:04:05") + "] " + line + "\n"
}

func (j *Job) ParamsMap() map[string]string {
	m := map[string]string{}
	if j.Params != "" {
		_ = json.Unmarshal([]byte(j.Params), &m)
	}
	return m
}

func (j *Job) SetParam(k, v string) {
	m := j.ParamsMap()
	m[k] = v
	b, _ := json.Marshal(m)
	j.Params = string(b)
}

func (j *Job) SetResult(v any) {
	b, _ := json.Marshal(v)
	j.Result = string(b)
}