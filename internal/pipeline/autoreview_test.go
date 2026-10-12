package pipeline

// 自动审核分级闸门单元测试（M5 挂机无人值守）

import (
	"encoding/json"
	"testing"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
)

func TestAutoReviewGates(t *testing.T) {
	newEnv := func() *Env {
		cfg := &config.Config{}
		cfg.AutoReview = config.AutoReview{
			Enabled:       true,
			MinScore:      60,
			HardMinDurSec: 15,
			HardMaxDurSec: 120,
			BannedWords:   []string{"敏感词"},
		}
		return &Env{Cfg: cfg}
	}

	cases := []struct {
		name    string
		clip    ClipResult
		emptyTr bool
		wantAct string
	}{
		{"高分正常→自动入池", ClipResult{File: "a.mp4", Title: "好切片", Start: 0, End: 60, Score: 85}, false, "auto_pool"},
		{"时长过短→拒发", ClipResult{File: "b.mp4", Title: "太短", Start: 0, End: 10, Score: 90}, false, "reject"},
		{"时长过长→拒发", ClipResult{File: "c.mp4", Title: "太长", Start: 0, End: 200, Score: 90}, false, "reject"},
		{"无评分→留人工", ClipResult{File: "d.mp4", Title: "等分切片", Start: 0, End: 60, Score: 0}, false, "manual"},
		{"低分→留人工", ClipResult{File: "e.mp4", Title: "一般", Start: 0, End: 60, Score: 55}, false, "manual"},
		{"转写空→留人工", ClipResult{File: "f.mp4", Title: "纯音乐", Start: 0, End: 60, Score: 95}, true, "manual"},
		{"敏感词→留人工", ClipResult{File: "g.mp4", Title: "含敏感词的标题", Start: 0, End: 60, Score: 95}, false, "manual"},
		{"恰好达标→入池", ClipResult{File: "h.mp4", Title: "及格线", Start: 0, End: 15, Score: 60}, false, "auto_pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &core.Job{ID: "j-test", Dir: t.TempDir()}
			b, _ := json.Marshal(map[string]any{"clips": []ClipResult{tc.clip}})
			job.SetResult(json.RawMessage(b))
			vs := AutoReviewClips(job, newEnv(), tc.emptyTr, newEnv().Cfg.AutoReview.BannedWords)
			if len(vs) != 1 {
				t.Fatalf("应产出 1 条判定，实际 %d", len(vs))
			}
			if vs[0].Action != tc.wantAct {
				t.Fatalf("动作 %s，期望 %s（原因：%s）", vs[0].Action, tc.wantAct, vs[0].Reason)
			}
		})
	}
}

func TestAutoReviewDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.AutoReview = config.AutoReview{Enabled: false}
	env := &Env{Cfg: cfg}
	job := &core.Job{ID: "j-off", Dir: t.TempDir()}
	b, _ := json.Marshal(map[string]any{"clips": []ClipResult{{File: "x.mp4", Title: "高分", Start: 0, End: 60, Score: 99}}})
	job.SetResult(json.RawMessage(b))
	vs := AutoReviewClips(job, env, false, nil)
	if len(vs) != 1 {
		t.Fatalf("开关关时应照判（调用方决定不应用），实际 %d", len(vs))
	}
	// 应用层开关关 → applyAutoReview 应零动作
	a, m, r := applyAutoReview(job, env)
	if a != 0 || m != 0 || r != 0 {
		t.Fatalf("开关关时 applyAutoReview 应无动作，实际 auto=%d manual=%d reject=%d", a, m, r)
	}
}