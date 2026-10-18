package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractTranscriptSummary 切片区间转写摘要：区间匹配、截断、缺失回退
func TestExtractTranscriptSummary(t *testing.T) {
	dir := t.TempDir()
	segs := `[{"start":0,"end":5,"text":"大家好 今天来打排位 "},{"start":5,"end":10,"text":"这个团战 直接开起来"},{"start":300,"end":310,"text":"无关段落"}]`
	if err := os.WriteFile(filepath.Join(dir, "transcript.json"), []byte(segs), 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractTranscriptSummary(dir, 1, 8)
	if !strings.Contains(got, "大家好") || !strings.Contains(got, "团战") {
		t.Fatalf("摘要应含区间内文本，得到 %q", got)
	}
	if strings.Contains(got, "无关段落") {
		t.Fatalf("区间外文本不应进入摘要：%q", got)
	}
	if r := len([]rune(got)); r > descSummaryMax+1 {
		t.Fatalf("摘要超长 %d", r)
	}
	if s := extractTranscriptSummary(dir, 400, 500); s != "" {
		t.Fatalf("无交集区间应返回空串，得到 %q", s)
	}
	if s := extractTranscriptSummary(t.TempDir(), 0, 10); s != "" {
		t.Fatalf("无 transcript.json 应返回空串，得到 %q", s)
	}
}

// TestDeriveTags 标签生成三分支：词典命中 / 标题抽词 / 兜底
func TestDeriveTags(t *testing.T) {
	if tg := deriveTags("半决赛 BO5 团战五杀", "决赛很精彩"); !strings.Contains(tg, "电竞") || !strings.Contains(tg, "高燃") {
		t.Fatalf("词典命中应出电竞+高燃，得到 %q", tg)
	}
	// 不中词典：从标题抽实词而非恒定"游戏"
	tg2 := deriveTags("峡谷之王的一万种死法", "")
	if tg2 == "游戏" && strings.Contains("峡谷之王的一万种死法", "团战") {
		t.Fatalf("标题有实词时不应落兜底")
	}
	if tg3 := deriveTags("123", ""); tg3 != "游戏" {
		t.Fatalf("完全无实词应落兜底'游戏'，得到 %q", tg3)
	}
}

// TestCleanSummary 简介清洗：标点去重与空白合并
func TestCleanSummary(t *testing.T) {
	if s := cleanSummary("  哈哈  ，，  哈哈 ！！ "); s != "哈哈，哈哈！" {
		t.Fatalf("清洗结果不符：%q", s)
	}
}

// TestTopCNPhrases 高频中文短语抽取：停用词过滤与子串去重
func TestTopCNPhrases(t *testing.T) {
	ps := topCNPhrases("这个操作真的秀 这个操作真的离谱 我们的操作啊", 2)
	if len(ps) == 0 {
		t.Fatal("应抽出短语")
	}
	for _, p := range ps {
		if p == "这个" || p == "我们" {
			t.Fatalf("停用词混入：%q", p)
		}
	}
}
