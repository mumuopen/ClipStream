package gate

import (
	"context"
	"image"
	"image/png"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"chopperbot/internal/config"
	"chopperbot/internal/wexec"
)

// 本文件：ffmpeg 真链路集成测试（抽帧/干净度命中/crop 处置/fail-closed）。
// 无 ffmpeg 的环境自动跳过（CI/裸机），有则真跑——本地与服务器都有 ffmpeg。

func ffmpegBin() (string, bool) {
	for _, p := range []string{"ffmpeg", "ffmpeg.exe",
		filepath.Join("engines", "ffmpeg.exe"), filepath.Join("temp_server", "engines", "ffmpeg.exe")} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// TestExtractSamplesRealFFmpeg 真抽帧：固定间隔 + scene 帧落地、时间近似单调
func TestExtractSamplesRealFFmpeg(t *testing.T) {
	bin, ok := ffmpegBin()
	if !ok {
		t.Skip("本机无 ffmpeg，跳过真链路抽帧测试")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if out, err := run(bin, "-y", "-f", "lavfi", "-i", "testsrc=duration=8:size=320x240:rate=10", src); err != nil {
		t.Fatalf("造测试视频失败: %v：%.120s", err, out)
	}
	frames, err := ExtractSamples(context.Background(), bin, src, filepath.Join(dir, "frames"), 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("8s/30s 间隔应至少 2 帧（固定+scene），得到 %d", len(frames))
	}
	for i, f := range frames {
		if _, err := os.Stat(f.Path); err != nil {
			t.Fatalf("帧文件缺失 %s: %v", f.Path, err)
		}
		if i > 0 && frames[i-1].T > f.T {
			t.Fatalf("帧时间应单调递增：%v", frames)
		}
	}
}

// TestCheckCleanHitRealFFmpeg 完整链路：合成视频 → 自截图当水印模板 → enforce 拦截 reject
// 与 dry-run 记 would-reject；crop 处置真裁切
func TestCheckCleanHitRealFFmpeg(t *testing.T) {
	bin, ok := ffmpegBin()
	if !ok {
		t.Skip("本机无 ffmpeg，跳过真链路干净度测试")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "clip_01.mp4")
	// 画面右上角画个固定"水印"色块（drawbox），其余为动态测试图
	if out, err := run(bin, "-y", "-f", "lavfi",
		"-i", "testsrc=duration=6:size=320x240:rate=10",
		"-vf", "drawbox=x=260:y=10:w=50:h=30:color=red@0.9:t=fill",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", src); err != nil {
		t.Fatalf("造带水印测试视频失败: %v：%.120s", err, out)
	}
	// 从视频抽一帧，截"水印"区当模板（NCC 必然近 1）
	frameDir := filepath.Join(dir, "tplframes")
	if _, err := ExtractSamples(context.Background(), bin, src, frameDir, 6, false); err != nil {
		t.Fatal(err)
	}
	tpl := filepath.Join(dir, "wm_tpl.png")
	if err := cropPNG(regionFirstFrame(t, frameDir), tpl, 255, 5, 50, 30); err != nil {
		t.Fatal(err)
	}

	room := &config.RoomGate{
		Watermarks: []config.RectConf{{Name: "右上角贴片", X: 240, Y: 0, W: 80, H: 60, Tpl: tpl}},
	}
	base := config.Default().PublishGate
	base.Mode = "enforce"
	base.Rooms["r1"] = room
	in := Input{
		ClipID: "j1/clips/clip_01.mp4", ClipPath: src, RoomID: "r1",
		Score: 80, FFmpegBin: bin, FramesDir: filepath.Join(dir, "gframes"),
	}
	d := Check(context.Background(), &base, nil, in)
	if d.Gate1.Status != "dirty" || len(d.Gate1.Hits) == 0 {
		t.Fatalf("自截模板应命中水印：status=%s hits=%+v note=%s", d.Gate1.Status, d.Gate1.Hits, d.Gate1.Note)
	}
	if d.Pass || d.Final != "reject" {
		t.Fatalf("enforce+命中+reject 处置应拦截，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	// dry-run：不拦截，记 would-reject
	base.Mode = "dry-run"
	d = Check(context.Background(), &base, nil, in)
	if !d.Pass || d.Final != "would-reject" {
		t.Fatalf("dry-run 应记 would-reject 且放行，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	// crop 处置：enforce 下真裁切出 .crop.mp4 且放行
	base.Mode = "enforce"
	base.Clean.Action = "crop"
	base.Clean.SafeCrop = "0,0,240,240" // 左侧 240 宽避开水印区
	d = Check(context.Background(), &base, nil, in)
	if !d.Pass || d.Final != "cropped" || d.CroppedPath == "" {
		t.Fatalf("crop 处置应裁切放行，得到 pass=%v final=%s cropped=%s", d.Pass, d.Final, d.CroppedPath)
	}
	if _, err := os.Stat(d.CroppedPath); err != nil {
		t.Fatalf("裁切产物缺失: %v", err)
	}
}

// TestCheckFailClosedRealFFmpeg fail-closed：房间配了检测但 ffmpeg 路径坏 →
// enforce 一律不发；dry-run 记 would-fail-closed
func TestCheckFailClosedRealFFmpeg(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip_01.mp4")
	f, _ := os.Create(src)
	f.Close() // 0 字节假视频：抽帧必失败
	base := config.Default().PublishGate
	base.Mode = "enforce"
	base.Rooms["r1"] = &config.RoomGate{
		Watermarks: []config.RectConf{{Name: "wm", X: 0, Y: 0, W: 10, H: 10, Tpl: "x.png"}},
	}
	in := Input{ClipID: "j1/clips/clip_01.mp4", ClipPath: src, RoomID: "r1",
		FFmpegBin: filepath.Join(dir, "ffmpeg-no-such.exe"), FramesDir: filepath.Join(dir, "fr")}
	d := Check(context.Background(), &base, nil, in)
	if d.Pass || d.Final != "fail-closed" {
		t.Fatalf("工具故障 enforce 应 fail-closed，得到 pass=%v final=%s", d.Pass, d.Final)
	}
	if d.Gate1.Status != "fail" || d.Gate1.Note == "" {
		t.Fatalf("失败原因应落日志：status=%s note=%s", d.Gate1.Status, d.Gate1.Note)
	}
	base.Mode = "dry-run"
	d = Check(context.Background(), &base, nil, in)
	if !d.Pass || d.Final != "would-fail-closed" {
		t.Fatalf("dry-run 应记 would-fail-closed 且不拦，得到 %s", d.Final)
	}
}

// ── 测试辅助 ──

func regionFirstFrame(t *testing.T, frameDir string) string {
	t.Helper()
	entries, err := os.ReadDir(frameDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("无帧可用: %v", err)
	}
	return filepath.Join(frameDir, entries[0].Name())
}

// run 跑外部命令（集成测试造视频用）
func run(bin string, args ...string) (string, error) {
	out, err := wexec.Command(context.Background(), bin, args...).CombinedOutput()
	return string(out), err
}

// cropPNG 从源图截矩形区域存 PNG（测试里做水印模板）
func cropPNG(src, dst string, x, y, w, h int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	if x+w > b.Dx() {
		w = b.Dx() - x
	}
	if y+h > b.Dy() {
		h = b.Dy() - y
	}
	sub := image.NewRGBA(image.Rect(0, 0, w, h))
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			sub.Set(xx, yy, img.At(b.Min.X+x+xx, b.Min.Y+y+yy))
		}
	}
	of, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer of.Close()
	return png.Encode(of, sub)
}
