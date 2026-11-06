package gate

import (
	"testing"

	"chopperbot/internal/config"
)

// TestDecisionLogAppendAndHuman 决策日志：追加 → 人工回填写回同一条 → Latest 可查；
// 回填覆盖 final（approved→pass / rejected→human-rejected）——每条记录即训练样本
func TestDecisionLogAppendAndHuman(t *testing.T) {
	log, err := OpenDecisionLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d1 := &Decision{
		ClipID: "j1/clips/clip_01.mp4", SessionID: "room1_20261025",
		TRange: [2]float64{60, 90},
		Gate1:  &Gate1{Status: "dirty", Frames: 4, Hits: []Gate1Hit{{T: 62, Rect: [4]int{260, 10, 50, 30}, Kind: "watermark", Conf: 0.93}}},
		Gate2:  &Gate2{Status: "pass", BaselineVer: 2},
		Gate3:  &Gate3{Status: "pass", Score: 72, Threshold: 60},
		Gate4:  &Gate4{Human: "pending"},
		Final:  "would-reject", Pass: true, CfgVer: "abcd1234",
	}
	d2 := &Decision{ClipID: "j1/clips/clip_02.mp4", Final: "pass", Pass: true,
		Gate4: &Gate4{Human: "pending"}}
	if err := log.Append(d1); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(d2); err != nil {
		t.Fatal(err)
	}

	// 人工毙掉：写回同一条，final 覆盖
	if err := log.RecordHuman("j1/clips/clip_01.mp4", "rejected", "console"); err != nil {
		t.Fatal(err)
	}
	got, err := log.Latest("j1/clips/clip_01.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Gate4.Human != "rejected" || got.Gate4.Operator != "console" || got.Gate4.At == 0 {
		t.Fatalf("人工回填应写回同一条： %+v", got.Gate4)
	}
	if got.Final != "human-rejected" {
		t.Fatalf("毙掉后 final 应为 human-rejected，得到 %s", got.Final)
	}
	// 四关详情不丢（训练样本完整性）
	if got.Gate1 == nil || got.Gate1.Status != "dirty" || len(got.Gate1.Hits) != 1 || got.Gate2.BaselineVer != 2 {
		t.Fatalf("回填不应破坏四关详情：%+v %+v", got.Gate1, got.Gate2)
	}
	// 另一条不受影响
	other, _ := log.Latest("j1/clips/clip_02.mp4")
	if other.Final != "pass" || other.Gate4.Human != "pending" {
		t.Fatalf("回填不应影响其他记录：%+v", other)
	}
	// 人工通过
	if err := log.RecordHuman("j1/clips/clip_02.mp4", "approved", "console"); err != nil {
		t.Fatal(err)
	}
	if got, _ := log.Latest("j1/clips/clip_02.mp4"); got.Final != "pass" || got.Gate4.Human != "approved" {
		t.Fatalf("approved 应回写，得到 %+v", got)
	}
	// 不存在的记录：明确报错（不允许静默丢样本）
	if err := log.RecordHuman("nope.mp4", "approved", "x"); !IsNotFound(err) {
		t.Fatalf("不存在记录应报 NotFound，得到 %v", err)
	}
}

// TestOCRAvailable OCR 引擎可用性：未配置=不可用（关卡 1b 自动跳过的依据）
func TestOCRAvailable(t *testing.T) {
	if (&OCR{}).Available() {
		t.Fatal("未配置引擎不应可用")
	}
	if (&OCR{Bin: "definitely-no-such-ocr-binary-xyz"}).Available() {
		t.Fatal("不存在的二进制不应可用")
	}
}

// TestNormalizeOCR OCR 输出清洗：全角转半角、压空白
func TestNormalizeOCR(t *testing.T) {
	in := "  广告\r\n\r\n　直播返利：\r\n马上看 "
	want := "广告 直播返利: 马上看"
	if got := normalizeOCR(in); got != want {
		t.Fatalf("清洗不符：got=%q want=%q", got, want)
	}
}

// TestSafeCropParse crop 处置的配置解析容错（非法格式在 cropClip 报错，
// 决策走 fail-closed——不配置却选了 crop 是配置错误，宁可不发）
func TestSafeCropParse(t *testing.T) {
	_, err := cropClip(nil, Input{ClipPath: "x.mp4"}, "1,2,3")
	if err == nil {
		t.Fatal("缺一段应报错")
	}
	_, err = cropClip(nil, Input{ClipPath: "x.mp4"}, "1,2,a,4")
	if err == nil {
		t.Fatal("非数字应报错")
	}
	_, err = cropClip(nil, Input{ClipPath: "x.mp4"}, "")
	if err == nil {
		t.Fatal("空配置选 crop 处置应报错（fail-closed）")
	}
}

// TestCfgRoomNil 防御：Rooms 未初始化时 nil map 访问安全
func TestCfgRoomNil(t *testing.T) {
	cfg := &config.PublishGate{} // Rooms 为 nil
	g1 := gate1Clean(nil, cfg, Input{RoomID: "r1"})
	if g1.Status != "skipped" {
		t.Fatalf("nil Rooms 应 skipped，得到 %s", g1.Status)
	}
}
