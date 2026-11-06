package gate

import (
	"image"
	"image/color"
	"testing"

	"chopperbot/internal/config"
)

// mkGray 合成灰度渐变测试图（可加偏移扰动）
func mkGray(w, h int, off float64) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8(int(float64(x)*3+float64(y)*2+off) % 256)})
		}
	}
	return g
}

// mkNoise 确定性伪随机噪声图：背景块间互不相关（NCC 测试用——
// 渐变背景自相似会让任意背景块互匹配，掩盖真实匹配行为）
func mkNoise(w, h, seed int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	s := uint32(seed)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s = s*1664525 + 1013904223
			g.SetGray(x, y, color.Gray{Y: uint8(s >> 24)})
		}
	}
	return g
}

// TestPHashDistance 感知哈希：同图距离 0；轻微扰动距离小；完全不同内容距离大
func TestPHashDistance(t *testing.T) {
	a := mkGray(320, 240, 0)
	b := mkGray(320, 240, 30) // 轻微灰度偏移：结构不变，pHash 应稳定
	// 完全不同内容：两张不同种子的噪声图（高频信息池化后仍差异显著，期望距离 ~32）
	na, nb := mkNoise(320, 240, 1), mkNoise(320, 240, 2)
	fa, fb, fna, fnb := FeaturesOf(a), FeaturesOf(b), FeaturesOf(na), FeaturesOf(nb)
	if d := HashDistance(fa.PHash, fa.PHash); d != 0 {
		t.Fatalf("同图距离应为 0，得到 %d", d)
	}
	if d := HashDistance(fa.PHash, fb.PHash); d > 6 {
		t.Fatalf("轻微扰动距离应 ≤6（pHash 稳定性），得到 %d", d)
	}
	if d := HashDistance(fna.PHash, fnb.PHash); d < 10 {
		t.Fatalf("完全不同内容距离应显著（≥10），得到 %d", d)
	}
	if d := HashDistance(fna.PHash, fna.PHash); d != 0 {
		t.Fatal("噪声图自比对应为 0")
	}
}

// TestColorAndSizeFeature 色调距离与分辨率比
func TestColorAndSizeFeature(t *testing.T) {
	fa := FeaturesOf(mkGray(320, 240, 0))
	fb := FeaturesOf(mkGray(320, 240, 30))
	if d := ColorDistance(fa.ColorHist, fb.ColorHist); d > 0.2 {
		t.Fatalf("同分布微偏移色调距离应很小，得到 %f", d)
	}
	same := FeaturesOf(mkGray(640, 480, 0))
	if d := ColorDistance(fa.ColorHist, same.ColorHist); d > 0.2 {
		t.Fatalf("渐变图缩放不应改变色调分布，得到 %f", d)
	}
	if r := SizeRatio(fa, fb); r != 1 {
		t.Fatalf("同分辨率比应为 1，得到 %f", r)
	}
	big := FeaturesOf(mkGray(1280, 720, 0))
	if r := SizeRatio(fa, big); r >= 0.9 {
		t.Fatalf("320x240 vs 1280x720 相似比应 <0.9，得到 %f", r)
	}
	if r := SizeRatio(big, big); r != 1 {
		t.Fatal("同尺寸比应为 1")
	}
}

// TestMatchInRegion 模板匹配：从帧上截的模板必命中且峰值高；无关模板不命中。
// 背景用噪声图（渐变背景自相似会让任意背景块互匹配，测不出真实行为）
func TestMatchInRegion(t *testing.T) {
	frame := mkNoise(320, 240, 42)
	// 在 (120, 60) 处放一块特征图案（确定性纹理，与背景噪声不相似）
	for y := 60; y < 100; y++ {
		for x := 120; x < 180; x++ {
			frame.SetGray(x, y, color.Gray{Y: uint8((x*y/7 + 13) % 256)})
		}
	}
	// 模板=该图案本身
	tpl := image.NewGray(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			tpl.SetGray(x, y, frame.GrayAt(120+x, 60+y))
		}
	}
	peak, at := MatchInRegion(frame, tpl, image.Rect(0, 0, 320, 240))
	if peak < 0.98 {
		t.Fatalf("自截图模板 NCC 峰值应 ≥0.98，得到 %f @%v", peak, at)
	}
	if at.X != 120 || at.Y != 60 {
		t.Fatalf("命中位置应为 (120,60)，得到 %v", at)
	}
	// 无关模板（背景噪声块，搜索窗避开特征图案）峰值应低
	far := image.NewGray(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			far.SetGray(x, y, frame.GrayAt(10+x, 10+y))
		}
	}
	peak2, _ := MatchInRegion(frame, far, image.Rect(100, 40, 320, 240))
	if peak2 >= watermarkNCCThresh {
		t.Fatalf("无关模板不应命中阈值，得到 %f", peak2)
	}
	// 纯色模板无信息量：返回 0 不 panic
	flat := image.NewGray(image.Rect(0, 0, 10, 10))
	if p, _ := MatchInRegion(frame, flat, image.Rect(0, 0, 320, 240)); p != 0 {
		t.Fatal("纯色模板应返回 0")
	}
}

// TestGate3Score 分数闸：pass/blocked/unscored + reject_unscored
func TestGate3Score(t *testing.T) {
	cfg := &config.PublishGate{MinScore: 60}
	if g := gate3Score(cfg, 85); g.Status != "pass" {
		t.Fatalf("85 分应过，得到 %s", g.Status)
	}
	if g := gate3Score(cfg, 40); g.Status != "blocked" {
		t.Fatalf("40 分应拦，得到 %s", g.Status)
	}
	g := gate3Score(cfg, 0)
	if g.Status != "unscored" {
		t.Fatalf("等分切片默认应 unscored（不拦），得到 %s", g.Status)
	}
	cfg.RejectUnscored = true
	if g := gate3Score(cfg, 0); g.Status != "blocked" {
		t.Fatalf("reject_unscored=true 时等分切片应拦，得到 %s", g.Status)
	}
}

// fakeBaseline 测试用基线读取器
type fakeBaseline struct {
	drifted, ok bool
	ver         int
}

func (f fakeBaseline) RoomStyleState(string) (bool, int, bool) { return f.drifted, f.ver, f.ok }

// TestGate2Style 基线漂移闸：无来源/无基线/漂移/正常
func TestGate2Style(t *testing.T) {
	cfg := &config.PublishGate{}
	if g := gate2Style(cfg, nil, "r1"); g.Status != "skipped" {
		t.Fatal("无基线来源应 skipped")
	}
	if g := gate2Style(cfg, fakeBaseline{}, ""); g.Status != "skipped" {
		t.Fatal("无房间应 skipped")
	}
	if g := gate2Style(cfg, fakeBaseline{ok: false}, "r1"); g.Status != "skipped" {
		t.Fatal("无基线记录应 skipped")
	}
	g := gate2Style(cfg, fakeBaseline{ok: true, drifted: true, ver: 3}, "r1")
	if g.Status != "drift" || !g.Drifted || g.BaselineVer != 3 {
		t.Fatalf("漂移场应 drift + 带版本，得到 %+v", g)
	}
	if g := gate2Style(cfg, fakeBaseline{ok: true, ver: 2}, "r1"); g.Status != "pass" || g.BaselineVer != 2 {
		t.Fatalf("正常场应 pass，得到 %+v", g)
	}
}

// TestSummarize 处置汇总：dirty 按配置处置；fail=拦截；漂移=转人工；低分=剔除
func TestSummarize(t *testing.T) {
	cfg := &config.PublishGate{Clean: config.CleanGate{Action: "reject"}}
	d := &Decision{
		Gate1: &Gate1{Status: "dirty"},
		Gate2: &Gate2{Status: "pass"},
		Gate3: &Gate3{Status: "pass"},
	}
	if b, a := summarize(d, cfg); !b || a != "reject" {
		t.Fatalf("dirty 默认应 reject，得到 %v %s", b, a)
	}
	cfg.Clean.Action = "manual"
	if _, a := summarize(d, cfg); a != "manual" {
		t.Fatalf("配置 manual 应转人工，得到 %s", a)
	}
	d.Gate1 = &Gate1{Status: "fail"}
	if _, a := summarize(d, cfg); a != "fail-closed" {
		t.Fatalf("检测失败应 fail-closed，得到 %s", a)
	}
	d.Gate1 = &Gate1{Status: "clean"}
	d.Gate2 = &Gate2{Status: "drift", Drifted: true}
	if _, a := summarize(d, cfg); a != "manual" {
		t.Fatalf("漂移应转人工，得到 %s", a)
	}
	d.Gate2 = &Gate2{Status: "pass"}
	d.Gate3 = &Gate3{Status: "blocked"}
	if _, a := summarize(d, cfg); a != "reject" {
		t.Fatalf("低分应 reject，得到 %s", a)
	}
	// 全过/仅 skip → 放行
	d.Gate3 = &Gate3{Status: "pass"}
	if b, _ := summarize(d, cfg); b {
		t.Fatal("全过不应拦截")
	}
	d.Gate1 = &Gate1{Status: "skipped"}
	d.Gate3 = &Gate3{Status: "unscored"}
	if b, _ := summarize(d, cfg); b {
		t.Fatal("skip+unscored（默认）不应拦截")
	}
}

// TestCheckDecisionStruct Check 主流程不依赖外部工具的部分：
// 房间未配 + 无基线 + unscored → 全 skipped/unscored → pass（enforce 与 dry-run 同）
func TestCheckDecisionStruct(t *testing.T) {
	cfg := config.Default().PublishGate
	cfg.Mode = "enforce"
	d := Check(nil, &cfg, fakeBaseline{}, Input{
		ClipID: "j1/clips/clip_01.mp4", RoomID: "nosuch", Score: 0,
	})
	if !d.Pass || d.Final != "pass" {
		t.Fatalf("无配置房间+无评分默认应放行，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	if d.Gate1.Status != "skipped" || d.Gate2.Status != "skipped" || d.Gate3.Status != "unscored" {
		t.Fatalf("三关状态应 skipped/skipped/unscored，得到 %s/%s/%s", d.Gate1.Status, d.Gate2.Status, d.Gate3.Status)
	}
	if d.Gate4.Human != "pending" {
		t.Fatalf("关卡 4 初始应 pending，得到 %s", d.Gate4.Human)
	}
	// 漂移 + enforce → 拦截转人工
	cfg.Mode = "enforce"
	d = Check(nil, &cfg, fakeBaseline{ok: true, drifted: true, ver: 2}, Input{
		ClipID: "j1/clips/clip_02.mp4", RoomID: "r1", Score: 90,
	})
	if d.Pass || d.Final != "manual" {
		t.Fatalf("漂移场 enforce 应拦截转人工，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	// dry-run：同输入只记不拦
	cfg.Mode = "dry-run"
	d = Check(nil, &cfg, fakeBaseline{ok: true, drifted: true, ver: 2}, Input{
		ClipID: "j1/clips/clip_03.mp4", RoomID: "r1", Score: 90,
	})
	if !d.Pass || d.Final != "would-manual" {
		t.Fatalf("dry-run 应记 would-manual 且不拦，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	// 低分 + dry-run → would-reject；enforce → reject
	d = Check(nil, &cfg, nil, Input{ClipID: "j1/clips/clip_04.mp4", RoomID: "", Score: 30})
	if !d.Pass || d.Final != "would-reject" {
		t.Fatalf("dry-run 低分应记 would-reject，得到 %s", d.Final)
	}
	cfg.Mode = "enforce"
	d = Check(nil, &cfg, nil, Input{ClipID: "j1/clips/clip_05.mp4", RoomID: "", Score: 30})
	if d.Pass || d.Final != "reject" {
		t.Fatalf("enforce 低分应拦截，得到 pass=%v final=%s", d.Pass, d.Final)
	}
}

// TestCfgVer 配置版本：不同阈值产出不同版本号（评测样本的可复现性前提）
func TestCfgVer(t *testing.T) {
	a := config.Default().PublishGate
	b := config.Default().PublishGate
	b.MinScore = 75
	if cfgVer(&a) == cfgVer(&b) {
		t.Fatal("配置变化 cfg_ver 应变化")
	}
	if cfgVer(&a) != cfgVer(&a) {
		t.Fatal("同配置 cfg_ver 应稳定")
	}
}
