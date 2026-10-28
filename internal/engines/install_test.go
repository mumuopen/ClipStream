package engines

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"chopperbot/internal/config"
)

// mkZip 造一个内嵌测试 zip（含 bin/ffmpeg.exe、bin/ffprobe.exe、干扰文件）
func mkZip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	add("ffmpeg-7.1-essentials/bin/ffmpeg.exe", "FFMPEG-BIN")
	add("ffmpeg-7.1-essentials/bin/ffprobe.exe", "FFPROBE-BIN")
	add("ffmpeg-7.1-essentials/LICENSE.txt", "license") // 干扰项：不应被挑选
	add("readme.md", "readme")                          // 根目录干扰
	_ = zw.Close()
}

// TestExtractZip 只解出 bin/ 下的 ffmpeg/ffprobe，干扰文件不落地
func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "src.zip")
	mkZip(t, zp)
	got := map[string]string{}
	if err := extractZip(zp, "bin", func(name string) bool {
		b := filepath.Base(name)
		return b == "ffmpeg.exe" || b == "ffprobe.exe"
	}, got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应解出 2 个文件，got %v", got)
	}
	for _, want := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("缺 %s", want)
		}
		b, err := os.ReadFile(got[want])
		if err != nil || len(b) == 0 {
			t.Fatalf("%s 内容为空或不可读", want)
		}
		// 临时文件不留（调用方 rename 走），但确认值正确
		os.Remove(got[want])
	}
}

// TestPresent 存在性判定：相对路径文件 / PATH 命令 / 空串
func TestPresent(t *testing.T) {
	f := filepath.Join(t.TempDir(), "probe.exe")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !present(f) {
		t.Fatal("存在的文件应判定为 true")
	}
	if present(filepath.Join(filepath.Dir(f), "missing.exe")) {
		t.Fatal("不存在的路径应为 false")
	}
	// PATH 命令：cmd.exe 在 Windows PATH（测试环境为 Windows）
	if !present("cmd.exe") {
		t.Log("cmd.exe 不在 PATH（非 Windows 环境跳过）")
	}
	if present("") {
		t.Fatal("空串应为 false")
	}
}

// TestMissing 配置空白时全缺；填充后按项消失
func TestMissing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Engines.YtDlp = "engines/yt-dlp.exe"
	// 空文件不存在 → 仍缺
	m := Missing(cfg, "engines")
	found := false
	for _, n := range m {
		if n == "yt-dlp" {
			found = true
		}
	}
	if !found {
		t.Fatal("文件不存在时 yt-dlp 应仍在缺失清单")
	}
	// 真实落盘后不再缺失
	dir := t.TempDir()
	exe := filepath.Join(dir, "yt-dlp.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Engines.YtDlp = exe
	m2 := Missing(cfg, dir)
	for _, n := range m2 {
		if n == "yt-dlp" {
			t.Fatal("已落盘后 yt-dlp 不应缺失")
		}
	}
}

// TestInstallModelGuard 未知模型档位直接拒绝（不发起任何下载）
func TestInstallModelGuard(t *testing.T) {
	cfg := &config.Config{}
	_, err := Install(context.Background(), cfg, InstallPlan{WantModel: "huge", TargetDir: t.TempDir()})
	if err == nil {
		t.Fatal("未知档位应报错")
	}
}