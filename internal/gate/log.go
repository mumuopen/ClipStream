package gate

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件：决策日志（评测集地基）——每条候选片段无论通过与否写一条 JSONL。
// 设计标准（任务书）：字段按"以后每条记录都能直接当一条训练样本"设计；
// 人工回填必须写回同一条记录（按 clip_id 定位重写该行），不允许另开日志。

// Gate1 关卡 1 干净度详情
type Gate1 struct {
	Status string  `json:"status"` // clean | dirty | fail | skipped
	Frames int     `json:"frames"`  // 采样帧数
	Hits   []Gate1Hit `json:"hits,omitempty"`
	Note   string  `json:"note,omitempty"` // skipped/fail 的原因（OCR 未配、房间未配、抽帧失败…）
}

// Gate1Hit 单次命中：时间点（近似）、区域、类型、置信度
type Gate1Hit struct {
	T    float64 `json:"t"`
	Rect [4]int  `json:"rect"` // x,y,w,h
	Kind string  `json:"kind"` // watermark（模板匹配）| ad_ocr（广告文字）
	Name string  `json:"name"` // 配置的展示名
	Conf float64 `json:"conf"` // NCC 峰值 / OCR 命中恒 1.0
	Text string  `json:"text,omitempty"` // OCR 命中词所在全文片段
}

// Gate2 关卡 2 风格基线与漂移（切片侧读场级结论；建基线/比对在 daemon）
type Gate2 struct {
	Status      string `json:"status"` // pass | drift | skipped
	Drifted     bool   `json:"drifted"`
	BaselineVer int    `json:"baseline_ver"`
	Note        string `json:"note,omitempty"`
}

// Gate3 关卡 3 高光分阈值（接现有 LLM Score）
type Gate3 struct {
	Status    string  `json:"status"` // pass | blocked | unscored
	Score     float64 `json:"score"`
	Threshold int     `json:"threshold"`
	Note      string  `json:"note,omitempty"` // unscored：等分切片无 LLM 评分
}

// Gate4 关卡 4 人工确认（审核台操作回填：approved=通过，rejected=毙掉）
type Gate4 struct {
	Human    string `json:"human"` // pending | approved | rejected
	At       int64  `json:"at"`
	Operator string `json:"operator,omitempty"`
}

// Decision 一条候选切片的完整体检记录（= 一条未来训练样本）
type Decision struct {
	ClipID      string `json:"clip_id"`        // "<jobID>/clips/clip_03.mp4"（唯一键）
	SessionID   string `json:"live_session_id"` // 会话目录名（无直播来源为空）
	TRange      [2]float64 `json:"t_range"`     // 片段在源视频的时间区间
	BaselineVer int    `json:"baseline_ver"`
	Gate1 *Gate1 `json:"gate1"`
	Gate2 *Gate2 `json:"gate2"`
	Gate3 *Gate3 `json:"gate3"`
	Gate4 *Gate4 `json:"gate4"`
	// 最终处置：pass | reject | cropped | manual | fail-closed（enforce）
	// dry-run：pass | would-reject | would-cropped | would-manual | would-fail-closed
	Final string `json:"final"`
	Pass  bool   `json:"pass"` // enforce 下的放行结论（dry-run 恒 true=不拦截）
	CroppedPath string `json:"cropped_path,omitempty"` // crop 处置后的新素材路径
	CfgVer string `json:"cfg_ver"` // 配置版本（publish_gate 序列化 hash 前 8 位）
	CostMs int64 `json:"cost_ms"`
	At     int64 `json:"at"`
}

// DecisionLog 决策日志（JSONL 追加 + 按 clip_id 回填）
type DecisionLog struct {
	path string
	mu   sync.Mutex
}

// OpenDecisionLog 决策日志落盘位置：<dataDir>/gate/decisions.jsonl
func OpenDecisionLog(dataDir string) (*DecisionLog, error) {
	dir := filepath.Join(dataDir, "gate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DecisionLog{path: filepath.Join(dir, "decisions.jsonl")}, nil
}

// Append 追加一条决策记录（每条候选片段一条，无论通过与否）
func (l *DecisionLog) Append(d *Decision) error {
	if d == nil {
		return nil
	}
	if d.At == 0 {
		d.At = time.Now().Unix()
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// RecordHuman 人工回填：写回同一条记录（按 clip_id 定位重写该行）。
// action=approved|rejected；operator 标记操作来源（console/CLI）。
// 找不到对应记录时返回明确错误（体检门未跑过人工操作不该发生——防静默丢样本）
func (l *DecisionLog) RecordHuman(clipID, action, operator string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	found := false
	now := time.Now().Unix()
	for i, ln := range lines {
		if !strings.Contains(ln, `"clip_id":"`+clipID+`"`) {
			continue
		}
		var d Decision
		if err := json.Unmarshal([]byte(ln), &d); err != nil || d.ClipID != clipID {
			continue
		}
		if d.Gate4 == nil {
			d.Gate4 = &Gate4{}
		}
		d.Gate4.Human = action
		d.Gate4.At = now
		d.Gate4.Operator = operator
		// 人工最终处置覆盖：approved → final=pass（人工放行），rejected → final=human-rejected
		if action == "approved" {
			d.Final = "pass"
		} else {
			d.Final = "human-rejected"
		}
		b, err := json.Marshal(&d)
		if err != nil {
			return err
		}
		lines[i] = string(b)
		found = true
	}
	if !found {
		return errRecordNotFound(clipID)
	}
	// 原子重写：先写临时文件再替换（回填不产生半行损坏）
	tmp := l.path + ".tmp"
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Latest 按 clip_id 查最近一条记录（体检单展示/调试用）
func (l *DecisionLog) Latest(clipID string) (*Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last *Decision
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20) // 记录可达 1MB（命中详情+全文）
	for sc.Scan() {
		var d Decision
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.ClipID == clipID {
			last = &d
		}
	}
	if last == nil {
		return nil, errRecordNotFound(clipID)
	}
	return last, sc.Err()
}

// RecordHumanByPrefix 任务级回填（审核台"不采用"=整任务丢弃：该任务全部切片标毙）。
// 只覆盖 human 仍为 pending 的记录——已有人工结论（approved/rejected）的不翻案，
// 防止删已处理任务时把矛盾结论写进训练样本。返回回填条数
func (l *DecisionLog) RecordHumanByPrefix(clipPrefix, action, operator string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(raw), "\n")
	now := time.Now().Unix()
	n := 0
	for i, ln := range lines {
		if !strings.Contains(ln, `"clip_id":"`+clipPrefix) {
			continue
		}
		var d Decision
		if err := json.Unmarshal([]byte(ln), &d); err != nil || !strings.HasPrefix(d.ClipID, clipPrefix) {
			continue
		}
		if d.Gate4 != nil && d.Gate4.Human != "pending" {
			continue // 已有人工结论，不翻案
		}
		if d.Gate4 == nil {
			d.Gate4 = &Gate4{}
		}
		d.Gate4.Human, d.Gate4.At, d.Gate4.Operator = action, now, operator
		if action == "approved" {
			d.Final = "pass"
		} else {
			d.Final = "human-rejected"
		}
		b, err := json.Marshal(&d)
		if err != nil {
			return n, err
		}
		lines[i] = string(b)
		n++
	}
	if n == 0 {
		return 0, nil // 无体检记录的任务（开关关闭期间）：删除不回填，正常
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return n, err
	}
	return n, os.Rename(tmp, l.path)
}

// ListTail 尾部 N 条决策记录（时间序，最新在后）——体检报告/审核台列表用
func (l *DecisionLog) ListTail(n int) ([]*Decision, error) {
	if n <= 0 {
		n = 100
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []*Decision
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var d Decision
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.ClipID != "" {
			all = append(all, &d)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

func errRecordNotFound(clipID string) error {
	return &notFoundError{clipID: clipID}
}

type notFoundError struct{ clipID string }

func (e *notFoundError) Error() string {
	return "决策日志中未找到该切片的体检记录: " + e.clipID
}

// IsNotFound notFoundError 判定（server 层 404 映射用）
func IsNotFound(err error) bool {
	_, ok := err.(*notFoundError)
	return ok
}
