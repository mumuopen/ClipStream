package live

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"chopperbot/internal/config"
	"chopperbot/internal/gate"
	"chopperbot/internal/store"
	"chopperbot/internal/wexec"
)

// TestStyleVerdict 漂移判定三要素：结构（pHash）/色调/分辨率，任一超阈值即漂移
func TestStyleVerdict(t *testing.T) {
	th := config.StyleGate{HashThresh: 12, ColorThresh: 0.35, SizeThresh: 0.9}
	base := gate.FrameFeatures{W: 1280, H: 720, PHash: 0b10101010101010101010101010101010101010101010101010101010101010}
	// 基线色调：R 通道集中低 bin（4 个 bin 各 0.25，和=1）
	for i := 0; i < 4; i++ {
		base.ColorHist[i] = 0.25
	}
	same := base
	if drifted, why := styleVerdict(base, same, th); drifted {
		t.Fatalf("相同特征不应漂移：%s", why)
	}
	// pHash 结构大改：翻转中间 36 位（汉明距离必然 36）
	diffHash := base
	diffHash.PHash = base.PHash ^ 0x0000000FFFFFFF0
	if drifted, why := styleVerdict(base, diffHash, th); !drifted || why == "" {
		t.Fatalf("结构大改应漂移并给原因，得到 %v %q", drifted, why)
	}
	// 色调直方图整体移位：低 bin 移到高 bin，交集为 0 → 距离 1
	diffColor := base
	for i := 0; i < 4; i++ {
		diffColor.ColorHist[i] = 0
		diffColor.ColorHist[12+i] = 0.25
	}
	if drifted, why := styleVerdict(base, diffColor, th); !drifted {
		t.Fatalf("色调大改应漂移：%q", why)
	}
	// 分辨率减半
	small := base
	small.W, small.H = 640, 360
	if drifted, why := styleVerdict(base, small, th); !drifted {
		t.Fatalf("分辨率大改应漂移：%q", why)
	}
}

// TestSessionKey 场标识=会话目录名
func TestSessionKey(t *testing.T) {
	if got := sessionKey(nil); got != "" {
		t.Fatal("nil 会话应为空")
	}
	if got := sessionKey(&liveSession{segDir: filepath.Join("data", "live", "r1_20261025")}); got != "r1_20261025" {
		t.Fatalf("场标识应为目录名，得到 %q", got)
	}
	if got := sessionKey(&liveSession{segDir: filepath.Join("x", "s2") + string(os.PathSeparator)}); got != "s2" {
		t.Fatalf("尾分隔符应容忍，得到 %q", got)
	}
}

// TestStyleCheckRealFFmpeg 场级基线/漂移全链路（有 ffmpeg 才真跑）：
// 同风格两段 → 建基线+比对正常；风格突变段 → 漂移标记 + 切片侧可读到
func TestStyleCheckRealFFmpeg(t *testing.T) {
	bin, ok := ffmpegBinLocal()
	if !ok {
		t.Skip("本机无 ffmpeg，跳过风格基线真链路测试")
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Engines.FFmpeg = bin
	cfg.PublishGate.Enabled = true
	d := &Daemon{cfg: cfg, store: st} // notify=nil 安全（Enabled 检查短路）

	mkSeg := func(name, filter, size string) string {
		p := filepath.Join(dir, name)
		if out, err := runFF(bin, "-y", "-f", "lavfi", "-i", "testsrc2=duration=4:rate=10", "-vf", filter, "-s", size, p); err != nil {
			t.Fatalf("造段失败: %v：%.120s", err, out)
		}
		return p
	}
	segDir := filepath.Join(dir, "sess_1010")
	os.MkdirAll(segDir, 0o755)
	s := &liveSession{roomID: "r1", segDir: segDir, title: "测试直播"}

	// 段 1（首段）：建基线
	seg1 := mkSeg("seg_1.flv", "hue=h=0", "1280x720")
	d.styleCheck(context.Background(), s, seg1, 0)
	_, _, ver, drifted, ok := st.StyleBaseline("r1")
	if !ok || ver != 1 || drifted {
		t.Fatalf("首段应建基线 v1 无漂移，得到 ok=%v ver=%d drifted=%v", ok, ver, drifted)
	}
	// 段 2（同风格）：比对通过，不漂移
	seg2 := mkSeg("seg_2.flv", "hue=h=0", "1280x720")
	d.styleCheck(context.Background(), s, seg2, 1)
	if _, _, _, drifted, _ := st.StyleBaseline("r1"); drifted {
		t.Fatal("同风格段不应漂移")
	}
	// 段 3（风格突变：色相大改+内容换台）：漂移标记 + 切片侧可读
	seg3 := mkSeg("seg_3.flv", "hue=h=180", "1280x720")
	d.styleCheck(context.Background(), s, seg3, 2)
	if _, _, _, drifted, ok := st.StyleBaseline("r1"); !ok || !drifted {
		t.Fatal("风格突变段应标记漂移（允许误报，尽量不漏报）")
	}
	// 切片侧接口（gate.BaselineReader）：drifted=true → 整场拦截转人工
	if d, v, ok := st.RoomStyleState("r1"); !ok || !d || v != 1 {
		t.Fatalf("体检门关卡2应读到漂移，得到 drifted=%v ver=%d ok=%v", d, v, ok)
	}
}

func ffmpegBinLocal() (string, bool) {
	for _, p := range []string{"ffmpeg", "ffmpeg.exe",
		filepath.Join("engines", "ffmpeg.exe"), filepath.Join("temp_server", "engines", "ffmpeg.exe")} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

func runFF(bin string, args ...string) (string, error) {
	out, err := wexec.Command(context.Background(), bin, args...).CombinedOutput()
	return string(out), err
}
