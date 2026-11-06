package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPublishGateDefaults 体检门默认值：交付默认 enabled=true + dry-run（只记日志不拦截），
// 阈值齐备（间隔 30s / 超时 120s / pHash 12 / 色调 0.35 / 尺寸 0.9 / 分数 60）
func TestPublishGateDefaults(t *testing.T) {
	c := Default()
	g := c.PublishGate
	if !g.Enabled || g.Mode != "dry-run" {
		t.Fatalf("交付默认应为 enabled=true + dry-run，得到 enabled=%v mode=%q", g.Enabled, g.Mode)
	}
	if g.SampleIntervalSec != 30 || g.ClipTimeoutSec != 120 || !g.SceneDetect {
		t.Fatalf("抽帧间隔/超时/场景采样默认值不符：%d/%d/%v", g.SampleIntervalSec, g.ClipTimeoutSec, g.SceneDetect)
	}
	if g.Style.HashThresh != 12 || g.Style.ColorThresh != 0.35 || g.Style.SizeThresh != 0.9 {
		t.Fatalf("风格阈值默认值不符：%+v", g.Style)
	}
	if g.MinScore != 60 || g.RejectUnscored {
		t.Fatalf("分数闸默认应 60 且不拦等分切片，得到 %d/%v", g.MinScore, g.RejectUnscored)
	}
	if g.Clean.Action != "reject" || g.Clean.OCRLang != "chi_sim+eng" {
		t.Fatalf("干净度默认处置应 reject + chi_sim+eng：%+v", g.Clean)
	}
}

// TestPublishGateYAMLRoundTrip 配置往返：手写 yaml（含房间级水印/广告区）→ 解析 → 关键字段不丢
func TestPublishGateYAMLRoundTrip(t *testing.T) {
	src := `
publish_gate:
  enabled: false
  mode: enforce
  sample_interval_sec: 15
  clip_timeout_sec: 60
  min_score: 75
  reject_unscored: true
  clean:
    ocr_bin: ./engines/tesseract.exe
    ocr_lang: chi_sim
    action: crop
    safe_crop: "0,0,1280,60"
  style:
    hash_thresh: 8
    color_thresh: 0.2
    size_thresh: 0.95
  rooms:
    "660000":
      watermarks:
        - name: 右上角贴片
          x: 1000
          y: 20
          w: 260
          h: 80
          tpl: ./configs/wm_660000.png
      ad_regions:
        - name: 底部广告条
          x: 0
          y: 900
          w: 1280
          h: 60
          keywords: [广告, 赞助]
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	g := c.PublishGate
	if g.Enabled || g.Mode != "enforce" || g.SampleIntervalSec != 15 || g.ClipTimeoutSec != 60 {
		t.Fatalf("顶层字段解析不符：%+v", g)
	}
	if g.MinScore != 75 || !g.RejectUnscored {
		t.Fatalf("分数闸解析不符：%d/%v", g.MinScore, g.RejectUnscored)
	}
	if g.Clean.OCRBin != "./engines/tesseract.exe" || g.Clean.Action != "crop" || g.Clean.SafeCrop != "0,0,1280,60" {
		t.Fatalf("干净度解析不符：%+v", g.Clean)
	}
	if g.Style.HashThresh != 8 {
		t.Fatalf("风格阈值解析不符：%d", g.Style.HashThresh)
	}
	rg := g.Rooms["660000"]
	if rg == nil || len(rg.Watermarks) != 1 || len(rg.AdRegions) != 1 {
		t.Fatalf("房间配置解析不符：%+v", rg)
	}
	wm := rg.Watermarks[0]
	if wm.X != 1000 || wm.Y != 20 || wm.W != 260 || wm.H != 80 || wm.Tpl != "./configs/wm_660000.png" {
		t.Fatalf("水印矩形解析不符：%+v", wm)
	}
	if len(rg.AdRegions[0].Keywords) != 2 {
		t.Fatalf("广告关键词解析不符：%v", rg.AdRegions[0].Keywords)
	}
}

// TestPublishGateSaveKeepsShape Save 全量序列化不丢 publish_gate 段（旧配置文件升级后
// 重写不塌缩）
func TestPublishGateSaveKeepsShape(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	cfg := Default()
	cfg.PublishGate.Rooms["testroom"] = &RoomGate{
		Watermarks: []RectConf{{Name: "wm", X: 1, Y: 2, W: 3, H: 4, Tpl: "t.png"}},
	}
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	rg := c2.PublishGate.Rooms["testroom"]
	if rg == nil || len(rg.Watermarks) != 1 || rg.Watermarks[0].X != 1 {
		t.Fatal("Save/Load 往返丢了房间水印配置")
	}
	if !strings.Contains(readFile(t, p), "publish_gate:") {
		t.Fatal("序列化输出应包含 publish_gate 段")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
