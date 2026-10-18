package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// 投稿指纹治理（2026-10-19 风控审查）：自动入池条目曾恒定"空简介+固定
// 分区+空标签/兜底同一个词"，+B站搬运检测的经典机器特征组合。本文件在
// 自动入池时从现成数据（切片区间转写 + LLM 标题）生成差异化 desc 与 tags，
// 消除模板化投稿形状。

// descSummaryMax 简介截断长度：B站简介常用 50-200 字，取区间转写拼前 90 字
const descSummaryMax = 90

// extractTranscriptSummary 读产物 transcript.json，拼接切片区间内的转写文本
// 作简介兜底。区间匹配宽容 ±2s（whisper 分段边界与切片切割点不严格对齐）。
// 转写缺失/区间无文本时返回空串（调用方回退其他兜底）。
func extractTranscriptSummary(jobDir string, start, end float64) string {
	b, err := os.ReadFile(filepath.Join(jobDir, "transcript.json"))
	if err != nil {
		return ""
	}
	var segs []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	}
	if json.Unmarshal(b, &segs) != nil {
		return ""
	}
	var sb strings.Builder
	for _, s := range segs {
		if s.End < start-2 || s.Start > end+2 {
			continue // 不相交（含宽容量）
		}
		sb.WriteString(strings.TrimSpace(s.Text))
	}
	out := cleanSummary(sb.String())
	if len([]rune(out)) > descSummaryMax {
		r := []rune(out)
		out = string(r[:descSummaryMax]) + "…"
	}
	return out
}

// cleanSummary 简介清洗：压空白为单空格、连续标点只留首个（whisper 常吐
// 连续标点/空格）；首尾不留空白
func cleanSummary(s string) string {
	var sb strings.Builder
	inSpace := false
	var lastLetter rune = -1
	lastPunct := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		isPunct := strings.ContainsRune("，。！？、,.!?…；;：:", r)
		if isPunct {
			if lastPunct || lastLetter < 0 {
				continue // 标点重复或开头标点：跳过
			}
			if inSpace {
				inSpace = false // 标点前的空格不需要
			}
			sb.WriteRune(r)
			lastPunct = true
			lastLetter = -1
			continue
		}
		if inSpace && lastLetter >= 0 {
			sb.WriteRune(' ') // 空白压成单空格（仅出现在两段文字之间）
		}
		inSpace = false
		lastPunct = false
		lastLetter = r
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

// tagDict 内容标签词典（与控制台 autoTags 同源，勿单边漂移）
var tagDict = map[string][]string{
	"电竞": {"赛区", "入围", "决赛", "半决赛", "BO5", "BO3", "小组赛", "淘汰赛", "赛季", "联赛", "冠军", "MVP"},
	"高燃": {"团战", "开团", "五杀", "逆转", "翻盘", "极限", "反杀", "神级", "炸裂", "高能", "名场面"},
	"搞笑": {"失误", "翻车", "下饭", "离谱", "迷惑", "搞笑", "乐子", "梗", "整活"},
	"教学": {"套路", "攻略", "阵容", "打法", "对线", "运营", "细节", "思路", "揭秘", "技巧"},
}

// deriveTags 生成内容标签：词典命中（标题+转写首段）优先；
// 全不中时从标题抽高频连续中文短语（LLM 爆款标题必含具体内容词，比恒定
// 兜底词"游戏"更像人）；仍无 → "游戏"。
func deriveTags(title, transcriptHead string) string {
	corpus := title + " " + transcriptHead
	var tags []string
	seen := map[string]bool{}
	for tag, words := range tagDict {
		for _, w := range words {
			if strings.Contains(corpus, w) && !seen[tag] {
				tags = append(tags, tag)
				seen[tag] = true
				break
			}
		}
	}
	if len(tags) > 0 {
		return strings.Join(tags[:min(3, len(tags))], ",")
	}
	if ps := topCNPhrases(title, 2); len(ps) > 0 {
		return strings.Join(ps, ",")
	}
	return "游戏"
}

// topCNPhrases 从文本提取频次最高的连续汉字短语（≥2 字）：无分词器的
// 务实近似——按"最长连续汉字段"切词根，再取完整段与 2~4 字滑窗中频次
// 最高者（避免"这个操作"里的虚词黏连优先，短高频段更接近真实搜索词）
func topCNPhrases(s string, n int) []string {
	// 连续汉字段
	var blocks []string
	var cur []rune
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			cur = append(cur, r)
		} else {
			if len(cur) >= 2 {
				blocks = append(blocks, string(cur))
			}
			cur = nil
		}
	}
	if len(cur) >= 2 {
		blocks = append(blocks, string(cur))
	}
	// 停用段（量词/语气/通用词，抽出来无信息量）
	stop := map[string]bool{"这个": true, "那个": true, "什么": true, "怎么": true, "可以": true, "一个": true, "没有": true, "我们": true, "他们": true, "就是": true, "但是": true, "然后": true}

	freq := map[string]int{}
	for _, b := range blocks {
		r := []rune(b)
		if len(r) > 6 { // 超长段滑窗 2~6 字
			for size := 2; size <= 6; size++ {
				for i := 0; i+size <= len(r); i++ {
					freq[string(r[i:i+size])]++
				}
			}
		} else if !stop[b] {
			freq[b]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range freq {
		if v > 0 && len([]rune(k)) >= 2 && !stop[k] {
			list = append(list, kv{k, v})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		// 同频取更长的段（信息量更高）
		return len([]rune(list[i].k)) > len([]rune(list[j].k))
	})
	var out []string
	for _, e := range list {
		dup := false
		for _, o := range out {
			if strings.Contains(o, e.k) { // 子串去重
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, e.k)
		}
		if len(out) >= n {
			break
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
